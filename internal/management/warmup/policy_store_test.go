package warmup

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// memoryPolicyStore stands in for the warmup_policies table.
type memoryPolicyStore struct {
	mu       sync.Mutex
	rows     map[string]StoredPolicy
	failList bool
}

func newMemoryPolicyStore() *memoryPolicyStore {
	return &memoryPolicyStore{rows: make(map[string]StoredPolicy)}
}

func (m *memoryPolicyStore) ListPolicies(_ context.Context, tenantID string) ([]StoredPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failList {
		return nil, errors.New("database unavailable")
	}
	var out []StoredPolicy
	for _, sp := range m.rows {
		if tenantID == "" || sp.Policy.TenantID == tenantID {
			out = append(out, sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Policy.ID < out[j].Policy.ID })
	return out, nil
}

func (m *memoryPolicyStore) SavePolicy(_ context.Context, p Policy) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := policyKey(p.TenantID, p.ID)
	rev := m.rows[key].Revision + 1
	m.rows[key] = StoredPolicy{Policy: p, Revision: rev}
	return rev, nil
}

func (m *memoryPolicyStore) SaveRunState(_ context.Context, p Policy, revision int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := policyKey(p.TenantID, p.ID)
	if current, ok := m.rows[key]; !ok || current.Revision != revision {
		return false, nil
	}
	m.rows[key] = StoredPolicy{Policy: p, Revision: revision}
	return true, nil
}

func (m *memoryPolicyStore) get(t *testing.T, tenantID, id string) StoredPolicy {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	sp, ok := m.rows[policyKey(tenantID, id)]
	if !ok {
		t.Fatalf("policy %s/%s not stored", tenantID, id)
	}
	return sp
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func schedulerNode(store PolicyStore, coord *cluster.Coordinator, clock *fakeClock) *PolicyScheduler {
	s := NewPolicyScheduler(nil, NewDriverRegistry(), nil)
	s.SetStore(store)
	s.SetLeaderCheck(coord.IsLeader)
	s.SetNowFunc(clock.Now)
	return s
}

// A policy saved on any node runs exactly once per interval, on the leader,
// and keeps running from its stored bookkeeping after leadership moves.
func TestWarmupPoliciesRunOnLeaderOnly(t *testing.T) {
	hub := cluster.NewMemoryHub()
	coordA, coordB := hub.Join("node-a"), hub.Join("node-b")
	store := newMemoryPolicyStore()
	clock := &fakeClock{now: time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)}
	nodeA := schedulerNode(store, coordA, clock)
	nodeB := schedulerNode(store, coordB, clock)
	ctx := context.Background()

	if err := nodeB.SavePolicy(ctx, Policy{ID: "p1", TenantID: "t1", Enabled: true, IntervalSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	listed, err := nodeA.ListPolicies(ctx, "t1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("policy saved on node B must be visible on node A: %+v err=%v", listed, err)
	}

	nodeB.EvaluateTick(ctx)
	if runs := store.get(t, "t1", "p1").Policy.TotalRuns; runs != 0 {
		t.Fatalf("a follower must not run policies, runs=%d", runs)
	}
	nodeA.EvaluateTick(ctx)
	nodeA.EvaluateTick(ctx)
	if runs := store.get(t, "t1", "p1").Policy.TotalRuns; runs != 1 {
		t.Fatalf("the leader must run the policy once per interval, runs=%d", runs)
	}

	hub.SetLeader("node-b")
	clock.Advance(30 * time.Minute)
	nodeB.EvaluateTick(ctx)
	if runs := store.get(t, "t1", "p1").Policy.TotalRuns; runs != 1 {
		t.Fatalf("the new leader must honour the stored next run, runs=%d", runs)
	}
	clock.Advance(31 * time.Minute)
	nodeA.EvaluateTick(ctx)
	nodeB.EvaluateTick(ctx)
	if runs := store.get(t, "t1", "p1").Policy.TotalRuns; runs != 2 {
		t.Fatalf("only the new leader runs the due policy, runs=%d", runs)
	}
}

// An operator edit made while the leader evaluates wins over the leader's
// bookkeeping write-back.
func TestWarmupRunStateDoesNotOverwriteConcurrentEdit(t *testing.T) {
	store := newMemoryPolicyStore()
	clock := &fakeClock{now: time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)}
	hub := cluster.NewMemoryHub()
	leader := schedulerNode(store, hub.Join("node-a"), clock)
	ctx := context.Background()
	if err := leader.SavePolicy(ctx, Policy{ID: "p1", TenantID: "t1", Name: "before", Enabled: true, IntervalSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if !leader.reload(ctx) {
		t.Fatal("reload failed")
	}
	// The edit lands between the leader's read and its write-back.
	if _, err := store.SavePolicy(ctx, Policy{ID: "p1", TenantID: "t1", Name: "edited", Enabled: true, IntervalSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	leader.mu.Lock()
	before := leader.runStatesLocked()
	leader.evaluateLocked(ctx, clock.Now())
	changed := leader.changedLocked(before)
	leader.mu.Unlock()
	leader.persistRunStates(ctx, changed)

	if got := store.get(t, "t1", "p1"); got.Policy.Name != "edited" || got.Revision != 2 {
		t.Fatalf("edit was overwritten: %+v", got)
	}
}

// A store that cannot be read makes the leader skip the tick rather than run
// from a stale copy.
func TestWarmupSkipsTickWhenStoreUnavailable(t *testing.T) {
	store := newMemoryPolicyStore()
	clock := &fakeClock{now: time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)}
	hub := cluster.NewMemoryHub()
	leader := schedulerNode(store, hub.Join("node-a"), clock)
	ctx := context.Background()
	if err := leader.SavePolicy(ctx, Policy{ID: "p1", TenantID: "t1", Enabled: true, IntervalSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	store.failList = true
	leader.EvaluateTick(ctx)
	store.failList = false
	if runs := store.get(t, "t1", "p1").Policy.TotalRuns; runs != 0 {
		t.Fatalf("tick must be skipped when policies cannot be loaded, runs=%d", runs)
	}
}

// Two tenants may use the same policy id without replacing each other.
func TestWarmupPolicyIDsAreTenantScoped(t *testing.T) {
	s := NewPolicyScheduler(nil, NewDriverRegistry(), nil)
	ctx := context.Background()
	_ = s.SavePolicy(ctx, Policy{ID: "shared-id", TenantID: "t1", Name: "one"})
	_ = s.SavePolicy(ctx, Policy{ID: "shared-id", TenantID: "t2", Name: "two"})
	if got := s.GetPolicies("t1"); len(got) != 1 || got[0].Name != "one" {
		t.Fatalf("tenant t1 policies = %+v", got)
	}
	if got := s.GetPolicies(""); len(got) != 2 {
		t.Fatalf("all policies = %+v", got)
	}
}
