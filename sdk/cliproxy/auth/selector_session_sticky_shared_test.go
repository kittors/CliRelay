package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAffinityStore mimics the shared Redis bindings: a map every process
// sees, with the lookup/bind/release semantics of the real scripts.
type fakeAffinityStore struct {
	mu       sync.Mutex
	bindings map[string]*SessionAffinityBinding
	down     atomic.Bool
	// raceWinner, when set, is bound by "another process" between this
	// process's lookup and its bind.
	raceWinner string
}

func newFakeAffinityStore() *fakeAffinityStore {
	return &fakeAffinityStore{bindings: map[string]*SessionAffinityBinding{}}
}

var errStoreDown = errors.New("shared store unreachable")

func (f *fakeAffinityStore) Lookup(_ context.Context, key string, _ time.Duration) (SessionAffinityBinding, error) {
	if f.down.Load() {
		return SessionAffinityBinding{}, errStoreDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.bindings[key]
	if b == nil {
		return SessionAffinityBinding{}, nil
	}
	out := *b
	b.Served++
	return out, nil
}

func (f *fakeAffinityStore) Bind(_ context.Context, key, accountRef string, _ time.Duration) (SessionAffinityBinding, error) {
	if f.down.Load() {
		return SessionAffinityBinding{}, errStoreDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.raceWinner != "" && f.bindings[key] == nil {
		f.bindings[key] = &SessionAffinityBinding{AccountRef: AffinityAccountRef(f.raceWinner), Served: 1, Found: true}
	}
	if b := f.bindings[key]; b != nil {
		out := *b
		b.Served++
		return out, nil
	}
	f.bindings[key] = &SessionAffinityBinding{AccountRef: accountRef, Served: 1, Found: true}
	return SessionAffinityBinding{AccountRef: accountRef, Found: true}, nil
}

func (f *fakeAffinityStore) Release(_ context.Context, key, accountRef string) error {
	if f.down.Load() {
		return errStoreDown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.bindings[key]; b != nil && b.AccountRef == accountRef {
		delete(f.bindings, key)
	}
	return nil
}

// boundTo reports which of the test auths key is bound to, by reference.
func (f *fakeAffinityStore) boundTo(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.bindings[key]
	if b == nil {
		return ""
	}
	for _, auth := range stickyAuths() {
		if AffinityAccountRef(auth.ID) == b.AccountRef {
			return auth.ID
		}
	}
	return "unknown:" + b.AccountRef
}

// stickyProcess is one process's sticky selector wired to the shared store.
func stickyProcess(store SessionAffinityStore) *SessionStickySelector {
	deps := &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: NewAccountConcurrencyLimiter()}
	if store != nil {
		deps.affinity.Store(&sessionAffinityRef{store: store})
	}
	selector := NewSessionStickySelector(&RoundRobinSelector{deps: deps})
	selector.deps = deps
	return selector
}

func stickyAuths() []*Auth {
	return []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}, {ID: "c", Provider: "codex"}}
}

func pickID(t *testing.T, s *SessionStickySelector, session string, extra map[string]any, auths []*Auth) string {
	t.Helper()
	picked, err := s.Pick(context.Background(), "codex", "gpt-5.5", stickyOptions(session, extra), auths)
	if err != nil {
		t.Fatalf("Pick(%s): %v", session, err)
	}
	return picked.ID
}

func TestSharedStickyBindingFollowsConversationAcrossProcesses(t *testing.T) {
	store := newFakeAffinityStore()
	nodeA, nodeB := stickyProcess(store), stickyProcess(store)
	auths := stickyAuths()

	// Advance node b's round-robin so a fresh pick there would differ.
	if got := pickID(t, nodeB, "warm-up", nil, auths); got != "a" {
		t.Fatalf("warm-up pick = %s", got)
	}
	first := pickID(t, nodeA, "sess-1", nil, auths)
	if first != "a" || store.boundTo(selectionKeyFor("sess-1")) != "a" {
		t.Fatalf("node a must bind in the shared store: picked %s, stored %q", first, store.boundTo(selectionKeyFor("sess-1")))
	}
	for i := 0; i < 3; i++ {
		if got := pickID(t, nodeB, "sess-1", nil, auths); got != first {
			t.Fatalf("turn %d on node b went to %s, want the conversation's account %s", i, got, first)
		}
	}
}

