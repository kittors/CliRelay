package aiaccountstatus

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	_ "modernc.org/sqlite"
)

func newTestManager(t *testing.T, tenant string, auths ...*coreauth.Auth) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		auth.TenantID = tenant
		if auth.FileName == "" && auth.ID != "" {
			auth.FileName = auth.ID + ".json"
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	return manager
}

func registeredIndexes(t *testing.T, manager *coreauth.Manager, tenant string) []string {
	t.Helper()
	auths := manager.ListForTenant(tenant)
	if len(auths) == 0 {
		t.Fatal("no auths registered for tenant")
	}
	out := make([]string, 0, len(auths))
	for _, a := range auths {
		out = append(out, a.EnsureIndex())
	}
	return out
}

func TestStartRefreshDedupeNeverBypassedByForce(t *testing.T) {
	authA := &coreauth.Auth{ID: "id-a", Provider: "codex", FileName: "a.json", Metadata: map[string]any{"account_id": "acct-a"}}
	manager := newTestManager(t, "tenant-1", authA)
	indexes := registeredIndexes(t, manager, "tenant-1")
	var probes atomic.Int32
	block := make(chan struct{})
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-1", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(ctx context.Context, _ *managementapitools.Service, _ *config.Config, _ *coreauth.Auth) (ProbeResult, error) {
		probes.Add(1)
		select {
		case <-block:
		case <-ctx.Done():
		}
		return ProbeResult{Quotas: []usage.QuotaWindowDTO{}}, nil
	})
	svc.SetMaxConcurrency(1)

	first := svc.StartRefresh("tenant-1", RefreshRequest{AuthIndexes: indexes[:1], Force: true})
	if first.Accepted != 1 {
		t.Fatalf("accepted=%d indexes=%v", first.Accepted, indexes)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && probes.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	second := svc.StartRefresh("tenant-1", RefreshRequest{AuthIndexes: indexes[:1], Force: true})
	if second.Deduplicated != 1 {
		t.Fatalf("force must still dedupe in-flight, deduplicated=%d accepted=%d", second.Deduplicated, second.Accepted)
	}
	if second.Accepted != 0 {
		t.Fatalf("accepted should be 0, got %d", second.Accepted)
	}
	close(block)
	waitJob(t, svc, "tenant-1", first.JobID)
}

func TestStartRefreshFailureIsolationAndResult(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	authA := &coreauth.Auth{ID: "id-a", Provider: "codex", FileName: "a.json", Metadata: map[string]any{"account_id": "a"}}
	authB := &coreauth.Auth{ID: "id-b", Provider: "codex", FileName: "b.json", Metadata: map[string]any{"account_id": "b"}}
	manager := newTestManager(t, "tenant-1", authA, authB)
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-1", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(_ context.Context, _ *managementapitools.Service, _ *config.Config, auth *coreauth.Auth) (ProbeResult, error) {
		if auth.ID == "id-a" || auth.FileName == "a.json" {
			return ProbeResult{}, context.DeadlineExceeded
		}
		pct := 80.0
		return ProbeResult{Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}, PlanType: "plus"}, nil
	})
	accepted := svc.StartRefresh("tenant-1", RefreshRequest{Force: true})
	if accepted.Accepted != 2 {
		t.Fatalf("accepted=%d", accepted.Accepted)
	}
	snap := waitJob(t, svc, "tenant-1", accepted.JobID)
	if snap.Failed < 1 || snap.Completed != snap.Total {
		t.Fatalf("snap=%+v", snap)
	}
	var sawSuccess, sawFail bool
	for _, r := range snap.Results {
		if r.State == RefreshSuccess {
			sawSuccess = true
			if r.Result == nil {
				t.Fatalf("success result missing status view")
			}
			if r.Result.Version < 1 {
				t.Fatalf("success progressive version=%d want DB-assigned >=1", r.Result.Version)
			}
		}
		if r.State == RefreshError {
			sawFail = true
		}
	}
	if !sawSuccess || !sawFail {
		t.Fatalf("expected mixed results: %+v", snap.Results)
	}
}

