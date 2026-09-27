package allocation

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/fleet"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

func TestReleaseKeepsSiblingLeaseOccupied(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.json")
	clock := platform.NewManualClock(time.Now().UTC().Truncate(time.Second))
	ids := platform.CryptoIDGenerator{}

	stateStore := openStore(t, path)
	seedActivePlanAndNodes(t, stateStore, clock.Now())
	service := New(stateStore, clock, ids)
	nodes := fleet.New(stateStore, clock, ids)

	group := allocate(t, ctx, service, "request-0001", 2)
	if len(group.LeaseIDs) != 2 {
		t.Fatalf("expected two leases in the batch, got %d", len(group.LeaseIDs))
	}
	leaseNodes := leaseNodeMap(t, ctx, stateStore, group.LeaseIDs)
	releasedLease := group.LeaseIDs[0]
	siblingLease := group.LeaseIDs[1]
	releasedNode := leaseNodes[releasedLease]
	siblingNode := leaseNodes[siblingLease]

	released, err := service.Release(ctx, releasedLease)
	if err != nil {
		t.Fatalf("release lease: %v", err)
	}
	if released.State != domain.LeaseReleased {
		t.Fatalf("released lease must be released, got %q", released.State)
	}

	assertNodeState(t, ctx, stateStore, releasedNode, domain.NodeReady)
	assertNodeState(t, ctx, stateStore, siblingNode, domain.NodeLeased)
	detail, err := nodes.Get(ctx, siblingNode)
	if err != nil {
		t.Fatalf("read sibling node: %v", err)
	}
	if detail.State != domain.NodeLeased {
		t.Fatalf("detail read must report the sibling node as leased, got %q", detail.State)
	}

	// The next allocation may reuse the released node immediately, but it
	// must not hand out the node still bound to the claimed sibling lease.
	second := allocate(t, ctx, service, "request-0002", 2)
	if len(second.LeaseIDs) != 1 {
		t.Fatalf("only the released node may be reallocated, got %d leases", len(second.LeaseIDs))
	}
	secondNodes := leaseNodeMap(t, ctx, stateStore, second.LeaseIDs)
	if got := secondNodes[second.LeaseIDs[0]]; got != releasedNode {
		t.Fatalf("reallocation must use the released node %q, got %q", releasedNode, got)
	}
	assertNodeState(t, ctx, stateStore, siblingNode, domain.NodeLeased)

	// A restart must recover the same occupancy facts: both still-claimed
	// leases keep their nodes, and the ended lease stays ended.
	reopened := openStore(t, path)
	assertNodeState(t, ctx, reopened, siblingNode, domain.NodeLeased)
	assertNodeState(t, ctx, reopened, releasedNode, domain.NodeLeased)
	var reloadedLease domain.NodeLease
	readState(t, ctx, reopened, func(state *domain.State) {
		reloadedLease = state.Leases[releasedLease]
	})
	if reloadedLease.State != domain.LeaseReleased {
		t.Fatalf("ended lease must not be reactivated by recovery, got %q", reloadedLease.State)
	}

	// Once the remaining leases expire, every node is immediately reusable.
	clock.Advance(2 * time.Hour)
	recovered := New(reopened, clock, ids)
	expired, err := recovered.ExpireDue(ctx)
	if err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	if expired != 2 {
		t.Fatalf("expected the two still-claimed leases to expire, got %d", expired)
	}
	assertNodeState(t, ctx, reopened, siblingNode, domain.NodeReady)
	third := allocate(t, ctx, recovered, "request-0003", 2)
	if len(third.LeaseIDs) != 2 {
		t.Fatalf("expired nodes must be reusable, got %d leases", len(third.LeaseIDs))
	}
	readState(t, ctx, reopened, func(state *domain.State) {
		if state.Leases[releasedLease].State != domain.LeaseReleased {
			t.Fatalf("released lease must stay released, got %q", state.Leases[releasedLease].State)
		}
	})
}

