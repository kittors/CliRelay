package aiaccountstatus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// The reason the scheduler exists: an account nobody is looking at still gets
// probed, so a Grok subscription drained by Grok Chat on the web stops reading
// as untouched until a human opens the panel.
func TestSchedulerProbesWithoutAPanelSession(t *testing.T) {
	auth := &coreauth.Auth{ID: "id-a", Provider: "xai", FileName: "a.json", Metadata: map[string]any{"account_id": "acct-a"}}
	manager := newTestManager(t, "tenant-1", auth)
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-1", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	var probes atomic.Int32
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		probes.Add(1)
		return ProbeResult{}, nil
	})

	scheduler := NewScheduler(func() *Service { return svc }, func() *coreauth.Manager { return manager }, 20*time.Millisecond, 0)
	scheduler.Start()
	defer scheduler.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if probes.Load() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scheduler never probed the account")
}

// Stop has to actually end the loop; a scheduler that keeps ticking after
// shutdown keeps calling upstream during drain.
func TestSchedulerStopEndsRounds(t *testing.T) {
	auth := &coreauth.Auth{ID: "id-a", Provider: "xai", FileName: "a.json", Metadata: map[string]any{"account_id": "acct-a"}}
	manager := newTestManager(t, "tenant-1", auth)
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-1", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	var rounds atomic.Int32
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		rounds.Add(1)
		return ProbeResult{}, nil
	})

	scheduler := NewScheduler(func() *Service { return svc }, func() *coreauth.Manager { return manager }, 10*time.Millisecond, 0)
	scheduler.Start()
	time.Sleep(60 * time.Millisecond)
	scheduler.Stop()

	settled := rounds.Load()
	time.Sleep(80 * time.Millisecond)
	// The per-account min-gap suppresses repeat probes, so the count may simply
	// stop climbing; what must not happen is it climbing after Stop.
	if grew := rounds.Load() - settled; grew > 1 {
		t.Fatalf("rounds grew by %d after Stop", grew)
	}
}

// Stopping something that never started, and starting twice, are both things a
// config reload does.
func TestSchedulerRestartAndStopWithoutStartAreSafe(t *testing.T) {
	manager := newTestManager(t, "tenant-1")
	svc := New(&config.Config{}, manager, nil, nil)

	scheduler := NewScheduler(func() *Service { return svc }, func() *coreauth.Manager { return manager }, time.Hour, time.Hour)
	scheduler.Stop()
	scheduler.Start()
	scheduler.Start()
	scheduler.Stop()
	scheduler.Stop()
}

// A tenant with no credentials must not cost a refresh job per round.
func TestTenantIDsWithAccountsDedupesAndSkipsEmpty(t *testing.T) {
	if got := tenantIDsWithAccounts(nil); got != nil {
		t.Fatalf("nil manager tenants=%v, want nil", got)
	}
	manager := newTestManager(t, "tenant-1",
		&coreauth.Auth{ID: "id-a", Provider: "xai", FileName: "a.json", Metadata: map[string]any{"account_id": "a"}},
		&coreauth.Auth{ID: "id-b", Provider: "codex", FileName: "b.json", Metadata: map[string]any{"account_id": "b"}},
	)
	tenants := tenantIDsWithAccounts(manager)
	if len(tenants) != 1 || tenants[0] != "tenant-1" {
		t.Fatalf("tenants=%v, want [tenant-1]", tenants)
	}
}