func TestGlobalSemaphoreBoundsAcrossJobs(t *testing.T) {
	authA := &coreauth.Auth{ID: "id-a", Provider: "codex", FileName: "a.json", Metadata: map[string]any{"account_id": "a"}}
	authB := &coreauth.Auth{ID: "id-b", Provider: "codex", FileName: "b.json", Metadata: map[string]any{"account_id": "b"}}
	manager := newTestManager(t, "tenant-1", authA, authB)
	indexes := registeredIndexes(t, manager, "tenant-1")
	var current, peak atomic.Int32
	gate := make(chan struct{})
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-1", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetMaxConcurrency(1)
	svc.SetProbeFunc(func(ctx context.Context, _ *managementapitools.Service, _ *config.Config, _ *coreauth.Auth) (ProbeResult, error) {
		n := current.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		current.Add(-1)
		return ProbeResult{}, nil
	})
	j1 := svc.StartRefresh("tenant-1", RefreshRequest{AuthIndexes: []string{indexes[0]}, Force: true})
	j2 := svc.StartRefresh("tenant-1", RefreshRequest{AuthIndexes: []string{indexes[1]}, Force: true})
	time.Sleep(50 * time.Millisecond)
	if peak.Load() > 1 {
		t.Fatalf("peak concurrency %d > 1", peak.Load())
	}
	close(gate)
	waitJob(t, svc, "tenant-1", j1.JobID)
	waitJob(t, svc, "tenant-1", j2.JobID)
}

func TestGetJobTenantIsolation(t *testing.T) {
	authA := &coreauth.Auth{ID: "id-a", Provider: "codex", FileName: "a.json", Metadata: map[string]any{"account_id": "a"}}
	manager := newTestManager(t, "tenant-1", authA)
	svc := New(&config.Config{}, manager, nil, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		return ProbeResult{}, nil
	})
	acc := svc.StartRefresh("tenant-1", RefreshRequest{Force: true})
	if _, ok := svc.GetJob("other", acc.JobID); ok {
		t.Fatal("cross-tenant job leak")
	}
}

func waitJob(t *testing.T, svc *Service, tenant, jobID string) JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := svc.GetJob(tenant, jobID)
		if ok && snap.State == "completed" {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s not completed", jobID)
	return JobSnapshot{}
}

func TestStartRefreshWithNoMatchingAccountsCompletesImmediately(t *testing.T) {
	manager := newTestManager(t, "tenant-1")
	svc := New(&config.Config{}, manager, nil, nil)
	accepted := svc.StartRefresh("tenant-1", RefreshRequest{AuthIndexes: []string{"missing"}, Force: true})
	if accepted.Accepted != 0 || accepted.Deduplicated != 0 {
		t.Fatalf("accepted=%+v", accepted)
	}
	snap, ok := svc.GetJob("tenant-1", accepted.JobID)
	if !ok || snap.State != "completed" || snap.Total != 0 || snap.Completed != 0 || snap.Failed != 0 {
		t.Fatalf("snapshot=%+v ok=%v", snap, ok)
	}
}

func TestListStatusDoesNotReadRequestLogs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
	})

	auth := &coreauth.Auth{ID: "id-read-model", Provider: "codex", FileName: "read-model.json", Metadata: map[string]any{"account_id": "acct-read-model"}}
	manager := newTestManager(t, "tenant-read-model", auth)
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		t.Fatal("missing auth subject identity")
	}
	pct := 65.0
	now := time.Now().UTC()
	if err := usage.UpsertAIAccountSubject(identity); err != nil {
		t.Fatal(err)
	}
	if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
		AuthSubjectID: identity.ID, Provider: "codex", LastProbeState: string(RefreshSuccess),
		PlanType: "plus", Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// If the hot path regresses to request_logs, this rename makes ListStatus fail.
	adminDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Close()
	if _, err := adminDB.Exec(`ALTER TABLE request_logs RENAME TO request_logs_hidden`); err != nil {
		t.Fatal(err)
	}

	svc := New(&config.Config{}, manager, nil, nil)
	response, err := svc.ListStatus("tenant-read-model", nil, nil)
	if err != nil {
		t.Fatalf("ListStatus touched request_logs: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].PlanType != "plus" || len(response.Items[0].Quotas) != 1 {
		t.Fatalf("response=%+v", response)
	}
}

func TestStartRefreshSkipsDisabledAccountWithoutProbe(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "id-disabled", Provider: "codex", FileName: "disabled.json",
		Status: coreauth.StatusDisabled, Disabled: true,
		Metadata: map[string]any{"account_id": "acct-disabled"},
	}
	manager := newTestManager(t, "tenant-1", auth)
	var probes atomic.Int32
	svc := New(&config.Config{}, manager, nil, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		probes.Add(1)
		return ProbeResult{}, nil
	})
	accepted := svc.StartRefresh("tenant-1", RefreshRequest{Force: true})
	if accepted.Accepted != 0 || len(accepted.Skipped) != 1 {
		t.Fatalf("accepted=%+v", accepted)
	}
	if probes.Load() != 0 {
		t.Fatalf("disabled account was probed %d times", probes.Load())
	}
	snap, ok := svc.GetJob("tenant-1", accepted.JobID)
	if !ok || snap.State != "completed" || snap.Total != 1 || snap.Completed != 1 || snap.Failed != 0 {
		t.Fatalf("snapshot=%+v ok=%v", snap, ok)
	}
}

