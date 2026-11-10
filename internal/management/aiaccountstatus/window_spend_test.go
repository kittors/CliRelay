package aiaccountstatus

import (
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	_ "modernc.org/sqlite"
)

func initWindowSpendServiceDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })
	admin, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	return admin
}

// recordSpend writes the rollup buckets one request leaves behind, in every
// kind the window spend reads. The usage DB runs on UTC, so local keys are UTC.
func recordSpend(t *testing.T, admin *sql.DB, tenant, subject, model string, at time.Time, cost float64) {
	t.Helper()
	at = at.UTC()
	for kind, start := range map[string]string{
		"quota_minute_utc": at.Format("2006-01-02T15:04"),
		"hour":             at.Format("2006-01-02T15"),
		"day":              at.Format("2006-01-02"),
		"lifetime":         "1970-01-01",
	} {
		if _, err := admin.Exec(`
			INSERT INTO usage_rollup_buckets
				(tenant_id, bucket_kind, bucket_start, auth_subject_id, model, source, channel_name, request_count, cost_total, updated_at)
			VALUES (?, ?, ?, ?, ?, 'test', 'shared-label', 1, ?, ?)
			ON CONFLICT(tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id, model, source, channel_name)
			DO UPDATE SET request_count = request_count + 1, cost_total = cost_total + excluded.cost_total
		`, tenant, kind, start, subject, model, cost, at.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("record %s spend: %v", kind, err)
		}
	}
}

func bindAuths(t *testing.T, auths ...*coreauth.Auth) {
	t.Helper()
	for _, auth := range auths {
		if err := usage.UpsertAIAccountTenantBinding(auth, usage.ResolveAuthSubjectIdentity(auth)); err != nil {
			t.Fatal(err)
		}
	}
}

func upsertWindows(t *testing.T, subject, provider string, windows ...usage.QuotaWindowDTO) {
	t.Helper()
	checked := time.Now().UTC().Truncate(time.Second)
	if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
		AuthSubjectID: subject, Provider: provider, LastProbeState: string(RefreshSuccess), HealthStatus: "ok",
		Quotas: windows, UpstreamCheckedAt: &checked, UpdatedAt: checked,
	}); err != nil {
		t.Fatal(err)
	}
}

func quotaWindow(key string, windowSeconds int64, resetIn time.Duration) usage.QuotaWindowDTO {
	percent := 40.0
	window := usage.QuotaWindowDTO{QuotaKey: key, Percent: &percent, WindowSeconds: windowSeconds}
	if resetIn != 0 {
		reset := time.Now().UTC().Add(resetIn)
		window.ResetAt = &reset
	}
	return window
}

func spendByWindow(t *testing.T, item AccountStatusView) map[string]*float64 {
	t.Helper()
	out := make(map[string]*float64, len(item.Quotas))
	for _, window := range item.Quotas {
		out[window.QuotaKey] = window.UsedCostUSD
	}
	return out
}

func assertSpend(t *testing.T, label string, got map[string]*float64, key string, want float64) {
	t.Helper()
	value := got[key]
	if value == nil || math.Abs(*value-want) > 1e-6 {
		t.Fatalf("%s %s spend = %v, want %v", label, key, value, want)
	}
}

