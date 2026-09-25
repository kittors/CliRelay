package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSlotCluster is the cluster-wide slot count shared by several limiters,
// each standing for one process.
type fakeSlotCluster struct {
	mu         sync.Mutex
	held       map[string]int
	down       atomic.Bool
	failAcq    atomic.Bool
	nodes      int
	acquires   atomic.Int64
	releases   atomic.Int64
	releaseLog []string
}

func newFakeSlotCluster(nodes int) *fakeSlotCluster {
	return &fakeSlotCluster{held: map[string]int{}, nodes: nodes}
}

func (f *fakeSlotCluster) Shared() bool { return !f.down.Load() }

func (f *fakeSlotCluster) NodeShare(limit int) int {
	if f.nodes <= 1 {
		return limit
	}
	return (limit + f.nodes - 1) / f.nodes
}

func (f *fakeSlotCluster) Acquire(authID string, limit int) (bool, error) {
	f.acquires.Add(1)
	if f.failAcq.Load() {
		return false, errors.New("cluster count unreachable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[authID] >= limit {
		return false, nil
	}
	f.held[authID]++
	return true, nil
}

func (f *fakeSlotCluster) Release(authID string) {
	f.releases.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseLog = append(f.releaseLog, authID)
	if f.held[authID] > 0 {
		f.held[authID]--
	}
}

func (f *fakeSlotCluster) heldOn(authID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[authID]
}

func clusterLimiter(c AccountSlotCoordinator) *AccountConcurrencyLimiter {
	l := NewAccountConcurrencyLimiter()
	l.SetCoordinator(c)
	return l
}

func TestClusterAccountSlotsAreSharedBetweenProcesses(t *testing.T) {
	cluster := newFakeSlotCluster(2)
	nodeA, nodeB := clusterLimiter(cluster), clusterLimiter(cluster)
	auth := limitedAuth("acct-1", 2)

	releaseA1, err := nodeA.AcquireSlot(auth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeB.AcquireSlot(auth); err != nil {
		t.Fatal(err)
	}
	_, err = nodeA.AcquireSlot(auth)
	var saturated *AccountConcurrencyError
	if !errors.As(err, &saturated) || saturated.Limit != 2 {
		t.Fatalf("third slot cluster-wide must be refused with the full limit, got %v", err)
	}
	if nodeA.GetInFlight("acct-1") != 1 {
		t.Fatalf("a refused reservation must be undone, in-flight = %d", nodeA.GetInFlight("acct-1"))
	}

	releaseA1()
	releaseA1() // idempotent
	if cluster.heldOn("acct-1") != 1 || cluster.releases.Load() != 1 {
		t.Fatalf("release must give back exactly one cluster slot, held=%d releases=%d", cluster.heldOn("acct-1"), cluster.releases.Load())
	}
	if _, err := nodeA.AcquireSlot(auth); err != nil {
		t.Fatalf("slot freed cluster-wide must be usable: %v", err)
	}
}

func TestClusterAccountHandoffKeepsTheClusterSlot(t *testing.T) {
	cluster := newFakeSlotCluster(2)
	node := clusterLimiter(cluster)
	auth := limitedAuth("acct-1", 1)

	release, err := node.AcquireSlot(auth)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		release func()
		err     error
	}
	queued := make(chan result, 1)
	go func() {
		r, _, err := node.AcquireSlotWait(context.Background(), []*Auth{auth}, 5*time.Second, 0)
		queued <- result{r, err}
	}()
	waitForQueue(t, node, "acct-1", 1)

	release()
	got := <-queued
	if got.err != nil {
		t.Fatalf("queued caller: %v", got.err)
	}
	if cluster.releases.Load() != 0 || cluster.heldOn("acct-1") != 1 || cluster.acquires.Load() != 1 {
		t.Fatalf("a handoff must not touch the cluster count: acquires=%d releases=%d held=%d",
			cluster.acquires.Load(), cluster.releases.Load(), cluster.heldOn("acct-1"))
	}
	got.release()
	if cluster.releases.Load() != 1 || cluster.heldOn("acct-1") != 0 {
		t.Fatalf("the final release gives the cluster slot back: releases=%d held=%d", cluster.releases.Load(), cluster.heldOn("acct-1"))
	}
}

// TestClusterAccountWaiterGetsSlotFreedElsewhere covers the case the local
// FIFO cannot: the slot this request waits for is held by another process,
// whose release produces no local event.
func TestClusterAccountWaiterGetsSlotFreedElsewhere(t *testing.T) {
	cluster := newFakeSlotCluster(2)
	nodeA, nodeB := clusterLimiter(cluster), clusterLimiter(cluster)
	auth := limitedAuth("acct-1", 1)

	releaseB, err := nodeB.AcquireSlot(auth)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var releaseA func()
	go func() {
		r, id, err := nodeA.AcquireSlotWait(context.Background(), []*Auth{auth}, 5*time.Second, 0)
		if err == nil && id != "acct-1" {
			err = errors.New("unexpected auth " + id)
		}
		releaseA = r
		done <- err
	}()
	waitForQueue(t, nodeA, "acct-1", 1)
	time.Sleep(50 * time.Millisecond)
	releaseB()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never picked up the slot released on another process")
	}
	if nodeA.QueueDepth("acct-1") != 0 {
		t.Fatal("waiter must leave the queue")
	}
	releaseA()
	if cluster.heldOn("acct-1") != 0 {
		t.Fatalf("held = %d", cluster.heldOn("acct-1"))
	}
}