func TestPreferEnabledAliasOverDisabledSameSubject(t *testing.T) {
	// Same upstream account_id => same auth_subject_id; disabled alias registered first.
	disabled := &coreauth.Auth{
		ID: "id-dis", Provider: "codex", FileName: "dis.json",
		Status: coreauth.StatusDisabled, Disabled: true,
		Metadata: map[string]any{"account_id": "shared-acct"},
	}
	enabled := &coreauth.Auth{
		ID: "id-en", Provider: "codex", FileName: "en.json",
		Status: coreauth.StatusActive, Disabled: false,
		Metadata: map[string]any{"account_id": "shared-acct"},
	}
	manager := newTestManager(t, "tenant-alias", disabled, enabled)
	var probedID atomic.Value
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-alias", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(_ context.Context, _ *managementapitools.Service, _ *config.Config, auth *coreauth.Auth) (ProbeResult, error) {
		probedID.Store(auth.ID)
		pct := 90.0
		return ProbeResult{Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}}, nil
	})
	acc := svc.StartRefresh("tenant-alias", RefreshRequest{Force: true})
	if acc.Accepted != 1 {
		t.Fatalf("accepted=%d want 1 (one subject, enabled preferred)", acc.Accepted)
	}
	waitJob(t, svc, "tenant-alias", acc.JobID)
	if got, _ := probedID.Load().(string); got != "id-en" {
		t.Fatalf("probed auth=%q want id-en", got)
	}

	// ListStatus representative must also prefer enabled.
	resp, err := svc.ListStatus("tenant-alias", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d", len(resp.Items))
	}
	// Auth index should match enabled credential after EnsureIndex.
	enabledIdx := ""
	for _, a := range manager.ListForTenant("tenant-alias") {
		if a.ID == "id-en" {
			enabledIdx = a.Index
		}
	}
	if resp.Items[0].AuthIndex != enabledIdx {
		t.Fatalf("list auth_index=%q want enabled %q", resp.Items[0].AuthIndex, enabledIdx)
	}
}

func TestPersistFailureDoesNotReportSuccess(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "id-persist", Provider: "codex", FileName: "p.json",
		Metadata: map[string]any{"account_id": "acct-persist"},
	}
	manager := newTestManager(t, "tenant-persist", auth)
	// Seed a prior successful snapshot that must be retained on persist failure.
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
	})
	identity := usage.ResolveAuthSubjectIdentity(auth)
	oldPct := 55.0
	now := time.Now().UTC()
	if err := usage.UpsertAIAccountStatus(usage.AIAccountStatusRecord{
		TenantID: "tenant-persist", AuthSubjectID: identity.ID, AuthIndex: auth.EnsureIndex(), Provider: "codex",
		RefreshState: "success", PlanType: "plus",
		Quotas:            []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &oldPct}},
		UpstreamCheckedAt: &now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-persist", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		pct := 10.0
		return ProbeResult{PlanType: "team", Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}}, nil
	})
	svc.SetUpsertStatusFunc(func(usage.AIAccountStatusRecord) error {
		return context.DeadlineExceeded
	})

	acc := svc.StartRefresh("tenant-persist", RefreshRequest{Force: true})
	snap := waitJob(t, svc, "tenant-persist", acc.JobID)
	if snap.Failed != 1 {
		t.Fatalf("failed=%d snap=%+v", snap.Failed, snap)
	}
	var saw bool
	for _, r := range snap.Results {
		if r.State == RefreshError && r.ErrorCode == "persist_failed" {
			saw = true
			if r.Result != nil {
				t.Fatal("must not attach fabricated success result on persist failure")
			}
		}
		if r.State == RefreshSuccess {
			t.Fatalf("must not report success when persist failed: %+v", r)
		}
	}
	if !saw {
		t.Fatalf("results=%+v", snap.Results)
	}

	// Previous successful quotas/plan must still be readable.
	rows, err := usage.ListAIAccountStatusForTenant("tenant-persist", []string{identity.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].PlanType != "plus" || len(rows[0].Quotas) != 1 || rows[0].Quotas[0].Percent == nil || *rows[0].Quotas[0].Percent != 55 {
		t.Fatalf("prior snapshot not retained: %+v", rows[0])
	}
	if rows[0].RefreshState != "error" || rows[0].ErrorCode != "persist_failed" {
		t.Fatalf("refresh failure not recorded: %+v", rows[0])
	}
}

