package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// A background refresh may spend a single-use refresh token. These tests stop
// the refresh loop (as Shutdown does) while an exchange is in flight and check
// that the rotated token still reaches memory and the store.

// ctxStore fails a write whose context has ended, as a database write does,
// and remembers the refresh token of every write that succeeded.
type ctxStore struct {
	mu    sync.Mutex
	saved []string
}

func (s *ctxStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *ctxStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, authRefreshToken(auth))
	return "", nil
}

func (s *ctxStore) Delete(context.Context, string) error { return nil }

func (s *ctxStore) savedTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.saved...)
}

// rotatingExecutor exchanges a refresh token the way an HTTP client does: it
// gives up when its context ends. Each exchange blocks until release is
// closed, so a test can stop the refresh loop in between.
type rotatingExecutor struct {
	entered chan struct{}
	release chan struct{}
	calls   chan struct{}
}

func newRotatingExecutor() *rotatingExecutor {
	return &rotatingExecutor{entered: make(chan struct{}, 8), release: make(chan struct{}), calls: make(chan struct{}, 8)}
}

func (e *rotatingExecutor) Identifier() string { return "test-provider" }

func (e *rotatingExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *rotatingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *rotatingExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.calls <- struct{}{}
	e.entered <- struct{}{}
	select {
	case <-e.release:
	case <-ctx.Done():
		// The response with the rotated token is never read.
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	updated := auth.Clone()
	updated.Metadata["refresh_token"] = "rt-rotated"
	updated.Metadata["access_token"] = "at-rotated"
	return updated, nil
}

func (e *rotatingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *rotatingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type alwaysDue struct{}

func (alwaysDue) ShouldRefresh(time.Time, *Auth) bool { return true }

func newRotatingManager(t *testing.T) (*Manager, *ctxStore, *rotatingExecutor) {
	t.Helper()
	store := &ctxStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "rotating-auth",
		Provider: "test-provider",
		Runtime:  alwaysDue{},
		Metadata: map[string]any{"type": "codex", "refresh_token": "rt-original", "access_token": "at-original"},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}
	exec := newRotatingExecutor()
	mgr.RegisterExecutor(exec)
	return mgr, store, exec
}

func waitEntered(t *testing.T, exec *rotatingExecutor) {
	t.Helper()
	select {
	case <-exec.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh never reached the executor")
	}
}

func assertRotatedKept(t *testing.T, mgr *Manager, store *ctxStore) {
	t.Helper()
	current, ok := mgr.GetByID("rotating-auth")
	if !ok {
		t.Fatal("credential disappeared")
	}
	if got := authRefreshToken(current); got != "rt-rotated" {
		t.Fatalf("refresh token in memory = %q, want the rotated one; the old one is retired upstream", got)
	}
	saved := store.savedTokens()
	if len(saved) == 0 || saved[len(saved)-1] != "rt-rotated" {
		t.Fatalf("store writes = %v, want the rotated refresh token persisted", saved)
	}
}

func TestRefreshAuthKeepsTheRotatedTokenWhenTheLoopStopsMidExchange(t *testing.T) {
	mgr, store, exec := newRotatingManager(t)
	loop, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.refreshAuth(loop, "rotating-auth")
	}()
	waitEntered(t, exec)
	stop() // StopAutoRefresh during shutdown
	close(exec.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refreshAuth did not return")
	}
	assertRotatedKept(t, mgr, store)
}

func TestRefreshAuthStartsNothingOnceTheLoopStopped(t *testing.T) {
	mgr, store, exec := newRotatingManager(t)
	close(exec.release) // a refresh that did start would finish at once
	loop, stop := context.WithCancel(context.Background())
	stop()
	mgr.refreshAuth(loop, "rotating-auth")
	if len(exec.calls) != 0 {
		t.Fatal("a refresh token was spent after the loop had stopped")
	}
	if saved := store.savedTokens(); len(saved) != 0 {
		t.Fatalf("store writes = %v, want none", saved)
	}
}

func TestWaitForRefreshesWaitsForTheLoopsRefreshes(t *testing.T) {
	mgr, store, exec := newRotatingManager(t)
	loop, stop := context.WithCancel(context.Background())
	mgr.checkRefreshes(loop)
	waitEntered(t, exec)
	stop()

	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if err := mgr.WaitForRefreshes(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForRefreshes = %v while a refresh is in flight, want it to keep waiting", err)
	}

	close(exec.release)
	wait, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	if err := mgr.WaitForRefreshes(wait); err != nil {
		t.Fatalf("WaitForRefreshes = %v", err)
	}
	// WaitForRefreshes returned, so the result must already be stored.
	assertRotatedKept(t, mgr, store)
}

func TestWaitForRefreshesWithNothingInFlight(t *testing.T) {
	mgr := NewManager(&ctxStore{}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mgr.WaitForRefreshes(ctx); err != nil {
		t.Fatalf("WaitForRefreshes = %v, want nil", err)
	}
	var nilManager *Manager
	if err := nilManager.WaitForRefreshes(ctx); err != nil {
		t.Fatalf("nil manager: %v", err)
	}
}
