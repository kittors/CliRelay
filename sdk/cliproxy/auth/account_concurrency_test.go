package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAccountConcurrencyLimiter_AcquireAndRelease(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth := &Auth{
		ID:         "auth-limit-2",
		Attributes: map[string]string{"concurrency_limit": "2"},
	}

	if limit := auth.ConcurrencyLimit(); limit != 2 {
		t.Fatalf("expected concurrency limit 2, got %d", limit)
	}

	if !limiter.HasAvailableSlot(auth) {
		t.Fatal("expected slot available initially")
	}

	rel1, err1 := limiter.AcquireSlot(auth)
	if err1 != nil {
		t.Fatalf("failed to acquire slot 1: %v", err1)
	}
	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 1 {
		t.Fatalf("expected in-flight 1, got %d", inFlight)
	}
	if rate := limiter.GetLoadRate(auth.ID, 2); rate != 0.5 {
		t.Fatalf("expected load rate 0.5, got %f", rate)
	}

	rel2, err2 := limiter.AcquireSlot(auth)
	if err2 != nil {
		t.Fatalf("failed to acquire slot 2: %v", err2)
	}
	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 2 {
		t.Fatalf("expected in-flight 2, got %d", inFlight)
	}
	if limiter.HasAvailableSlot(auth) {
		t.Fatal("expected slot unavailable at capacity")
	}

	// Third attempt must fail
	rel3, err3 := limiter.AcquireSlot(auth)
	if err3 == nil {
		if rel3 != nil {
			rel3()
		}
		t.Fatal("expected 3rd acquire to fail due to concurrency limit")
	}
	if !errors.Is(err3, ErrAccountConcurrencyExceeded) {
		t.Fatalf("expected ErrAccountConcurrencyExceeded, got %v", err3)
	}

	// Release one
	rel1()
	// Duplicate release should be safe
	rel1()

	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 1 {
		t.Fatalf("expected in-flight 1 after release, got %d", inFlight)
	}
	if !limiter.HasAvailableSlot(auth) {
		t.Fatal("expected slot available after release")
	}

	// Now 3rd attempt can succeed
	rel3, err3 = limiter.AcquireSlot(auth)
	if err3 != nil {
		t.Fatalf("expected slot to be acquirable after release: %v", err3)
	}
	defer rel3()
	defer rel2()
}

func TestAccountConcurrencyLimiter_FilterAvailableCandidates(t *testing.T) {
	limiter := NewAccountConcurrencyLimiter()
	auth1 := &Auth{
		ID:         "auth-1",
		Attributes: map[string]string{"concurrency": "1"},
	}
	auth2 := &Auth{
		ID:         "auth-2",
		Attributes: map[string]string{"concurrency_limit": "2"},
	}
	auth3 := &Auth{
		ID: "auth-unlimited",
	}

	candidates := []*Auth{auth1, auth2, auth3}

	// All available initially
	filtered := limiter.FilterAvailableCandidates(candidates)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(filtered))
	}

	// Saturate auth1
	rel1, err := limiter.AcquireSlot(auth1)
	if err != nil {
		t.Fatalf("acquire auth1: %v", err)
	}
	defer rel1()

	filtered = limiter.FilterAvailableCandidates(candidates)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 candidates (auth2, auth3), got %d", len(filtered))
	}
	if filtered[0].ID != "auth-2" || filtered[1].ID != "auth-unlimited" {
		t.Fatalf("unexpected candidates: %v, %v", filtered[0].ID, filtered[1].ID)
	}

	// Saturate auth2 (2 slots)
	rel2a, _ := limiter.AcquireSlot(auth2)
	rel2b, _ := limiter.AcquireSlot(auth2)
	defer rel2a()
	defer rel2b()

	filtered = limiter.FilterAvailableCandidates(candidates)
	if len(filtered) != 1 || filtered[0].ID != "auth-unlimited" {
		t.Fatalf("expected only auth-unlimited, got %v", filtered)
	}
}

type blockingExecutor struct {
	started   chan struct{}
	release   chan struct{}
	callCount atomic.Int32
}