func TestProgressiveSuccessResultUsesDBVersion(t *testing.T) {
	// Frontend monotonic merge rejects result.version <= current; progressive
	// payload must carry the real post-upsert DB version, not the in-memory draft (0).
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	auth := &coreauth.Auth{
		ID: "id-ver", Provider: "codex", FileName: "ver.json",
		Metadata: map[string]any{"account_id": "acct-ver"},
	}
	manager := newTestManager(t, "tenant-ver", auth)
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		t.Fatal("missing identity")
	}
	if err := usage.UpsertAIAccountSubject(identity); err != nil {
		t.Fatal(err)
	}
	// Bump shared DB version well above 0.
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		pct := float64(10 + i)
		if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
			AuthSubjectID: identity.ID, Provider: "codex", LastProbeState: string(RefreshSuccess), PlanType: "plus",
			Quotas:            []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}},
			UpstreamCheckedAt: &now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := usage.ListAIAccountSubjectStatus([]string{identity.ID})
	if err != nil || len(before) != 1 {
		t.Fatalf("before=%v err=%v", before, err)
	}
	if before[0].Version < 2 {
		t.Fatalf("seed version=%d want >=2", before[0].Version)
	}

	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-ver", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		pct := 77.0
		return ProbeResult{PlanType: "team", Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}}, nil
	})
	acc := svc.StartRefresh("tenant-ver", RefreshRequest{Force: true})
	snap := waitJob(t, svc, "tenant-ver", acc.JobID)
	var progressive *AccountStatusView
	for _, r := range snap.Results {
		if r.State == RefreshSuccess {
			progressive = r.Result
		}
	}
	if progressive == nil {
		t.Fatalf("missing progressive result: %+v", snap.Results)
	}
	after, err := usage.ListAIAccountSubjectStatus([]string{identity.ID})
	if err != nil || len(after) != 1 {
		t.Fatalf("after=%v err=%v", after, err)
	}
	if progressive.Version != after[0].Version {
		t.Fatalf("progressive version=%d db=%d (must match for frontend merge)", progressive.Version, after[0].Version)
	}
	if progressive.Version <= before[0].Version {
		t.Fatalf("progressive version=%d not newer than before=%d", progressive.Version, before[0].Version)
	}
	if progressive.PlanType != "team" {
		t.Fatalf("plan=%q", progressive.PlanType)
	}
}

func TestNormalRefreshDoesNotReconcileWithoutActiveQuotaCooldown(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "id-normal", Provider: "codex", FileName: "n.json",
		Metadata: map[string]any{"account_id": "acct-normal"},
	}
	manager := newTestManager(t, "tenant-reconcile", auth)
	var probes, reconciles atomic.Int32
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-reconcile", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		probes.Add(1)
		return ProbeResult{Quotas: []usage.QuotaWindowDTO{}}, nil
	})
	svc.SetReconcileQuotaFunc(func(context.Context, string) (bool, error) {
		reconciles.Add(1)
		return false, nil
	})
	acc := svc.StartRefresh("tenant-reconcile", RefreshRequest{Force: true})
	waitJob(t, svc, "tenant-reconcile", acc.JobID)
	if probes.Load() != 1 {
		t.Fatalf("probes=%d want 1", probes.Load())
	}
	if reconciles.Load() != 0 {
		t.Fatalf("reconcile=%d want 0 for normal account (no double upstream)", reconciles.Load())
	}
}

func TestActiveQuotaCooldownTriggersReconcile(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "id-cool", Provider: "codex", FileName: "cool.json",
		Unavailable: true,
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			NextRecoverAt: time.Now().Add(time.Hour),
		},
		NextRetryAfter: time.Now().Add(time.Hour),
		Metadata:       map[string]any{"account_id": "acct-cool"},
	}
	manager := newTestManager(t, "tenant-cool", auth)
	var probes, reconciles atomic.Int32
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-cool", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		probes.Add(1)
		return ProbeResult{}, nil
	})
	svc.SetReconcileQuotaFunc(func(context.Context, string) (bool, error) {
		reconciles.Add(1)
		return true, nil
	})
	acc := svc.StartRefresh("tenant-cool", RefreshRequest{Force: true})
	waitJob(t, svc, "tenant-cool", acc.JobID)
	if probes.Load() != 1 {
		t.Fatalf("probes=%d", probes.Load())
	}
	if reconciles.Load() != 1 {
		t.Fatalf("reconcile=%d want 1 when active quota cooldown", reconciles.Load())
	}
}

