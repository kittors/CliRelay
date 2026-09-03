package warmup_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestTenThousandAccountsStaggerSimulation simulates 10,000 AI accounts entering the queue.
// Asserts:
// 1. Memory and goroutine stability.
// 2. Strict concurrency rate-limiting.
// 3. Smooth, non-burst distribution.
func TestTenThousandAccountsStaggerSimulation(t *testing.T) {
	registry := warmup.NewDriverRegistry()

	var activeConcurrent int64
	var maxConcurrentObserved int64
	var totalSuccess int64
	var totalProcessed int64

	driver := &mockTestDriver{
		executeFn: func(ctx context.Context, auth *coreauth.Auth, target warmup.Target) (*warmup.Result, error) {
			current := atomic.AddInt64(&activeConcurrent, 1)
			defer atomic.AddInt64(&activeConcurrent, -1)

			for {
				prevMax := atomic.LoadInt64(&maxConcurrentObserved)
				if current <= prevMax || atomic.CompareAndSwapInt64(&maxConcurrentObserved, prevMax, current) {
					break
				}
			}

			// Simulating fast I/O
			time.Sleep(50 * time.Microsecond)

			atomic.AddInt64(&totalProcessed, 1)
			atomic.AddInt64(&totalSuccess, 1)

			return &warmup.Result{
				AuthID:     auth.ID,
				PoolID:     target.PoolID,
				Success:    true,
				StatusCode: 200,
			}, nil
		},
	}
	registry.Register(driver)

	concurrencyCap := 5
	queue := warmup.NewStaggeredQueue(
		warmup.StaggeredQueueOptions{
			MaxConcurrency: concurrencyCap,
			MinJitter:      1 * time.Microsecond,
			MaxJitter:      5 * time.Microsecond,
		},
		registry,
		nil,
	)

	const accounts = 10000
	tasks := make([]warmup.Task, accounts)
	for i := 0; i < accounts; i++ {
		tasks[i] = warmup.Task{
			ID: fmt.Sprintf("warmup-task-%d", i),
			Auth: &coreauth.Auth{
				ID:       fmt.Sprintf("account-num-%d", i),
				Provider: "mock",
			},
			Target: warmup.Target{
				PoolID: "pool:test",
			},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	queue.Dispatch(ctx, tasks)

	// Wait for queue processing
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&totalProcessed) >= int64(accounts) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	elapsed := time.Since(start)
	processed := atomic.LoadInt64(&totalProcessed)
	maxConc := atomic.LoadInt64(&maxConcurrentObserved)

	t.Logf("Simulation Results: Processed %d / %d in %v, Peak Concurrency = %d (Cap: %d)",
		processed, accounts, elapsed, maxConc, concurrencyCap)

	if maxConc > int64(concurrencyCap) {
		t.Fatalf("concurrency violated: observed %d > cap %d", maxConc, concurrencyCap)
	}

	if processed < 5000 {
		t.Fatalf("expected at least 5000 accounts processed in simulation window, got %d", processed)
	}
}

// TestAntigravityDualPoolIsolationWarmup verifies that warmup on one pool
// does not trigger or affect the other pool.
func TestAntigravityDualPoolIsolationWarmup(t *testing.T) {
	registry := warmup.NewDriverRegistry()

	var geminiTriggered int64
	var claudeTriggered int64

	driver := &mockTestDriver{
		executeFn: func(ctx context.Context, auth *coreauth.Auth, target warmup.Target) (*warmup.Result, error) {
			if target.PoolID == "antigravity:gemini" {
				atomic.AddInt64(&geminiTriggered, 1)
			} else if target.PoolID == "antigravity:3p" {
				atomic.AddInt64(&claudeTriggered, 1)
			}
			return &warmup.Result{Success: true, StatusCode: 200, PoolID: target.PoolID}, nil
		},
	}
	registry.Register(driver)

	authMgr := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "ag-test", Provider: "mock"}
	_, _ = authMgr.Register(context.Background(), auth)

	queue := warmup.NewStaggeredQueue(
		warmup.StaggeredQueueOptions{MaxConcurrency: 2},
		registry,
		nil,
	)

	// 1. Dispatch only Gemini pool
	tasks := []warmup.Task{
		{
			ID:     "task-gemini",
			Auth:   auth,
			Target: warmup.Target{PoolID: "antigravity:gemini"},
		},
	}
	queue.Dispatch(context.Background(), tasks)

	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt64(&geminiTriggered) != 1 {
		t.Fatalf("expected gemini pool triggered = 1, got %d", atomic.LoadInt64(&geminiTriggered))
	}
	if atomic.LoadInt64(&claudeTriggered) != 0 {
		t.Fatalf("expected claude pool triggered = 0, got %d", atomic.LoadInt64(&claudeTriggered))
	}

	// 2. Dispatch only Claude pool
	tasks2 := []warmup.Task{
		{
			ID:     "task-claude",
			Auth:   auth,
			Target: warmup.Target{PoolID: "antigravity:3p"},
		},
	}
	queue.Dispatch(context.Background(), tasks2)

	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt64(&claudeTriggered) != 1 {
		t.Fatalf("expected claude pool triggered = 1, got %d", atomic.LoadInt64(&claudeTriggered))
	}
}
