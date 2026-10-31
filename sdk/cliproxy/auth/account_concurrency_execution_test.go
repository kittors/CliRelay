package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// newQueueTestManager wires a manager whose only provider blocks until released,
// so tests can hold an account at its concurrency limit deterministically.
func newQueueTestManager(t *testing.T, waitTimeout time.Duration, auths ...*Auth) (*Manager, *blockingExecutor) {
	t.Helper()
	mgr := NewManager(nil, &FillFirstSelector{}, nil)
	mgr.SetAccountConcurrencyConfig(waitTimeout, 0)
	for _, auth := range auths {
		if _, err := mgr.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	exec := &blockingExecutor{
		started: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	mgr.RegisterExecutor(exec)
	return mgr, exec
}

func queueTestAuth(id string, limit int) *Auth {
	return &Auth{
		ID:         id,
		Provider:   "blocking",
		Status:     StatusActive,
		Attributes: map[string]string{"concurrency_limit": strconv.Itoa(limit)},
	}
}

// TestManagerExecute_QueuesWhenAllCandidatesSaturated covers the reported failure:
// a single account at its limit used to return an error immediately, so a client
// sending one request more than the limit got a hard failure instead of waiting.
func TestManagerExecute_QueuesWhenAllCandidatesSaturated(t *testing.T) {
	auth := queueTestAuth("auth-queue-exec", 1)
	mgr, exec := newQueueTestManager(t, 5*time.Second, auth)

	firstDone := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		firstDone <- err
	}()
	<-exec.started

	queuedDone := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		queuedDone <- err
	}()

	// The second request must be waiting on the account, not failing.
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), auth.ID, 1)
	select {
	case err := <-queuedDone:
		t.Fatalf("second request failed instead of queuing: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(exec.release)

	for _, done := range []chan error{firstDone, queuedDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("request never completed after the account freed a slot")
		}
	}

	if calls := exec.callCount.Load(); calls != 2 {
		t.Fatalf("expected both requests to reach the upstream, got %d calls", calls)
	}
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("expected in-flight 0 after both requests, got %d", inFlight)
	}
}

func TestManagerExecute_QueueTimeoutReportsRateLimit(t *testing.T) {
	auth := queueTestAuth("auth-queue-timeout-exec", 1)
	mgr, exec := newQueueTestManager(t, 100*time.Millisecond, auth)
	defer close(exec.release)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	}()
	<-exec.started

	start := time.Now()
	_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the queued request to fail once the wait timed out")
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("request gave up after %s, before the configured wait elapsed", elapsed)
	}
	if !errors.Is(err, ErrAccountConcurrencyExceeded) {
		t.Fatalf("expected ErrAccountConcurrencyExceeded, got %v", err)
	}
	// A busy account is a rate limit, not a server fault: a 500 here would tell
	// the client the proxy broke and skip its own backoff.
	if status := statusCodeFromError(err); status != http.StatusTooManyRequests {
		t.Fatalf("expected HTTP 429 to be reported, got %d (err=%v)", status, err)
	}
	// The HTTP layer reads the status through a bare type assertion on the error
	// it receives, so the error must not be wrapped on its way out of Execute.
	statusCoder, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error reaching the HTTP layer does not expose StatusCode(): %T", err)
	}
	if code := statusCoder.StatusCode(); code != http.StatusTooManyRequests {
		t.Fatalf("HTTP layer would answer %d instead of 429", code)
	}
}

func TestManagerExecute_QueueStopsWhenClientDisconnects(t *testing.T) {
	auth := queueTestAuth("auth-queue-cancel-exec", 1)
	mgr, exec := newQueueTestManager(t, time.Minute, auth)
	defer close(exec.release)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	}()
	<-exec.started

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(ctx, []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		errCh <- err
	}()

	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), auth.ID, 1)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued request kept waiting after the client disconnected")
	}
	if depth := mgr.ConcurrencyLimiter().QueueDepth(auth.ID); depth != 0 {
		t.Fatalf("expected the abandoned request to leave the queue, got depth %d", depth)
	}
}