func TestListStatusThrottlesStaleNormalization(t *testing.T) {
	auth := &coreauth.Auth{ID: "id-stale", Provider: "codex", FileName: "s.json", Metadata: map[string]any{"account_id": "s"}}
	manager := newTestManager(t, "tenant-stale", auth)
	var norms atomic.Int32
	svc := New(&config.Config{}, manager, nil, nil)
	svc.SetNormalizeStaleFunc(func(string, time.Duration) (int64, error) {
		norms.Add(1)
		return 0, nil
	})
	for i := 0; i < 5; i++ {
		if _, err := svc.ListStatus("tenant-stale", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if norms.Load() != 1 {
		t.Fatalf("normalize calls=%d want 1 across consecutive ListStatus", norms.Load())
	}
}

func TestNeedsRuntimeQuotaReconcile(t *testing.T) {
	if needsRuntimeQuotaReconcile(nil) {
		t.Fatal("nil")
	}
	normal := &coreauth.Auth{ID: "a", Provider: "codex"}
	if needsRuntimeQuotaReconcile(normal) {
		t.Fatal("normal should not reconcile")
	}
	cool := &coreauth.Auth{
		ID: "b", Unavailable: true,
		Quota:          coreauth.QuotaState{Exceeded: true},
		NextRetryAfter: time.Now().Add(time.Minute),
	}
	if !needsRuntimeQuotaReconcile(cool) {
		t.Fatal("cooldown should reconcile")
	}
}

func TestApplyRuntimeQuotaProbe_XAIWeeklyZeroBlocksUntilRecovered(t *testing.T) {
	t.Parallel()

	auth := &coreauth.Auth{
		ID:       "xai-weekly-zero",
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "xai", "sub": "xai-user"},
	}
	manager := newTestManager(t, "tenant-xai", auth)
	svc := New(&config.Config{}, manager, nil, nil)
	zero := 0.0
	svc.applyRuntimeQuotaProbe(context.Background(), auth, ProbeResult{Quotas: []usage.QuotaWindowDTO{{
		QuotaKey: "weekly_limit",
		Percent:  &zero,
	}}})

	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("updated auth missing")
	}
	if !got.Quota.Exceeded || !got.Quota.RecoveryRequired || got.Quota.Window != "week" {
		t.Fatalf("quota = %#v, want weekly confirmed-recovery gate", got.Quota)
	}
	selector := &coreauth.RoundRobinSelector{}
	if picked, err := selector.Pick(context.Background(), "xai", "grok-4.5", cliproxyexecutor.Options{}, []*coreauth.Auth{got}); err == nil || picked != nil {
		t.Fatalf("Pick() = %#v, %v; want blocked auth", picked, err)
	}

	remaining := 25.0
	svc.applyRuntimeQuotaProbe(context.Background(), auth, ProbeResult{Quotas: []usage.QuotaWindowDTO{{
		QuotaKey: "weekly_limit",
		Percent:  &remaining,
	}}})
	got, _ = manager.GetByID(auth.ID)
	if got.Quota.Exceeded || got.Quota.RecoveryRequired {
		t.Fatalf("quota = %#v, want recovered", got.Quota)
	}
	if _, exists := got.Metadata["_cli_proxy_runtime_quota"]; exists {
		t.Fatal("recovered auth retained persisted quota runtime metadata")
	}
	if picked, err := selector.Pick(context.Background(), "xai", "grok-4.5", cliproxyexecutor.Options{}, []*coreauth.Auth{got}); err != nil || picked == nil {
		t.Fatalf("Pick() = %#v, %v; want recovered auth", picked, err)
	}
}

func TestFreshSkipUsesBatchLoadNotPerAccountUnderLock(t *testing.T) {
	// Multiple subjects recently successful: StartRefresh force=false should skip all
	// without probing, using one batch status load.
	a1 := &coreauth.Auth{ID: "f1", Provider: "codex", FileName: "f1.json", Metadata: map[string]any{"account_id": "f1"}}
	a2 := &coreauth.Auth{ID: "f2", Provider: "codex", FileName: "f2.json", Metadata: map[string]any{"account_id": "f2"}}
	manager := newTestManager(t, "tenant-fresh", a1, a2)
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })
	now := time.Now().UTC()
	for _, auth := range []*coreauth.Auth{a1, a2} {
		id := usage.ResolveAuthSubjectIdentity(auth)
		if err := usage.UpsertAIAccountSubject(id); err != nil {
			t.Fatal(err)
		}
		if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
			AuthSubjectID: id.ID, Provider: "codex", LastProbeState: string(RefreshSuccess), UpstreamCheckedAt: &now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var probes atomic.Int32
	svc := New(&config.Config{}, manager, nil, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		probes.Add(1)
		return ProbeResult{}, nil
	})
	acc := svc.StartRefresh("tenant-fresh", RefreshRequest{Force: false})
	if acc.Accepted != 0 || len(acc.Skipped) != 2 {
		t.Fatalf("accepted=%+v", acc)
	}
	if probes.Load() != 0 {
		t.Fatalf("probes=%d", probes.Load())
	}
}

