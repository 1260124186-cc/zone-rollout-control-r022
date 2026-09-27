package domain

import (
	"testing"
	"time"
)

func TestReconcileLeaseOwnershipReleasesOnlyTheEndedLeaseNode(t *testing.T) {
	now := time.Date(2026, time.September, 27, 12, 0, 0, 0, time.UTC)
	finishedAt := now.Add(-time.Minute)
	state := &State{
		Nodes: map[string]ResolverNode{
			"node-a": {ID: "node-a", State: NodeLeased, Version: 2, UpdatedAt: now.Add(-time.Hour)},
			"node-b": {ID: "node-b", State: NodeLeased, Version: 2, UpdatedAt: now.Add(-time.Hour)},
		},
		Leases: map[string]NodeLease{
			"lease-a": {
				ID:           "lease-a",
				NodeID:       "node-a",
				AllocationID: "group-1",
				State:        LeaseReleased,
				ClaimedAt:    now.Add(-time.Hour),
				ExpiresAt:    now.Add(time.Hour),
				FinishedAt:   &finishedAt,
			},
			"lease-b": {
				ID:           "lease-b",
				NodeID:       "node-b",
				AllocationID: "group-1",
				State:        LeaseClaimed,
				ClaimedAt:    now.Add(-time.Hour),
				ExpiresAt:    now.Add(time.Hour),
			},
		},
	}

	if changed := ReconcileLeaseOwnership(state, now); changed != 1 {
		t.Fatalf("expected exactly one node to change, got %d", changed)
	}
	if got := state.Nodes["node-a"].State; got != NodeReady {
		t.Fatalf("released lease node must become ready, got %q", got)
	}
	nodeB := state.Nodes["node-b"]
	if nodeB.State != NodeLeased {
		t.Fatalf("sibling lease node must stay leased, got %q", nodeB.State)
	}
	if nodeB.Version != 2 {
		t.Fatalf("sibling node must not be touched, version changed to %d", nodeB.Version)
	}
	if state.Leases["lease-a"].State != LeaseReleased {
		t.Fatalf("reconciliation must not mutate lease facts, got %q", state.Leases["lease-a"].State)
	}
}

func TestReconcileLeaseOwnershipNeverReactivatesEndedLeases(t *testing.T) {
	now := time.Date(2026, time.September, 27, 12, 0, 0, 0, time.UTC)
	finishedAt := now.Add(-time.Minute)
	state := &State{
		Nodes: map[string]ResolverNode{
			"node-released": {ID: "node-released", State: NodeReady, Version: 3},
			"node-reported": {ID: "node-reported", State: NodeReady, Version: 3},
			"node-stale":    {ID: "node-stale", State: NodeReady, Version: 3},
		},
		Leases: map[string]NodeLease{
			"lease-released": {
				ID:         "lease-released",
				NodeID:     "node-released",
				State:      LeaseReleased,
				ExpiresAt:  now.Add(time.Hour),
				FinishedAt: &finishedAt,
			},
			"lease-reported": {
				ID:         "lease-reported",
				NodeID:     "node-reported",
				State:      LeaseReported,
				ExpiresAt:  now.Add(time.Hour),
				FinishedAt: &finishedAt,
			},
			"lease-stale": {
				ID:        "lease-stale",
				NodeID:    "node-stale",
				State:     LeaseClaimed,
				ExpiresAt: now.Add(-time.Second),
			},
		},
	}

	if changed := ReconcileLeaseOwnership(state, now); changed != 0 {
		t.Fatalf("no node may be reactivated for ended leases, got %d changes", changed)
	}
	for id, node := range state.Nodes {
		if node.State != NodeReady {
			t.Fatalf("node %s must stay ready, got %q", id, node.State)
		}
	}
}

func TestReconcileLeaseOwnershipRestoresOccupancyFromActiveLeases(t *testing.T) {
	now := time.Date(2026, time.September, 27, 12, 0, 0, 0, time.UTC)
	state := &State{
		Nodes: map[string]ResolverNode{
			"node-a": {ID: "node-a", State: NodeReady, Version: 4},
			"node-b": {ID: "node-b", State: NodeQuarantined, Version: 4},
		},
		Leases: map[string]NodeLease{
			"lease-a": {
				ID:        "lease-a",
				NodeID:    "node-a",
				State:     LeaseClaimed,
				ExpiresAt: now.Add(time.Hour),
			},
			"lease-b": {
				ID:        "lease-b",
				NodeID:    "node-b",
				State:     LeaseClaimed,
				ExpiresAt: now.Add(time.Hour),
			},
		},
	}

	if changed := ReconcileLeaseOwnership(state, now); changed != 1 {
		t.Fatalf("expected only the ready node to be aligned, got %d changes", changed)
	}
	if got := state.Nodes["node-a"].State; got != NodeLeased {
		t.Fatalf("node with an active lease must be leased, got %q", got)
	}
	if got := state.Nodes["node-b"].State; got != NodeQuarantined {
		t.Fatalf("quarantined node must not be flipped, got %q", got)
	}
}

func TestActiveLeaseOccupancy(t *testing.T) {
	now := time.Date(2026, time.September, 27, 12, 0, 0, 0, time.UTC)
	state := &State{
		Leases: map[string]NodeLease{
			"active":   {ID: "active", NodeID: "node-a", State: LeaseClaimed, ExpiresAt: now.Add(time.Hour)},
			"boundary": {ID: "boundary", NodeID: "node-b", State: LeaseClaimed, ExpiresAt: now},
			"released": {ID: "released", NodeID: "node-c", State: LeaseReleased, ExpiresAt: now.Add(time.Hour)},
		},
	}

	occupied := ActiveLeaseOccupancy(state, now)
	if !occupied["node-a"] {
		t.Fatal("claimed and unexpired lease must occupy its node")
	}
	if occupied["node-b"] {
		t.Fatal("lease expiring exactly at now must not occupy its node")
	}
	if occupied["node-c"] {
		t.Fatal("released lease must not occupy its node")
	}
	if got := len(ActiveLeaseOccupancy(nil, now)); got != 0 {
		t.Fatalf("nil state must report no occupancy, got %d entries", got)
	}
}
