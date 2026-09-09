package auth

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAccountConcurrency_HighConcurrencyStress(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)

	numAccounts := 5
	limitPerAccount := 3
	for i := 0; i < numAccounts; i++ {
		auth := &Auth{
			ID:         fmt.Sprintf("auth-stress-%d", i),
			Provider:   "blocking",
			Status:     StatusActive,
			Attributes: map[string]string{"concurrency_limit": fmt.Sprintf("%d", limitPerAccount)},
		}
		_, _ = mgr.Register(context.Background(), auth)
	}

	exec := &stressDelayExecutor{
		minDelay: 5 * time.Millisecond,
		maxDelay: 20 * time.Millisecond,
	}
	mgr.RegisterExecutor(exec)

	// Launch 50 concurrent requests
	numWorkers := 50
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	var successCount atomic.Int32
	var rejectedCount atomic.Int32
	var maxObservedInFlight [5]atomic.Int32

	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			defer wg.Done()
			isStream := workerID%2 == 0
			if isStream {
				res, err := mgr.ExecuteStream(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "stress-model"}, cliproxyexecutor.Options{})
				if err != nil {
					rejectedCount.Add(1)
					return
				}
				for range res.Chunks {
				}
				successCount.Add(1)
			} else {
				_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "stress-model"}, cliproxyexecutor.Options{})
				if err != nil {
					rejectedCount.Add(1)
					return
				}
				successCount.Add(1)
			}
		}(i)
	}

	// Concurrently monitor that in-flight NEVER exceeds limitPerAccount
	stopMonitor := make(chan struct{})
	go func() {
		limiter := mgr.ConcurrencyLimiter()
		for {
			select {
			case <-stopMonitor:
				return
			default:
				for i := 0; i < numAccounts; i++ {
					inFlight := limiter.GetInFlight(fmt.Sprintf("auth-stress-%d", i))
					if inFlight > limitPerAccount {
						t.Errorf("INVARIANT VIOLATION: auth-stress-%d in-flight=%d > limit=%d", i, inFlight, limitPerAccount)
					}
					for {
						curMax := maxObservedInFlight[i].Load()
						if int32(inFlight) <= curMax || maxObservedInFlight[i].CompareAndSwap(curMax, int32(inFlight)) {
							break
						}
					}
				}
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	wg.Wait()
	close(stopMonitor)

	// Ensure all accounts returned to 0 in-flight
	limiter := mgr.ConcurrencyLimiter()
	for i := 0; i < numAccounts; i++ {
		if inFlight := limiter.GetInFlight(fmt.Sprintf("auth-stress-%d", i)); inFlight != 0 {
			t.Fatalf("auth-stress-%d residual in-flight = %d, expected 0", i, inFlight)
		}
	}

	t.Logf("Stress test finished: success=%d, rejected=%d", successCount.Load(), rejectedCount.Load())
	for i := 0; i < numAccounts; i++ {
		t.Logf("Auth %d peak in-flight: %d / %d", i, maxObservedInFlight[i].Load(), limitPerAccount)
	}
}

// TestAccountConcurrency_OverloadQueueStress drives far more requests than the
// pool has slots. Every request must still be served: queuing turns an overload
// into added latency instead of the errors this used to return.
func TestAccountConcurrency_OverloadQueueStress(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.SetAccountConcurrencyConfig(30*time.Second, 0)

	const (
		limitPerAccount = 2
		numWorkers      = 120
	)
	auth := &Auth{
		ID:         "auth-overload",
		Provider:   "blocking",
		Status:     StatusActive,
		Attributes: map[string]string{"concurrency_limit": fmt.Sprintf("%d", limitPerAccount)},
	}
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	mgr.RegisterExecutor(&stressDelayExecutor{minDelay: time.Millisecond, maxDelay: 5 * time.Millisecond})

	limiter := mgr.ConcurrencyLimiter()
	stopMonitor := make(chan struct{})
	monitorDone := make(chan struct{})
	var peakInFlight atomic.Int32
	go func() {
		defer close(monitorDone)
		for {
			select {
			case <-stopMonitor:
				return
			default:
			}
			inFlight := int32(limiter.GetInFlight(auth.ID))
			if inFlight > limitPerAccount {
				t.Errorf("INVARIANT VIOLATION: in-flight=%d > limit=%d", inFlight, limitPerAccount)
			}
			for {
				current := peakInFlight.Load()
				if inFlight <= current || peakInFlight.CompareAndSwap(current, inFlight) {
					break
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	var succeeded, failed atomic.Int32
	firstErr := make(chan error, numWorkers)
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			defer wg.Done()
			var err error
			if workerID%2 == 0 {
				var res *cliproxyexecutor.StreamResult
				res, err = mgr.ExecuteStream(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "stress-model"}, cliproxyexecutor.Options{})
				if err == nil {
					for range res.Chunks {
					}
				}
			} else {
				_, err = mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "stress-model"}, cliproxyexecutor.Options{})
			}
			if err != nil {
				failed.Add(1)
				firstErr <- err
				return
			}
			succeeded.Add(1)
		}(i)
	}

	wg.Wait()
	close(stopMonitor)
	<-monitorDone

	if got := failed.Load(); got != 0 {
		t.Fatalf("%d/%d requests failed under overload; first error: %v", got, numWorkers, <-firstErr)
	}
	if got := succeeded.Load(); got != numWorkers {
		t.Fatalf("expected %d successful requests, got %d", numWorkers, got)
	}
	if peak := peakInFlight.Load(); peak > limitPerAccount {
		t.Fatalf("peak in-flight %d exceeded limit %d", peak, limitPerAccount)
	}
	if inFlight := limiter.GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("residual in-flight = %d, expected 0", inFlight)
	}
	if depth := limiter.QueueDepth(auth.ID); depth != 0 {
		t.Fatalf("residual queue depth = %d, expected 0", depth)
	}
	t.Logf("overload stress finished: %d requests served through %d slots, peak in-flight %d", numWorkers, limitPerAccount, peakInFlight.Load())
}

