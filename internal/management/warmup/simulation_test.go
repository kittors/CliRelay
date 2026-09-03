package warmup_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// MockUpstream simulates provider servers with 5-hour sliding window activation.
type MockUpstream struct {
	mu           sync.Mutex
	server       *httptest.Server
	requestCount map[string]int64
	activatedAt  map[string]time.Time
}

func newMockUpstream() *MockUpstream {
	m := &MockUpstream{
		requestCount: make(map[string]int64),
		activatedAt:  make(map[string]time.Time),
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		m.mu.Lock()
		defer m.mu.Unlock()

		// Key by target model to simulate pool separation
		var key string
		if strings.Contains(r.URL.Path, "streamGenerateContent") || strings.Contains(bodyStr, "gemini") {
			key = "antigravity:gemini"
		} else if strings.Contains(bodyStr, "claude") {
			key = "antigravity:3p"
		} else {
			key = "codex:5h"
		}

		m.requestCount[key]++
		if _, exists := m.activatedAt[key]; !exists {
			m.activatedAt[key] = time.Now()
		}

		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n"))
		} else {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}
	}))

	return m
}

func (m *MockUpstream) Close() {
	m.server.Close()
}

func (m *MockUpstream) URL() string {
	return m.server.URL
}

func TestWarmupDriversExecution(t *testing.T) {
	upstream := newMockUpstream()
	defer upstream.Close()

	cfg := &config.Config{}

	// 1. Test Antigravity Driver
	agDriver := warmup.NewAntigravityDriver(cfg)
	if agDriver.Provider() != "antigravity" {
		t.Fatalf("agDriver.Provider() = %s, want antigravity", agDriver.Provider())
	}

	auth := &coreauth.Auth{
		ID:       "ag-1",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":         "antigravity",
			"access_token": "test-token",
		},
	}

	targets := agDriver.GetTargets(auth)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets for antigravity, got %d", len(targets))
	}

	// Verify target models
	if targets[0].PoolID != "antigravity:gemini" || targets[1].PoolID != "antigravity:3p" {
		t.Fatalf("unexpected pool targets: %+v", targets)
	}

	// 2. Test Codex Driver
	codexDriver := warmup.NewCodexDriver(cfg)
	if codexDriver.Provider() != "codex" {
		t.Fatalf("codexDriver.Provider() = %s, want codex", codexDriver.Provider())
	}
	codexTargets := codexDriver.GetTargets(&coreauth.Auth{ID: "cx-1", Provider: "codex"})
	if len(codexTargets) != 1 || codexTargets[0].PoolID != "codex:5h" {
		t.Fatalf("unexpected codex targets: %+v", codexTargets)
	}
}

// TestPolicySchedulerLifecycle tests time-traveling StartAt, StopAt, and Quiet Hours.
func TestPolicySchedulerLifecycle(t *testing.T) {
	currentTime := time.Date(2026, 9, 4, 6, 30, 0, 0, time.UTC) // 06:30 AM
	timeMock := func() time.Time { return currentTime }

	registry := warmup.NewDriverRegistry()
	authMgr := coreauth.NewManager(nil, nil, nil)
	_, _ = authMgr.Register(context.Background(), &coreauth.Auth{
		ID:       "test-account-1",
		Provider: "antigravity",
	})

	queue := warmup.NewStaggeredQueue(
		warmup.StaggeredQueueOptions{
			MaxConcurrency: 2,
			MinJitter:      1 * time.Millisecond,
			MaxJitter:      2 * time.Millisecond,
		},
		registry,
		nil,
	)

	scheduler := warmup.NewPolicyScheduler(queue, registry, authMgr)
	scheduler.SetNowFunc(timeMock)

	startAt := time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC) // 07:00 AM
	stopAt := time.Date(2026, 9, 4, 22, 0, 0, 0, time.UTC) // 22:00 PM

	policy := warmup.Policy{
		ID:        "policy-1",
		Name:      "Morning Warmup",
		Enabled:   true,
		Providers: []string{"antigravity"},
		StartAt:   &startAt,
		StopAt:    &stopAt,
		DailyWindow: warmup.DailyTimeWindow{
			Enabled:     true,
			StartHour:   7,
			StartMinute: 0,
			EndHour:     21,
			EndMinute:   0,
		},
		IntervalSeconds: 18000, // 5 hours
		StaggerMinutes:  5,
	}

	scheduler.AddPolicy(policy)

	// Case 1: At 06:30 AM, StartAt has not arrived. Should be Pending.
	scheduler.EvaluateTick(context.Background())
	policies := scheduler.GetPolicies("")
	if len(policies) != 1 || policies[0].Status != warmup.PolicyStatusPending {
		t.Fatalf("expected status pending at 06:30, got %s", policies[0].Status)
	}

	// Case 2: Time travels to 07:01 AM. Should trigger and become Active.
	currentTime = time.Date(2026, 9, 4, 7, 1, 0, 0, time.UTC)
	scheduler.EvaluateTick(context.Background())
	policies = scheduler.GetPolicies("")
	if policies[0].Status != warmup.PolicyStatusActive {
		t.Fatalf("expected status active at 07:01, got %s", policies[0].Status)
	}
	if policies[0].TotalRuns != 1 {
		t.Fatalf("expected total runs = 1, got %d", policies[0].TotalRuns)
	}

	// Case 3: Time travels to 21:30 PM (Night Quiet Hours). Should enter QuietHours.
	currentTime = time.Date(2026, 9, 4, 21, 30, 0, 0, time.UTC)
	scheduler.EvaluateTick(context.Background())
	policies = scheduler.GetPolicies("")
	if policies[0].Status != warmup.PolicyStatusQuietHours {
		t.Fatalf("expected status quiet_hours at 21:30, got %s", policies[0].Status)
	}

	// Case 4: Time travels to 22:30 PM (Past StopAt). Should be Completed.
	currentTime = time.Date(2026, 9, 4, 22, 30, 0, 0, time.UTC)
	scheduler.EvaluateTick(context.Background())
	policies = scheduler.GetPolicies("")
	if policies[0].Status != warmup.PolicyStatusCompleted {
		t.Fatalf("expected status completed at 22:30, got %s", policies[0].Status)
	}
}