func TestSharedStickyMaxRequestsCountsAllProcesses(t *testing.T) {
	store := newFakeAffinityStore()
	nodeA, nodeB := stickyProcess(store), stickyProcess(store)
	auths := stickyAuths()
	limit := map[string]any{stickyMaxRequestsKey: "3"}
	// Advance node b's round-robin past "a", as a busy process's would be, so
	// the rebind is observable.
	pickID(t, nodeB, "warm-up", nil, auths)

	seen := []string{
		pickID(t, nodeA, "sess-1", limit, auths),
		pickID(t, nodeB, "sess-1", limit, auths),
		pickID(t, nodeA, "sess-1", limit, auths),
		pickID(t, nodeB, "sess-1", limit, auths),
	}
	if seen[0] != seen[1] || seen[1] != seen[2] {
		t.Fatalf("picks = %v: the first three requests share one binding across processes", seen)
	}
	if seen[3] == seen[0] {
		t.Fatalf("picks = %v: the fourth request, on either process, must be released", seen)
	}
	if store.boundTo(selectionKeyFor("sess-1")) != seen[3] {
		t.Fatalf("the new binding must be shared, stored %q", store.boundTo(selectionKeyFor("sess-1")))
	}
}

func TestSharedStickyReleasesWhenBoundAccountUnavailableHere(t *testing.T) {
	store := newFakeAffinityStore()
	nodeA, nodeB := stickyProcess(store), stickyProcess(store)
	auths := stickyAuths()
	first := pickID(t, nodeA, "sess-1", nil, auths)

	// On node b the bound account is not eligible (cooling down there).
	eligible := make([]*Auth, 0, 2)
	for _, a := range auths {
		if a.ID != first {
			eligible = append(eligible, a)
		}
	}
	moved := pickID(t, nodeB, "sess-1", nil, eligible)
	if moved == first || store.boundTo(selectionKeyFor("sess-1")) != moved {
		t.Fatalf("an ineligible binding must be released and replaced: picked %s, stored %q", moved, store.boundTo(selectionKeyFor("sess-1")))
	}
	if got := pickID(t, nodeA, "sess-1", nil, auths); got != moved {
		t.Fatalf("node a must follow the replacement binding, got %s want %s", got, moved)
	}
}

func TestSharedStickyJoinsBindingWonByAnotherProcess(t *testing.T) {
	store := newFakeAffinityStore()
	store.raceWinner = "c"
	node := stickyProcess(store)
	if got := pickID(t, node, "sess-1", nil, stickyAuths()); got != "c" {
		t.Fatalf("a concurrent bind by another process wins; picked %s, want c", got)
	}
	if got := pickID(t, node, "sess-1", nil, stickyAuths()); got != "c" {
		t.Fatalf("the local mirror must follow the winner, got %s", got)
	}
}

func TestSharedStickyFallsBackToLocalBindingsAndPublishesOnRecovery(t *testing.T) {
	store := newFakeAffinityStore()
	store.down.Store(true)
	nodeA, nodeB := stickyProcess(store), stickyProcess(store)
	auths := stickyAuths()

	// Advance node a's round-robin so the binding is not the default pick.
	pickID(t, nodeA, "warm-up", nil, auths)
	first := pickID(t, nodeA, "sess-1", nil, auths)
	if first != "b" {
		t.Fatalf("setup: picked %s", first)
	}
	if got := pickID(t, nodeA, "sess-1", nil, auths); got != first {
		t.Fatalf("with the store down the local table keeps the conversation, got %s", got)
	}

	store.down.Store(false)
	if got := pickID(t, nodeA, "sess-1", nil, auths); got != first {
		t.Fatalf("after recovery the local binding stands, got %s", got)
	}
	if store.boundTo(selectionKeyFor("sess-1")) != first {
		t.Fatal("the local binding must be published once the store is back")
	}
	if got := pickID(t, nodeB, "sess-1", nil, auths); got != first {
		t.Fatalf("node b must now follow it, got %s", got)
	}
}

func TestManagerSetSessionAffinityStoreSurvivesSelectorSwap(t *testing.T) {
	m := NewManager(nil, nil, nil)
	store := newFakeAffinityStore()
	m.SetSessionAffinityStore(store)
	m.SetSelector(NewSessionStickySelector(nil))
	if m.sessionStickySelector.deps.sessionAffinity() == nil {
		t.Fatal("a replaced selector must keep the shared store")
	}
	m.SetSessionAffinityStore(nil)
	if m.sessionStickySelector.deps.sessionAffinity() != nil {
		t.Fatal("nil must restore per-process bindings")
	}
}

func selectionKeyFor(session string) string {
	return sessionStickySelectionKey("codex", stickyOptions(session, nil), session)
}