func TestListStatusEmptyFilterReturnsEmptyWithoutFullScan(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	// Seed an unrelated status row for this tenant so a full-scan would find it.
	now := time.Now().UTC()
	if err := usage.UpsertAIAccountStatus(usage.AIAccountStatusRecord{
		TenantID: "tenant-empty", AuthSubjectID: "sub-other", AuthIndex: "other", Provider: "codex",
		RefreshState: "success", PlanType: "plus", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	manager := newTestManager(t, "tenant-empty") // no auths
	svc := New(&config.Config{}, manager, nil, nil)
	// Unknown auth_index filter with empty catalog.
	resp, err := svc.ListStatus("tenant-empty", []string{"missing-auth-index"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Items == nil || len(resp.Items) != 0 {
		t.Fatalf("want empty items, got %+v", resp.Items)
	}

	// Empty tenant with no filter must also return empty (no auths => no subjects).
	resp2, err := svc.ListStatus("tenant-empty", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp2.Items) != 0 {
		t.Fatalf("empty catalog must not list orphan status rows: %+v", resp2.Items)
	}
}

func TestForceFalseRespectsInMemorySuccessMinGap(t *testing.T) {
	// Prove lastSuccess closes TOCTOU when DB recent deliberately misses:
	// after first success, delete the status row so lock-external batch cannot
	// see UpstreamCheckedAt; force=false must still skip from memory.
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	auth := &coreauth.Auth{ID: "id-gap", Provider: "codex", FileName: "gap.json", Metadata: map[string]any{"account_id": "gap"}}
	manager := newTestManager(t, "tenant-gap", auth)
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		t.Fatal("missing identity")
	}
	var probes atomic.Int32
	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-gap", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		probes.Add(1)
		pct := 50.0
		return ProbeResult{Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}}, nil
	})

	first := svc.StartRefresh("tenant-gap", RefreshRequest{Force: true})
	waitJob(t, svc, "tenant-gap", first.JobID)
	if probes.Load() != 1 {
		t.Fatalf("probes after first=%d", probes.Load())
	}

	// Force DB recent miss without touching production APIs.
	admin, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`DELETE FROM ai_account_status WHERE tenant_id = ? AND auth_subject_id = ?`, "tenant-gap", identity.ID); err != nil {
		t.Fatalf("delete status row: %v", err)
	}
	if rows, err := usage.ListAIAccountStatusForTenant("tenant-gap", []string{identity.ID}); err != nil || len(rows) != 0 {
		t.Fatalf("expected empty status after delete, rows=%d err=%v", len(rows), err)
	}

	// Immediately re-request without force; only lastSuccess can skip.
	second := svc.StartRefresh("tenant-gap", RefreshRequest{Force: false})
	if second.Accepted != 0 {
		t.Fatalf("second accepted=%d want 0 (memory fresh skip)", second.Accepted)
	}
	if len(second.Skipped) != 1 {
		t.Fatalf("skipped=%v", second.Skipped)
	}
	if probes.Load() != 1 {
		t.Fatalf("probes after second=%d want 1 (no re-probe via lastSuccess)", probes.Load())
	}

	// force=true still probes even with lastSuccess present.
	third := svc.StartRefresh("tenant-gap", RefreshRequest{Force: true})
	waitJob(t, svc, "tenant-gap", third.JobID)
	if probes.Load() != 2 {
		t.Fatalf("probes after force=%d want 2", probes.Load())
	}
}