func TestClusterAccountFallsBackToNodeShare(t *testing.T) {
	cluster := newFakeSlotCluster(2)
	cluster.down.Store(true)
	node := clusterLimiter(cluster)
	auth := limitedAuth("acct-1", 4)

	var releases []func()
	for i := 0; i < 2; i++ {
		r, err := node.AcquireSlot(auth)
		if err != nil {
			t.Fatalf("slot %d within the node share: %v", i+1, err)
		}
		releases = append(releases, r)
	}
	_, err := node.AcquireSlot(auth)
	var saturated *AccountConcurrencyError
	if !errors.As(err, &saturated) || saturated.Limit != 2 {
		t.Fatalf("share of 4 across 2 processes is 2, got %v", err)
	}
	if cluster.acquires.Load() != 0 {
		t.Fatal("an unavailable cluster count must not be called")
	}
	if !node.HasAvailableSlot(limitedAuth("acct-2", 4)) || node.HasAvailableSlot(auth) {
		t.Fatal("availability must follow the node share")
	}
	if node.unsharedSlots("acct-1") != 2 {
		t.Fatalf("fallback slots must be marked, got %d", node.unsharedSlots("acct-1"))
	}

	// The cluster comes back while those two are still running. Their release
	// must not give back cluster slots they never held.
	cluster.down.Store(false)
	r, err := node.AcquireSlot(auth)
	if err != nil || cluster.heldOn("acct-1") != 1 {
		t.Fatalf("shared again: %v held=%d", err, cluster.heldOn("acct-1"))
	}
	for _, release := range releases {
		release()
	}
	if cluster.releases.Load() != 0 || cluster.heldOn("acct-1") != 1 {
		t.Fatalf("fallback slots released cluster slots: releases=%d held=%d", cluster.releases.Load(), cluster.heldOn("acct-1"))
	}
	r()
	if cluster.heldOn("acct-1") != 0 {
		t.Fatalf("shared slot not given back, held=%d", cluster.heldOn("acct-1"))
	}
}

func TestClusterAccountAcquireErrorUsesNodeShareForThatRequest(t *testing.T) {
	cluster := newFakeSlotCluster(3)
	cluster.failAcq.Store(true)
	node := clusterLimiter(cluster)
	auth := limitedAuth("acct-1", 3)

	release, err := node.AcquireSlot(auth)
	if err != nil {
		t.Fatalf("share of 3 across 3 processes is 1: %v", err)
	}
	if _, err := node.AcquireSlot(auth); err == nil {
		t.Fatal("second slot exceeds the node share")
	}
	release()
	if cluster.releases.Load() != 0 {
		t.Fatal("a slot that never held a cluster slot must not release one")
	}
}

func TestClusterAccountUnlimitedAuthSkipsCoordinator(t *testing.T) {
	cluster := newFakeSlotCluster(2)
	node := clusterLimiter(cluster)
	release, err := node.AcquireSlot(&Auth{ID: "free"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if cluster.acquires.Load() != 0 || cluster.releases.Load() != 0 {
		t.Fatal("accounts without a limit must never reach the coordinator")
	}
}

func TestManagerSetAccountSlotCoordinator(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cluster := newFakeSlotCluster(2)
	m.SetAccountSlotCoordinator(cluster)
	auth := limitedAuth("acct-1", 1)
	release, err := m.acquireAccountSlot(auth)
	if err != nil || cluster.heldOn("acct-1") != 1 {
		t.Fatalf("manager must route through the coordinator: %v held=%d", err, cluster.heldOn("acct-1"))
	}
	release()
	m.SetAccountSlotCoordinator(nil)
	if m.ConcurrencyLimiter().coord() != nil {
		t.Fatal("nil must restore per-process limits")
	}
}

func waitForQueue(t *testing.T, l *AccountConcurrencyLimiter, authID string, depth int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for l.QueueDepth(authID) != depth {
		if time.Now().After(deadline) {
			t.Fatalf("queue depth for %s never reached %d", authID, depth)
		}
		time.Sleep(time.Millisecond)
	}
}
