package session

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// twoNodes wires two ClusterStores over one repository, the way two cluster
// nodes share the database. Polling is effectively disabled so a test only
// passes when the bus event (or a resync) wakes the waiter.
func twoNodes(t *testing.T) (*cluster.MemoryHub, *MemoryRepo, *ClusterStore, *ClusterStore) {
	t.Helper()
	hub := cluster.NewMemoryHub()
	nodeA, nodeB := hub.Join("node-a"), hub.Join("node-b")
	repo := NewMemoryRepo()
	storeA := NewClusterStore(repo, func() *cluster.Coordinator { return nodeA }, time.Minute)
	storeB := NewClusterStore(repo, func() *cluster.Coordinator { return nodeB }, time.Minute)
	for _, s := range []*ClusterStore{storeA, storeB} {
		s.SetPollInterval(time.Hour)
		t.Cleanup(s.Close)
	}
	return hub, repo, storeA, storeB
}

type waitResult struct {
	payload map[string]string
	err     error
}

func waitAsync(store *ClusterStore, provider, state string, timeout time.Duration) <-chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		payload, err := store.WaitCallback("", provider, state, timeout, 0)
		out <- waitResult{payload: payload, err: err}
	}()
	return out
}

func receive(t *testing.T, ch <-chan waitResult) waitResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not return")
		return waitResult{}
	}
}

// waitForWaiter blocks until store has a waiter for state, so a delivery in
// the test cannot race ahead of the subscription it is meant to exercise.
func waitForWaiter(t *testing.T, store *ClusterStore, state string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		_, ok := store.waiters[state]
		store.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no waiter registered for %s", state)
}

func TestClusterStoreCallbackOnOtherNodeWakesOwner(t *testing.T) {
	_, _, storeA, storeB := twoNodes(t)

	storeA.RegisterTenant("state-1", "codex", "tenant-1")
	result := waitAsync(storeA, "codex", "state-1", 5*time.Second)
	waitForWaiter(t, storeA, "state-1")

	if session, ok := storeB.Get("state-1"); !ok || session.Status != "" || session.TenantID != "tenant-1" {
		t.Fatalf("node B must see the pending session: %+v ok=%v", session, ok)
	}
	if err := storeB.DeliverCallback("", "codex", "state-1", " the-code ", ""); err != nil {
		t.Fatalf("deliver on node B: %v", err)
	}
	res := receive(t, result)
	if res.err != nil || res.payload["code"] != "the-code" || res.payload["state"] != "state-1" {
		t.Fatalf("owner wait = %+v", res)
	}

	storeA.Complete("state-1")
	session, ok := storeB.Get("state-1")
	if !ok || session.Status != StatusCompleted {
		t.Fatalf("node B must report completion: %+v ok=%v", session, ok)
	}
	lookup, err := storeB.Lookup("state-1")
	if err != nil || lookup.Outcome != LookupFound || lookup.Session.Status != StatusCompleted {
		t.Fatalf("lookup = %+v err=%v", lookup, err)
	}
	if err := storeB.DeliverCallback("", "codex", "state-1", "again", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a finished session must refuse callbacks, got %v", err)
	}
}

func TestClusterStoreResyncRecoversLostCallbackEvent(t *testing.T) {
	hub, _, storeA, storeB := twoNodes(t)

	storeA.RegisterTenant("state-resync", "anthropic", "")
	result := waitAsync(storeA, "anthropic", "state-resync", 5*time.Second)
	waitForWaiter(t, storeA, "state-resync")

	hub.Disconnect("node-a")
	if err := storeB.DeliverCallback("", "claude", "state-resync", "code-1", ""); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	select {
	case res := <-result:
		t.Fatalf("waiter must not wake while its node is cut off: %+v", res)
	case <-time.After(50 * time.Millisecond):
	}
	hub.Reconnect("node-a")
	if res := receive(t, result); res.err != nil || res.payload["code"] != "code-1" {
		t.Fatalf("resync must wake the waiter: %+v", res)
	}
}

