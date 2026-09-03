package warmup_test

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestBatchWarmupAndPolicyExclusions(t *testing.T) {
	registry := warmup.NewDriverRegistry()
	mockD := &mockTestDriver{
		executeFn: func(ctx context.Context, auth *coreauth.Auth, target warmup.Target) (*warmup.Result, error) {
			return &warmup.Result{
				AuthID:     auth.ID,
				PoolID:     target.PoolID,
				Success:    true,
				StatusCode: 200,
			}, nil
		},
	}
	registry.Register(mockD)

	authMgr := coreauth.NewManager(nil, nil, nil)
	auth1 := &coreauth.Auth{ID: "acc-1", Provider: "mock", FileName: "acc-1.json"}
	auth2 := &coreauth.Auth{ID: "acc-2", Provider: "mock", FileName: "acc-2.json"}
	auth3 := &coreauth.Auth{ID: "acc-3", Provider: "mock", FileName: "acc-3.json"}

	_, _ = authMgr.Register(context.Background(), auth1)
	_, _ = authMgr.Register(context.Background(), auth2)
	_, _ = authMgr.Register(context.Background(), auth3)

	queue := warmup.NewStaggeredQueue(
		warmup.StaggeredQueueOptions{
			MaxConcurrency: 2,
			MinJitter:      1 * time.Millisecond,
			MaxJitter:      2 * time.Millisecond,
		},
		registry,
		nil,
	)

	// 1. Test Policy with Account Inclusions & Exclusions
	now := time.Now()
	timeMock := func() time.Time { return now }

	scheduler := warmup.NewPolicyScheduler(queue, registry, authMgr)
	scheduler.SetNowFunc(timeMock)

	// Policy 1: Only includes acc-1 and acc-3
	pInclusion := warmup.Policy{
		ID:         "policy-inclusion",
		TenantID:   "t1",
		Enabled:    true,
		Providers:  []string{"mock"},
		AccountIDs: []string{"acc-1", "acc-3"},
		DailyWindow: warmup.DailyTimeWindow{
			Enabled: false,
		},
		IntervalSeconds: 3600,
	}
	scheduler.AddPolicy(pInclusion)
	scheduler.EvaluateTick(context.Background())

	policies := scheduler.GetPolicies("t1")
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if policies[0].TotalRuns != 1 {
		t.Fatalf("expected 1 run, got %d", policies[0].TotalRuns)
	}

	// Policy 2: Excludes acc-2
	pExclusion := warmup.Policy{
		ID:              "policy-exclusion",
		TenantID:        "t1",
		Enabled:         true,
		Providers:       []string{"mock"},
		ExcludedAuthIDs: []string{"acc-2"},
		DailyWindow: warmup.DailyTimeWindow{
			Enabled: false,
		},
		IntervalSeconds: 3600,
	}
	scheduler.AddPolicy(pExclusion)
	scheduler.EvaluateTick(context.Background())
}
