package auth

import (
	"context"
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