func TestAllocateDoesNotDoubleClaimNodeWithActiveLease(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.json")
	clock := platform.NewManualClock(time.Now().UTC().Truncate(time.Second))
	stateStore := openStore(t, path)
	now := clock.Now()

	// Seed a legacy-inconsistent state: the node looks ready even though a
	// claimed, unexpired lease is still bound to it.
	err := stateStore.Write(ctx, func(state *domain.State) error {
		state.Plans["plan-1"] = activePlan(now)
		state.Nodes["node-east-1"] = readyNode("node-east-1", now)
		state.Leases["lease-1"] = domain.NodeLease{
			ID:        "lease-1",
			PlanID:    "plan-1",
			NodeID:    "node-east-1",
			State:     domain.LeaseClaimed,
			ClaimedAt: now,
			ExpiresAt: now.Add(time.Hour),
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}

	service := New(stateStore, clock, platform.CryptoIDGenerator{})
	_, err = service.Allocate(ctx, "plan-1", domain.LeaseInput{
		RequestKey:   "request-0001",
		Region:       "east",
		LeaseSeconds: 3600,
		MaxNodes:     1,
	})
	if domain.ErrorCodeOf(err) != domain.CodeNodeUnavailable {
		t.Fatalf("occupied node must not be handed out again, got %v", err)
	}
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	stateStore, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return stateStore
}

func activePlan(now time.Time) domain.RolloutPlan {
	return domain.RolloutPlan{
		ID:                  "plan-1",
		Zone:                "lease.example",
		State:               domain.PlanActive,
		MinimumSuccessRatio: 0.5,
		MaximumLatencyMS:    100,
		ExpectedNodes:       2,
		Version:             1,
		CreatedAt:           now,
	}
}

func readyNode(id string, now time.Time) domain.ResolverNode {
	return domain.ResolverNode{
		ID:        id,
		NodeName:  id + ".example",
		Region:    "east",
		State:     domain.NodeReady,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func seedActivePlanAndNodes(t *testing.T, stateStore *store.Store, now time.Time) {
	t.Helper()
	err := stateStore.Write(context.Background(), func(state *domain.State) error {
		state.Plans["plan-1"] = activePlan(now)
		state.Nodes["node-east-1"] = readyNode("node-east-1", now)
		state.Nodes["node-east-2"] = readyNode("node-east-2", now)
		return nil
	})
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func allocate(t *testing.T, ctx context.Context, service *Service, requestKey string, maxNodes int) domain.LeaseGroup {
	t.Helper()
	group, err := service.Allocate(ctx, "plan-1", domain.LeaseInput{
		RequestKey:   requestKey,
		Region:       "east",
		LeaseSeconds: 3600,
		MaxNodes:     maxNodes,
	})
	if err != nil {
		t.Fatalf("allocate with %s: %v", requestKey, err)
	}
	return group
}

func leaseNodeMap(t *testing.T, ctx context.Context, stateStore *store.Store, leaseIDs []string) map[string]string {
	t.Helper()
	result := make(map[string]string, len(leaseIDs))
	readState(t, ctx, stateStore, func(state *domain.State) {
		for _, leaseID := range leaseIDs {
			lease, ok := state.Leases[leaseID]
			if !ok {
				t.Fatalf("lease %q is missing", leaseID)
			}
			result[leaseID] = lease.NodeID
		}
	})
	return result
}

func assertNodeState(t *testing.T, ctx context.Context, stateStore *store.Store, nodeID string, want domain.NodeState) {
	t.Helper()
	readState(t, ctx, stateStore, func(state *domain.State) {
		node, ok := state.Nodes[nodeID]
		if !ok {
			t.Fatalf("node %q is missing", nodeID)
		}
		if node.State != want {
			t.Fatalf("node %q must be %q, got %q", nodeID, want, node.State)
		}
	})
}

func readState(t *testing.T, ctx context.Context, stateStore *store.Store, fn func(*domain.State)) {
	t.Helper()
	if err := stateStore.Read(ctx, func(state *domain.State) error {
		fn(state)
		return nil
	}); err != nil {
		t.Fatalf("read state: %v", err)
	}
}
