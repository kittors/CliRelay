package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// A sticky binding used to be dropped the moment its account reached its
// concurrency limit: selection removed saturated accounts before the sticky
// selector looked, so the binding read as gone, was released, and the
// conversation moved to another account for good, paying a full-price turn
// there. These tests pin the replacement: the binding stands, the request
// queues on its account, and only a request whose wait runs out moves, alone.

func saturationTestAuths() []*Auth {
	limited := func(id string) *Auth {
		return &Auth{ID: id, Provider: "codex", Attributes: map[string]string{"concurrency_limit": "1"}}
	}
	return []*Auth{limited("a"), limited("b")}
}

func authWithID(t *testing.T, auths []*Auth, id string) *Auth {
	t.Helper()
	for _, auth := range auths {
		if auth.ID == id {
			return auth
		}
	}
	t.Fatalf("no test auth %q", id)
	return nil
}

func otherAuthID(auths []*Auth, id string) string {
	for _, auth := range auths {
		if auth.ID != id {
			return auth.ID
		}
	}
	return ""
}

// saturate holds auth at its concurrency limit; the returned func frees it.
func saturate(t *testing.T, limiter *AccountConcurrencyLimiter, auth *Auth) func() {
	t.Helper()
	release, err := limiter.AcquireSlot(auth)
	if err != nil {
		t.Fatalf("acquire slot on %s: %v", auth.ID, err)
	}
	var once sync.Once
	free := func() { once.Do(release) }
	t.Cleanup(free)
	return free
}

// pickAsManager picks the way the manager's selection does: every candidate,
// plus the ones with a free slot.
func pickAsManager(t *testing.T, s *SessionStickySelector, note *stickyPickNote, session string, extra map[string]any, auths []*Auth) string {
	t.Helper()
	unsaturated := s.deps.limiter.FilterAvailableCandidates(auths)
	picked, err := s.pick(withStickyPickNote(context.Background(), note), "codex", "gpt-5.5", stickyOptions(session, extra), auths, unsaturated)
	if err != nil {
		t.Fatalf("pick(%s): %v", session, err)
	}
	return picked.ID
}

func TestSessionStickyBindingStandsWhileItsAccountIsSaturated(t *testing.T) {
	t.Parallel()
	selector := stickyProcess(nil)
	auths := saturationTestAuths()
	bound := pickID(t, selector, "sess-1", nil, auths)
	free := saturate(t, selector.deps.limiter, authWithID(t, auths, bound))

	note := &stickyPickNote{}
	if got := pickAsManager(t, selector, note, "sess-1", nil, auths); got != bound {
		t.Fatalf("pick with %s at its limit = %s, want %s: moving costs the conversation its prompt cache", bound, got, bound)
	}
	if !note.holds(bound) {
		t.Fatalf("the note must name the bound account so the request queues on it, got %+v", *note)
	}

	free()
	if got := pickID(t, selector, "sess-1", nil, auths); got != bound {
		t.Fatalf("after the saturation passed the session went to %s, want its binding %s", got, bound)
	}
}

func TestSessionStickyNewBindingAvoidsSaturatedAccount(t *testing.T) {
	t.Parallel()
	selector := stickyProcess(nil)
	auths := saturationTestAuths()
	saturate(t, selector.deps.limiter, authWithID(t, auths, "a"))

	note := &stickyPickNote{}
	if got := pickAsManager(t, selector, note, "sess-new", nil, auths); got != "b" {
		t.Fatalf("a new conversation went to %s, want the account with room (b)", got)
	}
	if !note.holds("b") {
		t.Fatalf("the note must name the new binding, got %+v", *note)
	}
}

func TestSessionStickyOverflowLeavesBindingAlone(t *testing.T) {
	t.Parallel()
	selector := stickyProcess(nil)
	auths := saturationTestAuths()
	bound := pickID(t, selector, "sess-1", nil, auths)
	other := otherAuthID(auths, bound)

	// The request waited for its account in vain: that account is now among the
	// request's tried ones, so only the other is a candidate.
	note := &stickyPickNote{overflow: true}
	if got := pickAsManager(t, selector, note, "sess-1", nil, []*Auth{authWithID(t, auths, other)}); got != other {
		t.Fatalf("the overflowing request went to %s, want %s", got, other)
	}
	if note.bound != "" {
		t.Fatalf("an overflow pick must not claim a binding, got %+v", *note)
	}
	if got := pickID(t, selector, "sess-1", nil, auths); got != bound {
		t.Fatalf("the next turn went to %s, want the untouched binding %s", got, bound)
	}
}

func TestSessionStickyReleaseAtLoadIgnoresConcurrency(t *testing.T) {
	t.Parallel()
	deps := &schedulerDeps{
		tracker: newSelectionPressureTracker(),
		limiter: NewAccountConcurrencyLimiter(),
		quota:   stubQuotaSource{"a": 0.1, "b": 0.1},
	}
	selector := NewSessionStickySelector(&RoundRobinSelector{deps: deps})
	selector.deps = deps
	auths := saturationTestAuths()
	if got := pickID(t, selector, "sess-1", nil, auths[:1]); got != "a" {
		t.Fatalf("binding pick = %s, want a", got)
	}
	saturate(t, deps.limiter, auths[0])

	// A full concurrency slot is load of 1.0 to accountLoadRatio, but the
	// release threshold is about the quota window, which is barely touched.
	release := map[string]any{stickyReleaseAtLoadKey: "0.8"}
	if got := pickAsManager(t, selector, &stickyPickNote{}, "sess-1", release, auths); got != "a" {
		t.Fatalf("a binding at its concurrency limit was released to %s; release-at-load must follow the quota", got)
	}
}