// TestAccountConcurrency_QueueDepthShedsLoad checks the safety valve: with a
// bounded queue, excess requests are rejected quickly instead of piling up.
func TestAccountConcurrency_QueueDepthShedsLoad(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.SetAccountConcurrencyConfig(10*time.Second, 2)

	auth := &Auth{
		ID:         "auth-shed",
		Provider:   "blocking",
		Status:     StatusActive,
		Attributes: map[string]string{"concurrency_limit": "1"},
	}
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	mgr.RegisterExecutor(&stressDelayExecutor{minDelay: 40 * time.Millisecond, maxDelay: 60 * time.Millisecond})

	const numWorkers = 24
	var wg sync.WaitGroup
	var succeeded, shed atomic.Int32
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func() {
			defer wg.Done()
			_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "stress-model"}, cliproxyexecutor.Options{})
			if err == nil {
				succeeded.Add(1)
				return
			}
			if !errors.Is(err, ErrAccountConcurrencyExceeded) {
				t.Errorf("unexpected shed reason: %v", err)
				return
			}
			if status := statusCodeFromError(err); status != http.StatusTooManyRequests {
				t.Errorf("shed request reported status %d, want 429", status)
			}
			shed.Add(1)
		}()
	}
	wg.Wait()

	if shed.Load() == 0 {
		t.Fatal("expected the bounded queue to shed load, nothing was rejected")
	}
	if succeeded.Load() == 0 {
		t.Fatal("expected queued requests to still be served")
	}
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight(auth.ID); inFlight != 0 {
		t.Fatalf("residual in-flight = %d, expected 0", inFlight)
	}
	if depth := mgr.ConcurrencyLimiter().QueueDepth(auth.ID); depth != 0 {
		t.Fatalf("residual queue depth = %d, expected 0", depth)
	}
	t.Logf("queue-depth shedding: served=%d shed=%d", succeeded.Load(), shed.Load())
}

type stressDelayExecutor struct {
	minDelay time.Duration
	maxDelay time.Duration
}

func (e *stressDelayExecutor) Identifier() string { return "blocking" }
func (e *stressDelayExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	delay := e.minDelay
	if diff := e.maxDelay - e.minDelay; diff > 0 {
		delay += time.Duration(rand.Int64N(int64(diff)))
	}
	select {
	case <-time.After(delay):
		return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
	case <-ctx.Done():
		return cliproxyexecutor.Response{}, ctx.Err()
	}
}

func (e *stressDelayExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	delay := e.minDelay
	if diff := e.maxDelay - e.minDelay; diff > 0 {
		delay += time.Duration(rand.Int64N(int64(diff)))
	}
	go func() {
		defer close(chunks)
		select {
		case <-time.After(delay):
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: chunk\n\n")}
		case <-ctx.Done():
			return
		}
	}()
	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{},
		Chunks:  chunks,
	}, nil
}

func (e *stressDelayExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *stressDelayExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}
func (e *stressDelayExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("not implemented")
}