func (e *blockingExecutor) Identifier() string { return "blocking" }
func (e *blockingExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.callCount.Add(1)
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return cliproxyexecutor.Response{}, ctx.Err()
		}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *blockingExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.callCount.Add(1)
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	go func() {
		defer close(chunks)
		if e.release != nil {
			select {
			case <-e.release:
			case <-ctx.Done():
				return
			}
		}
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: chunk\n\n")}
	}()
	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{},
		Chunks:  chunks,
	}, nil
}

func (e *blockingExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *blockingExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}
func (e *blockingExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("not implemented")
}

func TestManagerExecute_AccountConcurrencyFailover(t *testing.T) {
	mgr := NewManager(nil, &FillFirstSelector{}, nil)

	auth1 := &Auth{
		ID:         "auth-limited-1",
		Provider:   "blocking",
		Status:     StatusActive,
		Attributes: map[string]string{"concurrency_limit": "1", "priority": "10"},
	}
	auth2 := &Auth{
		ID:         "auth-fallback-2",
		Provider:   "blocking",
		Status:     StatusActive,
		Attributes: map[string]string{"concurrency_limit": "5", "priority": "5"},
	}

	_, _ = mgr.Register(context.Background(), auth1)
	_, _ = mgr.Register(context.Background(), auth2)

	exec := &blockingExecutor{
		started: make(chan struct{}, 10),
		release: make(chan struct{}),
	}
	mgr.RegisterExecutor(exec)

	// Launch 1st request on auth1
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		if err != nil {
			t.Errorf("req1 failed: %v", err)
		}
	}()

	// Wait for 1st request to start
	<-exec.started

	// Verify auth1 has 1 in-flight
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight("auth-limited-1"); inFlight != 1 {
		t.Fatalf("expected auth-limited-1 in-flight 1, got %d", inFlight)
	}

	// Launch 2nd request - auth1 is saturated, FillFirst should pick auth2
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		if err != nil {
			t.Errorf("req2 failed: %v", err)
		}
	}()

	// Wait for 2nd request to start
	<-exec.started

	if inFlight := mgr.ConcurrencyLimiter().GetInFlight("auth-fallback-2"); inFlight != 1 {
		t.Fatalf("expected auth-fallback-2 in-flight 1, got %d", inFlight)
	}

	// Release both
	close(exec.release)
	wg.Wait()

	// Both in-flight counts should return to 0
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight("auth-limited-1"); inFlight != 0 {
		t.Fatalf("expected auth-limited-1 in-flight 0, got %d", inFlight)
	}
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight("auth-fallback-2"); inFlight != 0 {
		t.Fatalf("expected auth-fallback-2 in-flight 0, got %d", inFlight)
	}
}

func TestManagerExecuteStream_AccountConcurrencySlotLifecycle(t *testing.T) {
	mgr := NewManager(nil, &FillFirstSelector{}, nil)

	auth := &Auth{
		ID:         "auth-stream-1",
		Provider:   "blocking",
		Status:     StatusActive,
		Attributes: map[string]string{"concurrency_limit": "1"},
	}
	_, _ = mgr.Register(context.Background(), auth)

	exec := &blockingExecutor{
		started: make(chan struct{}, 10),
		release: make(chan struct{}),
	}
	mgr.RegisterExecutor(exec)

	res, err := mgr.ExecuteStream(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}

	// Wait for stream to start
	<-exec.started

	// Slot must be held while stream is open
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight("auth-stream-1"); inFlight != 1 {
		t.Fatalf("expected in-flight 1 during active stream, got %d", inFlight)
	}

	// Second request must fail with ErrAccountConcurrencyExceeded (or auth_not_found since only 1 saturated auth exists)
	_, err2 := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err2 == nil {
		t.Fatal("expected second request to fail when account slot saturated")
	}

	// Release stream chunks
	close(exec.release)

	// Consume stream
	for range res.Chunks {
	}

	// Small grace period for goroutine cleanup
	time.Sleep(10 * time.Millisecond)

	// Slot must be released after stream closes
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight("auth-stream-1"); inFlight != 0 {
		t.Fatalf("expected in-flight 0 after stream closed, got %d", inFlight)
	}
}
