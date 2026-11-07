package warmup_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func registerAuths(t *testing.T, mgr *coreauth.Manager, auths ...*coreauth.Auth) {
	t.Helper()
	for _, auth := range auths {
		if _, err := mgr.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
}

// A tenant may warm only the accounts it owns: another tenant's account (or a
// platform account shared with it) answers as if it did not exist, and nothing
// is sent on it.
func TestWarmupRefusesAccountsOfAnotherTenant(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	own := &coreauth.Auth{ID: "own-codex", Provider: "codex", FileName: "own-codex.json", TenantID: "tenant-a"}
	other := &coreauth.Auth{ID: "other-codex", Provider: "codex", FileName: "other-codex.json", TenantID: "tenant-b"}
	platform := &coreauth.Auth{ID: "platform-codex", Provider: "codex", FileName: "platform-codex.json"}
	registerAuths(t, mgr, own, other, platform)
	svc := warmup.NewService(&config.Config{}, mgr)

	if _, err := svc.GetAccountTargets("tenant-a", own.ID); errors.Is(err, warmup.ErrAuthNotFound) {
		t.Fatalf("own account refused: %v", err)
	}
	for _, id := range []string{other.ID, platform.ID, "missing"} {
		if _, err := svc.GetAccountTargets("tenant-a", id); !errors.Is(err, warmup.ErrAuthNotFound) {
			t.Fatalf("targets of %s: err = %v, want ErrAuthNotFound", id, err)
		}
		if _, err := svc.WarmupSingleAccount(context.Background(), "tenant-a", id, ""); !errors.Is(err, warmup.ErrAuthNotFound) {
			t.Fatalf("warmup of %s: err = %v, want ErrAuthNotFound", id, err)
		}
	}
	count, err := svc.WarmupBatchAccounts(context.Background(), "tenant-a", []string{other.ID, platform.ID}, "")
	if err != nil || count != 0 {
		t.Fatalf("batch of foreign accounts dispatched %d (err %v), want 0", count, err)
	}
	// The platform still warms its own accounts.
	if _, err := svc.GetAccountTargets("", platform.ID); errors.Is(err, warmup.ErrAuthNotFound) {
		t.Fatalf("platform account refused to the platform: %v", err)
	}
}

// A policy warms the accounts of its own tenant only, even with no account
// filter: it used to walk every tenant's credentials.
func TestWarmupPolicyStaysInItsTenant(t *testing.T) {
	var mu sync.Mutex
	warmed := make(map[string]int)
	registry := warmup.NewDriverRegistry()
	registry.Register(&mockTestDriver{executeFn: func(_ context.Context, auth *coreauth.Auth, target warmup.Target) (*warmup.Result, error) {
		mu.Lock()
		warmed[auth.ID]++
		mu.Unlock()
		return &warmup.Result{AuthID: auth.ID, PoolID: target.PoolID, Success: true, StatusCode: 200}, nil
	}})
	mgr := coreauth.NewManager(nil, nil, nil)
	registerAuths(t, mgr,
		&coreauth.Auth{ID: "a-1", Provider: "mock", FileName: "a-1.json", TenantID: "tenant-a"},
		&coreauth.Auth{ID: "b-1", Provider: "mock", FileName: "b-1.json", TenantID: "tenant-b"},
		&coreauth.Auth{ID: "sys-1", Provider: "mock", FileName: "sys-1.json"},
	)
	queue := warmup.NewStaggeredQueue(warmup.StaggeredQueueOptions{MaxConcurrency: 2, MinJitter: time.Millisecond, MaxJitter: 2 * time.Millisecond}, registry, nil)
	scheduler := warmup.NewPolicyScheduler(queue, registry, mgr)
	scheduler.AddPolicy(warmup.Policy{ID: "all", TenantID: "tenant-a", Enabled: true, Providers: []string{"mock"}, IntervalSeconds: 3600, StaggerMinutes: 0})
	scheduler.EvaluateTick(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := warmed["a-1"] > 0
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if warmed["a-1"] == 0 {
		t.Fatalf("the policy's own account was not warmed: %v", warmed)
	}
	if warmed["b-1"] != 0 || warmed["sys-1"] != 0 {
		t.Fatalf("the policy warmed accounts of other tenants: %v", warmed)
	}
}