func TestPersistReloadFailedDoesNotReportSuccess(t *testing.T) {
	// No seed row: UpdateAIAccountRefreshState writes a shell during running, then
	// no-op upsert leaves that shell; delete it before reload would be races.
	// Minimal path: no-op upsert + empty DB after running state is deleted under lock
	// is hard. Instead: no-op upsert after ensuring load sees empty by deleting any
	// shell row from the no-op path via SetUpsertStatusFunc that only returns nil
	// and never writes — but running state already inserted a shell. So delete
	// that shell inside the no-op upsert before return; real load then gets no row.
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	auth := &coreauth.Auth{ID: "id-reload", Provider: "codex", FileName: "reload.json", Metadata: map[string]any{"account_id": "reload"}}
	manager := newTestManager(t, "tenant-reload", auth)
	identity := usage.ResolveAuthSubjectIdentity(auth)

	svc := New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-reload", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		pct := 11.0
		return ProbeResult{PlanType: "team", Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}}, nil
	})
	svc.SetUpsertStatusFunc(func(usage.AIAccountStatusRecord) error {
		// Claim success without writing the success payload. Clear shell rows so
		// the real loadPersistedStatus path observes "status row missing after upsert".
		admin, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			return err
		}
		defer admin.Close()
		_, _ = admin.Exec(`DELETE FROM ai_account_status WHERE tenant_id = ? AND auth_subject_id = ?`, "tenant-reload", identity.ID)
		return nil
	})

	acc := svc.StartRefresh("tenant-reload", RefreshRequest{Force: true})
	snap := waitJob(t, svc, "tenant-reload", acc.JobID)
	if snap.Failed != 1 {
		t.Fatalf("failed=%d snap=%+v", snap.Failed, snap)
	}
	for _, r := range snap.Results {
		if r.State == RefreshSuccess {
			t.Fatalf("must not report success on reload miss: %+v", r)
		}
		if r.ErrorCode != "persist_reload_failed" {
			t.Fatalf("error_code=%q want persist_reload_failed", r.ErrorCode)
		}
		if r.Result != nil {
			t.Fatalf("Result must be nil, got %+v", r.Result)
		}
	}
	shared, err := usage.ListAIAccountSubjectStatus([]string{identity.ID})
	if err != nil || len(shared) != 1 {
		t.Fatalf("shared status=%+v err=%v", shared, err)
	}
	if shared[0].LastProbeState != string(RefreshSuccess) || shared[0].PlanType != "team" || len(shared[0].Quotas) != 1 {
		t.Fatalf("legacy reload failure blocked shared status: %+v", shared[0])
	}
}

func TestListStatusNewTenantBindingReadsExistingSharedStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	manager := coreauth.NewManager(nil, nil, nil)
	authA := &coreauth.Auth{ID: "shared-a", TenantID: "tenant-a", Provider: "codex", FileName: "a.json", Metadata: map[string]any{"account_id": "physical-account"}}
	authB := &coreauth.Auth{ID: "shared-b", TenantID: "tenant-b", Provider: "codex", FileName: "b.json", Metadata: map[string]any{"account_id": "physical-account"}}
	for _, auth := range []*coreauth.Auth{authA, authB} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	identity := usage.ResolveAuthSubjectIdentity(authA)
	if identity == nil || identity.ID != usage.ResolveAuthSubjectIdentity(authB).ID {
		t.Fatal("expected shared account subject")
	}
	if err := usage.UpsertAIAccountTenantBinding(authA, identity); err != nil {
		t.Fatal(err)
	}
	checked := time.Now().UTC()
	pct := 68.0
	if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
		AuthSubjectID:     identity.ID,
		Provider:          "codex",
		LastProbeState:    string(RefreshSuccess),
		HealthStatus:      "ok",
		PlanType:          "plus",
		Quotas:            []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}},
		UpstreamCheckedAt: &checked,
		UpdatedAt:         checked,
	}); err != nil {
		t.Fatal(err)
	}
	resetAt := checked.Add(48 * time.Hour)
	cycleStart := resetAt.Add(-7 * 24 * time.Hour)
	if err := usage.RecordAIAccountSubjectQuotaPoints(identity.ID, "codex", []usage.QuotaSnapshotPoint{{
		RecordedAt: checked, Provider: "codex", QuotaKey: "code_week", QuotaLabel: "Week",
		Percent: &pct, ResetAt: &resetAt, WindowSeconds: 604800,
	}}); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`
		INSERT INTO ai_account_subject_usage_buckets
			(auth_subject_id, bucket_kind, bucket_start, request_count, success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at)
		VALUES (?, 'cycle', ?, 1, 1, 0, 1, 456, ?, ?)
	`, identity.ID, cycleStart.Format(time.RFC3339Nano), cycleStart.Format(time.RFC3339Nano), checked.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`
		INSERT INTO auth_subject_usage_daily
			(tenant_id, auth_subject_id, day_key, request_count, success_count, failure_count, cost_total, updated_at)
		VALUES (?, ?, ?, 9, 9, 0, 9, ?)
	`, authA.TenantID, identity.ID, cycleStart.Format("2006-01-02"), checked.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`
		INSERT INTO auth_subject_quota_cycles
			(tenant_id, subject_id, auth_index, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, ?, ?, 'codex', 'code_week', ?, ?, 604800, ?)
	`, authA.TenantID, identity.ID, authA.EnsureIndex(), cycleStart.Format(time.RFC3339Nano), resetAt.Format(time.RFC3339Nano), checked.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	// Mounting the same physical account in tenant B repairs only B's missing binding,
	// then immediately reads the already-populated shared subject snapshot.
	svc := New(&config.Config{}, manager, nil, nil)
	response, err := svc.ListStatus("tenant-b", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("items=%+v", response.Items)
	}
	item := response.Items[0]
	if item.AuthSubjectID != identity.ID || item.PlanType != "plus" || item.StatusScope != usage.AIAccountStatusScopeShared || item.SubjectScope != usage.AIAccountSubjectScopeShared || !item.ShareEligible {
		t.Fatalf("shared status=%+v", item)
	}
	if item.CurrentTenantBindingCount != 1 || item.AuthIndex != authB.EnsureIndex() {
		t.Fatalf("tenant B binding projection=%+v", item)
	}
	if !item.Usage.CycleKnown || item.Usage.CycleRequestTotal != 1 || item.Usage.CycleTotalTokens != 456 {
		t.Fatalf("binding repair changed shared cycle usage: %+v", item.Usage)
	}
}