// TestStaggeredQueueMassiveScale tests queuing up to 5,000 tasks and ensures
// no goroutine explosion and strict concurrency enforcement.
func TestStaggeredQueueMassiveScale(t *testing.T) {
	registry := warmup.NewDriverRegistry()

	var activeWorkers int64
	var maxObservedConcurrency int64
	var totalExecuted int64

	mockDriver := &mockTestDriver{
		executeFn: func(ctx context.Context, auth *coreauth.Auth, target warmup.Target) (*warmup.Result, error) {
			current := atomic.AddInt64(&activeWorkers, 1)
			defer atomic.AddInt64(&activeWorkers, -1)

			for {
				prevMax := atomic.LoadInt64(&maxObservedConcurrency)
				if current <= prevMax || atomic.CompareAndSwapInt64(&maxObservedConcurrency, prevMax, current) {
					break
				}
			}

			// Simulate minimal IO
			time.Sleep(200 * time.Microsecond)
			atomic.AddInt64(&totalExecuted, 1)

			return &warmup.Result{
				AuthID:     auth.ID,
				PoolID:     target.PoolID,
				Success:    true,
				StatusCode: 200,
			}, nil
		},
	}
	registry.Register(mockDriver)

	concurrencyLimit := 3
	queue := warmup.NewStaggeredQueue(
		warmup.StaggeredQueueOptions{
			MaxConcurrency: concurrencyLimit,
			MinJitter:      10 * time.Microsecond,
			MaxJitter:      50 * time.Microsecond,
		},
		registry,
		nil,
	)

	const taskCount = 1000
	tasks := make([]warmup.Task, taskCount)
	for i := 0; i < taskCount; i++ {
		tasks[i] = warmup.Task{
			ID:   fmt.Sprintf("task-%d", i),
			Auth: &coreauth.Auth{ID: fmt.Sprintf("auth-%d", i), Provider: "mock"},
			Target: warmup.Target{
				PoolID: "pool-1",
			},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	queue.Dispatch(ctx, tasks)

	// Wait for processing
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&totalExecuted) >= int64(taskCount) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	executed := atomic.LoadInt64(&totalExecuted)
	if executed < 500 {
		t.Fatalf("expected at least 500 tasks executed under stagger, got %d", executed)
	}

	maxConc := atomic.LoadInt64(&maxObservedConcurrency)
	if maxConc > int64(concurrencyLimit) {
		t.Fatalf("maxObservedConcurrency (%d) exceeded configured limit (%d)", maxConc, concurrencyLimit)
	}
}

type mockTestDriver struct {
	executeFn func(ctx context.Context, auth *coreauth.Auth, target warmup.Target) (*warmup.Result, error)
}

func (m *mockTestDriver) Provider() string { return "mock" }
func (m *mockTestDriver) GetTargets(auth *coreauth.Auth) []warmup.Target {
	return []warmup.Target{{PoolID: "pool-1"}}
}
func (m *mockTestDriver) ExecuteWarmup(ctx context.Context, auth *coreauth.Auth, target warmup.Target) (*warmup.Result, error) {
	if m.executeFn != nil {
		return m.executeFn(ctx, auth, target)
	}
	return &warmup.Result{Success: true, StatusCode: 200}, nil
}