func TestSharedStickyBindingStandsWhileItsAccountIsSaturated(t *testing.T) {
	t.Parallel()
	store := newFakeAffinityStore()
	nodeA, nodeB := stickyProcess(store), stickyProcess(store)
	auths := saturationTestAuths()
	bound := pickID(t, nodeA, "sess-1", nil, auths)
	saturate(t, nodeB.deps.limiter, authWithID(t, auths, bound))

	if got := pickAsManager(t, nodeB, &stickyPickNote{}, "sess-1", nil, auths); got != bound {
		t.Fatalf("node b moved the conversation to %s while %s was saturated", got, bound)
	}
	if stored := store.boundTo(selectionKeyFor("sess-1")); stored != bound {
		t.Fatalf("the shared binding must survive saturation, it names %q", stored)
	}
}

// stickyRecordingExecutor blocks every call until release is closed and
// records which account each call ran on.
type stickyRecordingExecutor struct {
	started chan string
	release chan struct{}
	mu      sync.Mutex
	calls   []string
}

func (e *stickyRecordingExecutor) Identifier() string { return "blocking" }

func (e *stickyRecordingExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	e.mu.Unlock()
	e.started <- auth.ID
	select {
	case <-e.release:
	case <-ctx.Done():
		return cliproxyexecutor.Response{}, ctx.Err()
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *stickyRecordingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not used")
}

func (e *stickyRecordingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *stickyRecordingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not used")
}

func (e *stickyRecordingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (e *stickyRecordingExecutor) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

const (
	stickyQueueAuthA = "sticky-queue-a"
	stickyQueueAuthB = "sticky-queue-b"
)

// newStickyQueueManager wires session-sticky routing over two accounts that
// take one request each.
func newStickyQueueManager(t *testing.T, wait time.Duration, ids ...string) (*Manager, *stickyRecordingExecutor) {
	t.Helper()
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "session-sticky", IncludeDefaultGroup: false}})
	mgr.SetAccountConcurrencyConfig(wait, 0)
	exec := &stickyRecordingExecutor{started: make(chan string, 8), release: make(chan struct{})}
	mgr.RegisterExecutor(exec)
	for _, id := range ids {
		if _, err := mgr.Register(context.Background(), queueTestAuth(id, 1)); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	return mgr, exec
}

func stickyTurn(mgr *Manager, session string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{
			SourceFormat: "openai",
			Metadata:     map[string]any{cliproxyexecutor.SessionStickyMetadataKey: "header:session-id:" + session},
		})
		done <- err
	}()
	return done
}

func awaitStarted(t *testing.T, exec *stickyRecordingExecutor) string {
	t.Helper()
	select {
	case id := <-exec.started:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the upstream")
		return ""
	}
}

func awaitTurn(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("turn failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn never completed")
	}
}

func TestManagerExecute_StickySessionQueuesOnItsSaturatedAccount(t *testing.T) {
	mgr, exec := newStickyQueueManager(t, 5*time.Second, stickyQueueAuthA, stickyQueueAuthB)

	first := stickyTurn(mgr, "sess-1")
	bound := awaitStarted(t, exec)
	idle := stickyQueueAuthA
	if bound == idle {
		idle = stickyQueueAuthB
	}

	second := stickyTurn(mgr, "sess-1")
	// The next turn waits for its own account rather than taking the idle one.
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), bound, 1)
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight(idle); inFlight != 0 {
		t.Fatalf("the turn moved to the idle account %s (in-flight %d) instead of queuing on %s", idle, inFlight, bound)
	}

	close(exec.release)
	awaitTurn(t, first)
	awaitTurn(t, second)
	if calls := exec.recorded(); len(calls) != 2 || calls[0] != bound || calls[1] != bound {
		t.Fatalf("calls = %v, want both turns on %s", calls, bound)
	}
}

func TestManagerExecute_StickySessionOverflowKeepsItsBinding(t *testing.T) {
	mgr, exec := newStickyQueueManager(t, 100*time.Millisecond, stickyQueueAuthA, stickyQueueAuthB)

	first := stickyTurn(mgr, "sess-1")
	bound := awaitStarted(t, exec)

	// The bound account stays busy past the wait, so this turn runs elsewhere...
	second := stickyTurn(mgr, "sess-1")
	if moved := awaitStarted(t, exec); moved == bound {
		t.Fatalf("the second turn ran on %s while it was still at its limit", bound)
	}
	close(exec.release)
	awaitTurn(t, first)
	awaitTurn(t, second)

	// ...without taking the conversation with it.
	third := stickyTurn(mgr, "sess-1")
	if got := awaitStarted(t, exec); got != bound {
		t.Fatalf("the turn after an overflow went to %s, want the conversation's account %s", got, bound)
	}
	awaitTurn(t, third)
}

func TestManagerExecute_StickySessionWaitTimeoutStillReportsRateLimit(t *testing.T) {
	mgr, exec := newStickyQueueManager(t, 100*time.Millisecond, stickyQueueAuthA)
	defer close(exec.release)

	_ = stickyTurn(mgr, "sess-1")
	awaitStarted(t, exec)

	start := time.Now()
	_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{
		SourceFormat: "openai",
		Metadata:     map[string]any{cliproxyexecutor.SessionStickyMetadataKey: "header:session-id:sess-1"},
	})
	if err == nil {
		t.Fatal("expected the turn to fail once its only account stayed busy past the wait")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("the turn gave up after %s, want one wait of 100ms", elapsed)
	}
	if !errors.Is(err, ErrAccountConcurrencyExceeded) || statusCodeFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("expected a 429 for the busy account, got %v", err)
	}
}