func TestListStatusRejectsGuessedSubjectWithoutCurrentTenantBinding(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	authA := &coreauth.Auth{ID: "only-a", TenantID: "tenant-a", Provider: "codex", FileName: "a.json", Metadata: map[string]any{"account_id": "secret-account"}}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), authA); err != nil {
		t.Fatal(err)
	}
	identity := usage.ResolveAuthSubjectIdentity(authA)
	if err := usage.UpsertAIAccountTenantBinding(authA, identity); err != nil {
		t.Fatal(err)
	}
	if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
		AuthSubjectID: identity.ID, Provider: "codex", LastProbeState: string(RefreshSuccess), PlanType: "plus", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	response, err := New(&config.Config{}, manager, nil, nil).ListStatus("tenant-b", nil, []string{identity.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 0 {
		t.Fatalf("guessed subject leaked shared status: %+v", response.Items)
	}
}

func TestCrossTenantRefreshSingleflightDoesNotLeakForeignJobID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	manager := coreauth.NewManager(nil, nil, nil)
	authA := &coreauth.Auth{ID: "refresh-a", TenantID: "tenant-a", Provider: "codex", FileName: "a.json", Metadata: map[string]any{"account_id": "refresh-physical"}}
	authB := &coreauth.Auth{ID: "refresh-b", TenantID: "tenant-b", Provider: "codex", FileName: "b.json", Metadata: map[string]any{"account_id": "refresh-physical"}}
	for _, auth := range []*coreauth.Auth{authA, authB} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}

	var probes atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	svc := New(&config.Config{}, manager, func(tenantID string) *managementapitools.Service {
		return managementapitools.NewForTenant(tenantID, &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(ctx context.Context, _ *managementapitools.Service, _ *config.Config, _ *coreauth.Auth) (ProbeResult, error) {
		if probes.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ProbeResult{Quotas: []usage.QuotaWindowDTO{}}, nil
	})

	first := svc.StartRefresh("tenant-a", RefreshRequest{Force: true})
	if first.Accepted != 1 {
		t.Fatalf("first=%+v", first)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first probe did not start")
	}
	second := svc.StartRefresh("tenant-b", RefreshRequest{Force: true})
	if second.Accepted != 0 || second.Deduplicated != 1 {
		t.Fatalf("second=%+v", second)
	}
	secondJob, ok := svc.GetJob("tenant-b", second.JobID)
	if !ok || len(secondJob.Results) != 1 {
		t.Fatalf("tenant B job=%+v ok=%v", secondJob, ok)
	}
	result := secondJob.Results[0]
	if result.ErrorCode != "deduplicated" || result.ErrorMessage != "shared refresh already in progress" {
		t.Fatalf("dedupe result=%+v", result)
	}
	if first.JobID != "" && (result.ErrorMessage == first.JobID || result.ErrorCode == first.JobID) {
		t.Fatalf("foreign job ID leaked: first=%s result=%+v", first.JobID, result)
	}
	if _, ok := svc.GetJob("tenant-b", first.JobID); ok {
		t.Fatal("tenant B can read tenant A job")
	}
	close(release)
	waitJob(t, svc, "tenant-a", first.JobID)
	if probes.Load() != 1 {
		t.Fatalf("cross-tenant shared subject probed %d times", probes.Load())
	}
}