func TestManagerExecute_PrefersIdleAccountOverQueuing(t *testing.T) {
	busy := queueTestAuth("auth-busy", 1)
	busy.Attributes["priority"] = "10"
	idle := queueTestAuth("auth-idle", 5)
	idle.Attributes["priority"] = "5"

	mgr, exec := newQueueTestManager(t, time.Minute, busy, idle)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	}()
	<-exec.started

	done := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		done <- err
	}()
	<-exec.started

	// Failover still wins over waiting: the second request runs on the idle
	// account rather than queuing behind the saturated one.
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight(idle.ID); inFlight != 1 {
		t.Fatalf("expected the idle account to take the request, in-flight=%d", inFlight)
	}
	if depth := mgr.ConcurrencyLimiter().QueueDepth(busy.ID); depth != 0 {
		t.Fatalf("expected no queuing while an idle account exists, got depth %d", depth)
	}

	close(exec.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("failover request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failover request never completed")
	}
}

func TestManagerExecuteStream_QueuedRequestResumesAfterStreamCloses(t *testing.T) {
	auth := queueTestAuth("auth-queue-stream", 1)
	mgr, exec := newQueueTestManager(t, 5*time.Second, auth)

	res, err := mgr.ExecuteStream(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	<-exec.started

	queued := make(chan error, 1)
	go func() {
		_, errQueued := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		queued <- errQueued
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), auth.ID, 1)

	// The stream holds the slot until its chunks are drained, so the queued
	// request must resume only once the stream is fully closed.
	close(exec.release)
	for range res.Chunks {
	}

	select {
	case errQueued := <-queued:
		if errQueued != nil {
			t.Fatalf("queued request failed after the stream closed: %v", errQueued)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued request never resumed after the stream closed")
	}
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("expected in-flight 0 after both requests, got %d", inFlight)
	}
}

// queueThenFailExecutor holds the first request open and fails every later one,
// which is how an account that is busy *and* broken behaves.
type queueThenFailExecutor struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (e *queueThenFailExecutor) Identifier() string { return "blocking" }

func (e *queueThenFailExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.calls.Add(1) == 1 {
		e.started <- struct{}{}
		select {
		case <-e.release:
		case <-ctx.Done():
			return cliproxyexecutor.Response{}, ctx.Err()
		}
		return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
	}
	return cliproxyexecutor.Response{}, &Error{Code: "upstream_failed", Message: "upstream rejected the request", HTTPStatus: http.StatusBadGateway}
}

func (e *queueThenFailExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{Code: "upstream_failed", Message: "upstream rejected the request", HTTPStatus: http.StatusBadGateway}
}

func (e *queueThenFailExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *queueThenFailExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *queueThenFailExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// TestManagerExecute_QueuedRequestDoesNotLoopOnFailure guards the queue against
// re-queuing an account that already had its turn: without that, a request whose
// upstream call keeps failing would cycle through the queue forever instead of
// reporting the failure.
func TestManagerExecute_QueuedRequestDoesNotLoopOnFailure(t *testing.T) {
	auth := queueTestAuth("auth-queue-no-loop", 1)
	mgr := NewManager(nil, &FillFirstSelector{}, nil)
	mgr.SetAccountConcurrencyConfig(5*time.Second, 0)
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	exec := &queueThenFailExecutor{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	mgr.RegisterExecutor(exec)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	}()
	<-exec.started

	queued := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		queued <- err
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), auth.ID, 1)

	close(exec.release)

	select {
	case err := <-queued:
		if err == nil {
			t.Fatal("expected the queued request to report the upstream failure")
		}
		if !strings.Contains(err.Error(), "upstream rejected the request") {
			t.Fatalf("expected the upstream error to surface, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request never returned; it is looping through the queue")
	}

	if calls := exec.calls.Load(); calls != 2 {
		t.Fatalf("expected exactly one retry attempt per account, got %d upstream calls", calls)
	}
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("expected in-flight 0 after the failure, got %d", inFlight)
	}
	if depth := mgr.ConcurrencyLimiter().QueueDepth(auth.ID); depth != 0 {
		t.Fatalf("expected an empty queue after the failure, got depth %d", depth)
	}
}

func TestManagerCountTokens_QueuesWhenSaturated(t *testing.T) {
	auth := queueTestAuth("auth-queue-count", 1)
	mgr, exec := newQueueTestManager(t, 5*time.Second, auth)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	}()
	<-exec.started

	counted := make(chan error, 1)
	go func() {
		_, err := mgr.ExecuteCount(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		counted <- err
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), auth.ID, 1)

	close(exec.release)
	select {
	case err := <-counted:
		if err != nil {
			t.Fatalf("queued count-tokens request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued count-tokens request never resumed")
	}
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("expected in-flight 0 after both requests, got %d", inFlight)
	}
}