// The card pairs each window's share with what the proxy spent inside it
// (CliRelay#1103). One Codex account is mounted by two tenants: its windows
// count both tenants' requests and read the same in each, while a second
// account in each tenant — same model, same labels — keeps its own spend.
func TestListStatusReportsSpendInsideEachQuotaWindow(t *testing.T) {
	admin := initWindowSpendServiceDB(t)
	const tenantA, tenantB = "tenant-spend-a", "tenant-spend-b"
	sharedA := &coreauth.Auth{ID: "codex-shared-a", Provider: "codex", Metadata: map[string]any{"account_id": "acct-shared"}}
	sharedB := &coreauth.Auth{ID: "codex-shared-b", Provider: "codex", Metadata: map[string]any{"account_id": "acct-shared"}}
	ownA := &coreauth.Auth{ID: "kimi-a", Provider: "kimi", Metadata: map[string]any{"email": "a@example.com"}}
	ownB := &coreauth.Auth{ID: "kimi-b", Provider: "kimi", Metadata: map[string]any{"email": "b@example.com"}}
	managerA := newTestManager(t, tenantA, sharedA, ownA)
	managerB := newTestManager(t, tenantB, sharedB, ownB)
	bindAuths(t, sharedA, sharedB, ownA, ownB)

	shared := usage.ResolveAuthSubjectIdentity(sharedA)
	if shared == nil || shared.ID != usage.ResolveAuthSubjectIdentity(sharedB).ID {
		t.Fatal("the shared account resolved to two subjects")
	}
	ownSubjectA := usage.ResolveAuthSubjectIdentity(ownA).ID
	ownSubjectB := usage.ResolveAuthSubjectIdentity(ownB).ID

	upsertWindows(t, shared.ID, "codex",
		quotaWindow("code_5h", 18000, 3*time.Hour),                        // started 2h ago
		quotaWindow("code_week", 604800, 48*time.Hour),                    // started 5 days ago
		quotaWindow("review_5h", 18000, time.Hour),                        // code review: no model request draws on it
		quotaWindow("code_subscription_2592000", 2592000, 0),              // no renewal reported
		quotaWindow("additional:codex_bengalfox:5h", 18000, -time.Minute), // renewed since read
	)
	upsertWindows(t, ownSubjectA, "kimi", quotaWindow("code_5h", 18000, 3*time.Hour))
	upsertWindows(t, ownSubjectB, "kimi", quotaWindow("code_5h", 18000, 3*time.Hour))

	now := time.Now().UTC()
	recordSpend(t, admin, tenantA, shared.ID, "gpt-6.1-sol", now.Add(-30*time.Minute), 1.25)
	recordSpend(t, admin, tenantB, shared.ID, "gpt-6.1-sol", now.Add(-90*time.Minute), 2)
	recordSpend(t, admin, tenantA, shared.ID, "gpt-6.1-sol", now.Add(-3*time.Hour), 4) // before the 5h window
	recordSpend(t, admin, tenantB, shared.ID, "gpt-6.1-sol", now.Add(-6*24*time.Hour), 8)
	recordSpend(t, admin, tenantA, ownSubjectA, "gpt-6.1-sol", now.Add(-30*time.Minute), 0.4)
	recordSpend(t, admin, tenantB, ownSubjectB, "gpt-6.1-sol", now.Add(-30*time.Minute), 0.7)

	for _, view := range []struct {
		tenant  string
		manager *coreauth.Manager
		own     string
		ownCost float64
	}{{tenantA, managerA, ownSubjectA, 0.4}, {tenantB, managerB, ownSubjectB, 0.7}} {
		response, err := New(&config.Config{}, view.manager, nil, nil).ListStatus(view.tenant, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !response.Capabilities["quota-window-spend"] {
			t.Fatalf("%s: capabilities = %+v", view.tenant, response.Capabilities)
		}
		bySubject := make(map[string]AccountStatusView, len(response.Items))
		for _, item := range response.Items {
			bySubject[item.AuthSubjectID] = item
		}
		if len(bySubject) != 2 {
			t.Fatalf("%s: items = %+v", view.tenant, response.Items)
		}
		spend := spendByWindow(t, bySubject[shared.ID])
		assertSpend(t, view.tenant, spend, "code_5h", 1.25+2)
		assertSpend(t, view.tenant, spend, "code_week", 1.25+2+4)
		for _, key := range []string{"review_5h", "code_subscription_2592000", "additional:codex_bengalfox:5h"} {
			if spend[key] != nil {
				t.Fatalf("%s: %s spend = %v, want none", view.tenant, key, *spend[key])
			}
		}
		assertSpend(t, view.tenant, spendByWindow(t, bySubject[view.own]), "code_5h", view.ownCost)
	}
}

// Antigravity's summary buckets split an account in two: every Gemini model
// draws on the Gemini bucket and the Claude/GPT models on the third-party one,
// so each bucket's spend counts only its own models.
func TestListStatusSplitsAntigravitySpendByBucket(t *testing.T) {
	admin := initWindowSpendServiceDB(t)
	const tenant = "tenant-spend-agy"
	auth := &coreauth.Auth{ID: "agy-1", Provider: "antigravity", Metadata: map[string]any{"email": "agy@example.com"}}
	manager := newTestManager(t, tenant, auth)
	bindAuths(t, auth)
	subject := usage.ResolveAuthSubjectIdentity(auth).ID
	upsertWindows(t, subject, "antigravity",
		quotaWindow("antigravity:gemini_5h", 18000, 3*time.Hour),
		quotaWindow("antigravity:3p_5h", 18000, 3*time.Hour),
	)
	now := time.Now().UTC()
	recordSpend(t, admin, tenant, subject, "gemini-3.1-flash-lite", now.Add(-30*time.Minute), 1)
	recordSpend(t, admin, tenant, subject, "gemini-3.1-flash-image", now.Add(-35*time.Minute), 0.5)
	recordSpend(t, admin, tenant, subject, "claude-sonnet-5-5-high", now.Add(-40*time.Minute), 2)

	response, err := New(&config.Config{}, manager, nil, nil).ListStatus(tenant, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("items = %+v", response.Items)
	}
	spend := spendByWindow(t, response.Items[0])
	assertSpend(t, "antigravity", spend, "antigravity:gemini_5h", 1.5)
	assertSpend(t, "antigravity", spend, "antigravity:3p_5h", 2)
}

func TestWithQuotaWindowSpendAsksOnlyForWindowsItCanAnswer(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	reset := func(in time.Duration) *time.Time { at := now.Add(in); return &at }
	items := []AccountStatusView{{
		AuthSubjectID: "subject-1",
		Quotas: quotaWindowViews([]usage.QuotaWindowDTO{
			{QuotaKey: "code_5h", WindowSeconds: 18000, ResetAt: reset(3 * time.Hour)},
			{QuotaKey: "seven_day_opus", WindowSeconds: 604800, ResetAt: reset(24 * time.Hour)},
			{QuotaKey: "review_week", WindowSeconds: 604800, ResetAt: reset(24 * time.Hour)},
			{QuotaKey: "five_hour", WindowSeconds: 18000},
			{QuotaKey: "seven_day", ResetAt: reset(time.Hour)},
			{QuotaKey: "code_week", WindowSeconds: 604800, ResetAt: reset(0)},
		}),
	}}
	var asked []usage.AIAccountWindowSpendRequest
	got := withQuotaWindowSpendAt(items, now, func(requests []usage.AIAccountWindowSpendRequest) (map[string]map[string]float64, error) {
		asked = requests
		return map[string]map[string]float64{"subject-1": {"code_5h": 1.2345678, "seven_day_opus": 0}}, nil
	})
	if len(asked) != 2 || asked[0].Window != "code_5h" || asked[1].Window != "seven_day_opus" {
		t.Fatalf("asked = %+v, want code_5h and seven_day_opus only", asked)
	}
	if !asked[0].Start.Equal(now.Add(-2*time.Hour)) || asked[0].Covers != nil {
		t.Fatalf("code_5h request = %+v, want a start 2h back and every model", asked[0])
	}
	if covers := asked[1].Covers; covers == nil || !covers("Claude-Opus-5-5") || covers("claude-sonnet-5-5") {
		t.Fatal("seven_day_opus must count Opus requests only")
	}
	spend := spendByWindow(t, got[0])
	if spend["code_5h"] == nil || *spend["code_5h"] != 1.234568 {
		t.Fatalf("code_5h = %v, want 1.234568", spend["code_5h"])
	}
	if spend["seven_day_opus"] == nil || *spend["seven_day_opus"] != 0 {
		t.Fatalf("a window nothing was spent in reports 0, got %v", spend["seven_day_opus"])
	}

	failed := withQuotaWindowSpendAt(quotaItemsCopy(items), now, func([]usage.AIAccountWindowSpendRequest) (map[string]map[string]float64, error) {
		return nil, errors.New("read pool gone")
	})
	for _, window := range failed[0].Quotas {
		if window.UsedCostUSD != nil {
			t.Fatalf("a failed read must leave the windows without spend: %+v", window)
		}
	}
}

func quotaItemsCopy(items []AccountStatusView) []AccountStatusView {
	out := make([]AccountStatusView, len(items))
	for i := range items {
		out[i] = items[i]
		out[i].Quotas = make([]QuotaWindowView, len(items[i].Quotas))
		for j := range items[i].Quotas {
			out[i].Quotas[j] = QuotaWindowView{QuotaWindowDTO: items[i].Quotas[j].QuotaWindowDTO}
		}
	}
	return out
}