func TestClusterStoreSupersededLoginsStopWaiting(t *testing.T) {
	_, _, storeA, storeB := twoNodes(t)

	storeB.RegisterTenant("old-login", "codex", "tenant-1")
	storeA.RegisterTenant("new-login", "codex", "tenant-1")
	storeA.RegisterTenant("other-tenant", "codex", "tenant-2")
	result := waitAsync(storeB, "codex", "old-login", 5*time.Second)
	waitForWaiter(t, storeB, "old-login")

	storeA.Complete("new-login")
	if removed := storeA.CompleteProviderTenant("codex", "tenant-1"); removed != 1 {
		t.Fatalf("cancelled = %d, want 1", removed)
	}
	if res := receive(t, result); !errors.Is(res.err, ErrNotPending) {
		t.Fatalf("superseded waiter must stop with ErrNotPending, got %+v", res)
	}
	if _, ok := storeA.Get("old-login"); ok {
		t.Fatal("superseded session must be gone for memory-store callers")
	}
	lookup, err := storeA.Lookup("old-login")
	if err != nil || lookup.Outcome != LookupSuperseded || lookup.Session.TenantID != "tenant-1" {
		t.Fatalf("lookup = %+v err=%v", lookup, err)
	}
	if !storeA.IsPending("other-tenant", "codex") {
		t.Fatal("another tenant's login must not be cancelled")
	}
}

func TestClusterStoreReportsLostOwner(t *testing.T) {
	_, repo, storeA, storeB := twoNodes(t)
	repo.SetOwnerLostAfter(20 * time.Millisecond)

	storeA.RegisterTenant("state-lost", "xai", "")
	// Node A dies: its heartbeat stops with it.
	storeA.Close()
	time.Sleep(40 * time.Millisecond)

	session, ok := storeB.Get("state-lost")
	if !ok || session.Status != MessageOwnerLost {
		t.Fatalf("session with a dead owner = %+v ok=%v", session, ok)
	}
	if err := storeB.DeliverCallback("", "xai", "state-lost", "code", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a callback for a dead owner must be refused, got %v", err)
	}
}

func TestClusterStoreHeartbeatKeepsOwnerAlive(t *testing.T) {
	_, repo, storeA, storeB := twoNodes(t)
	repo.SetOwnerLostAfter(60 * time.Millisecond)
	storeA.SetHeartbeatInterval(10 * time.Millisecond)

	storeA.RegisterTenant("state-alive", "gemini", "")
	time.Sleep(150 * time.Millisecond)
	if session, ok := storeB.Get("state-alive"); !ok || session.Status != "" {
		t.Fatalf("a heartbeating owner must stay pending: %+v ok=%v", session, ok)
	}
	storeA.SetError("state-alive", "Bad Request")
	if session, ok := storeB.Get("state-alive"); !ok || session.Status != "Bad Request" {
		t.Fatalf("failure must be visible: %+v ok=%v", session, ok)
	}
}

func TestClusterStoreExpiryAndUnknownStates(t *testing.T) {
	_, repo, storeA, storeB := twoNodes(t)
	var mu sync.Mutex
	now := time.Now()
	repo.SetClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})

	storeA.RegisterTenant("state-expiring", "iflow", "")
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()

	if _, ok := storeB.Get("state-expiring"); ok {
		t.Fatal("an expired session must be gone for memory-store callers")
	}
	lookup, err := storeB.Lookup("state-expiring")
	if err != nil || lookup.Outcome != LookupExpired {
		t.Fatalf("lookup = %+v err=%v", lookup, err)
	}
	if err := storeB.DeliverCallback("", "iflow", "state-expiring", "code", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("expired session must refuse callbacks, got %v", err)
	}
	if lookup, err := storeB.Lookup("never-registered"); err != nil || lookup.Outcome != LookupMissing {
		t.Fatalf("unknown lookup = %+v err=%v", lookup, err)
	}
	if _, err := storeB.WaitCallback("", "iflow", "state-expiring", time.Second, 0); !errors.Is(err, ErrNotPending) {
		t.Fatalf("waiting on an expired session must stop, got %v", err)
	}
}

func TestClusterStoreRejectsWrongProviderCallback(t *testing.T) {
	_, _, storeA, storeB := twoNodes(t)
	storeA.RegisterTenant("state-provider", "codex", "")
	if err := storeB.DeliverCallback("", "gemini", "state-provider", "code", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("callback for another provider must be refused, got %v", err)
	}
	if err := storeB.DeliverCallback("", "unknown", "state-provider", "code", ""); !errors.Is(err, ErrUnsupportedFlow) {
		t.Fatalf("unsupported provider error = %v", err)
	}
	if err := storeB.DeliverCallback("", "codex", "../escape", "code", ""); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid state error = %v", err)
	}
}

func TestClusterStoreWaitTimesOut(t *testing.T) {
	_, _, storeA, _ := twoNodes(t)
	storeA.SetPollInterval(5 * time.Millisecond)
	storeA.RegisterTenant("state-timeout", "codex", "")
	if _, err := storeA.WaitCallback("", "codex", "state-timeout", 30*time.Millisecond, 0); !errors.Is(err, ErrCallbackTimeout) {
		t.Fatalf("wait error = %v, want ErrCallbackTimeout", err)
	}
}
