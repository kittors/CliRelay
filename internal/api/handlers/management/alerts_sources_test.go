package management

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	_ "modernc.org/sqlite"
)

func ptr[T any](v T) *T { return &v }

func initAlertSourcesDB(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func TestAccountFailure(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	held := func(reason int) *coreauth.ModelState {
		return &coreauth.ModelState{
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), UpdatedAt: now.Add(-time.Minute),
			LastError: &coreauth.Error{HTTPStatus: reason},
		}
	}
	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantClass alerting.FailureClass
		wantWhy   string
		wantHTTP  int
	}{
		{"a working account", &coreauth.Auth{Status: coreauth.StatusActive}, alerting.FailureNone, "", 0},
		{
			"refused with 401",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, LastError: &coreauth.Error{HTTPStatus: 401}},
			alerting.FailureNeedsAction, coreauth.CooldownReasonUnauthorized, 401,
		},
		{
			"a 401 that names a reason is a credential the provider revoked",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, LastError: &coreauth.Error{HTTPStatus: 401, Code: "account_deactivated"}},
			alerting.FailureRevoked, coreauth.CooldownReasonReauthRequired, 401,
		},
		{
			"an expired token is being renewed, not broken",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, LastError: &coreauth.Error{HTTPStatus: 401, Code: "token_expired"}},
			alerting.FailureNone, "", 0,
		},
		{
			"billing or permission",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, LastError: &coreauth.Error{HTTPStatus: 402}},
			alerting.FailureNeedsAction, coreauth.CooldownReasonPaymentRequired, 402,
		},
		{
			"forbidden counts as permission",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, LastError: &coreauth.Error{HTTPStatus: 403}},
			alerting.FailureNeedsAction, coreauth.CooldownReasonPaymentRequired, 403,
		},
		{
			"rate limiting heals on its own",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, LastError: &coreauth.Error{HTTPStatus: 429}},
			alerting.FailureNone, "", 0,
		},
		{
			"an exhausted quota is the quota rule's business",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, Quota: coreauth.QuotaState{Exceeded: true}, LastError: &coreauth.Error{HTTPStatus: 429}},
			alerting.FailureNone, "", 0,
		},
		{
			"a fresh upstream error is reported",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, UpdatedAt: now.Add(-5 * time.Minute), LastError: &coreauth.Error{HTTPStatus: 502}},
			alerting.FailureTransient, coreauth.CooldownReasonUpstreamError, 502,
		},
		{
			"an old upstream error on an account nobody has used since is not still 'failing'",
			&coreauth.Auth{Status: coreauth.StatusError, Unavailable: true, UpdatedAt: now.Add(-3 * time.Hour), LastError: &coreauth.Error{HTTPStatus: 502}},
			alerting.FailureNone, "", 0,
		},
		{
			"an error on a credential that is still marked available is reported too",
			&coreauth.Auth{Status: coreauth.StatusError, LastError: &coreauth.Error{HTTPStatus: 401}},
			alerting.FailureNeedsAction, coreauth.CooldownReasonUnauthorized, 401,
		},
		{"an error status with no error says nothing", &coreauth.Auth{Status: coreauth.StatusError}, alerting.FailureNone, "", 0},
		{
			"every model refused: the cause is in the models",
			&coreauth.Auth{
				Status: coreauth.StatusActive, Unavailable: true,
				ModelStates: map[string]*coreauth.ModelState{"gpt-a": held(401), "gpt-b": held(401)},
			},
			alerting.FailureNeedsAction, coreauth.CooldownReasonUnauthorized, 401,
		},
		{
			"the most telling model reason wins over a rate limit",
			&coreauth.Auth{
				Status: coreauth.StatusActive, Unavailable: true,
				ModelStates: map[string]*coreauth.ModelState{"gpt-a": held(429), "gpt-b": held(403)},
			},
			alerting.FailureNeedsAction, coreauth.CooldownReasonPaymentRequired, 403,
		},
		{
			"every model rate limited is not a failure",
			&coreauth.Auth{
				Status: coreauth.StatusActive, Unavailable: true,
				ModelStates: map[string]*coreauth.ModelState{"gpt-a": held(429), "gpt-b": held(429)},
			},
			alerting.FailureNone, "", 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := accountFailure(c.auth, now)
			if got.Class != c.wantClass || got.Reason != c.wantWhy || got.HTTPStatus != c.wantHTTP {
				t.Fatalf("failure = %+v, want class %v reason %q http %d", got, c.wantClass, c.wantWhy, c.wantHTTP)
			}
		})
	}
}

func TestAccountDisplayNameNeverUsesTheKey(t *testing.T) {
	cases := []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{"label wins", &coreauth.Auth{ID: "id-1", Label: " Main account ", Metadata: map[string]any{"email": "a@example.com"}}, "Main account"},
		{"metadata label", &coreauth.Auth{ID: "id-1", Metadata: map[string]any{"label": "Team", "email": "a@example.com"}}, "Team"},
		{"login", &coreauth.Auth{ID: "id-1", Metadata: map[string]any{"email": " a@example.com "}}, "a@example.com"},
		{"login in attributes", &coreauth.Auth{ID: "id-1", Attributes: map[string]string{"email": "b@example.com"}}, "b@example.com"},
		{"imported file", &coreauth.Auth{ID: "id-1", FileName: "/auth/dir/codex-main.json"}, "codex-main.json"},
		{"the id as the last resort", &coreauth.Auth{ID: "id-1"}, "id-1"},
		{
			"a configured key is named by its channel, never by the key",
			&coreauth.Auth{ID: "claude:apikey:abc", Label: "claude-apikey", Attributes: map[string]string{"api_key": "sk-ant-secret-value"}},
			"claude-apikey",
		},
	}
	for _, c := range cases {
		if got := accountDisplayName(c.auth); got != c.want {
			t.Errorf("%s: name = %q, want %q", c.name, got, c.want)
		}
	}
	keyOnly := &coreauth.Auth{ID: "gemini:apikey:digest", Attributes: map[string]string{"api_key": "AIzaSySecretSecretSecret"}}
	if got := accountDisplayName(keyOnly); got != "gemini:apikey:digest" {
		t.Errorf("a key with no label is named %q, want its id", got)
	}
}

func TestIsStaticKeyCredential(t *testing.T) {
	if !isStaticKeyCredential(&coreauth.Auth{Attributes: map[string]string{"api_key": "sk-secret"}}) {
		t.Error("a credential that holds an API key must be a static key")
	}
	if isStaticKeyCredential(&coreauth.Auth{Metadata: map[string]any{"email": "a@example.com"}, Attributes: map[string]string{"api_key": "sk-secret"}}) {
		// AccountInfo prefers the login; a signed-in account is not a static key even if it also carries one.
		t.Error("a signed-in account is not a static key")
	}
	if isStaticKeyCredential(&coreauth.Auth{Metadata: map[string]any{"email": "a@example.com"}}) {
		t.Error("an OAuth account is not a static key")
	}
	if isStaticKeyCredential(&coreauth.Auth{}) {
		t.Error("a credential with nothing in it is not a static key")
	}
}

func TestQuotaWindowSamples(t *testing.T) {
	reset := time.Date(2026, 11, 5, 8, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	observed := time.Date(2026, 11, 2, 11, 30, 0, 0, time.UTC)
	rowObserved := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	got := quotaWindowSamples([]usage.QuotaWindowDTO{
		{QuotaKey: "code_week", QuotaLabel: "Weekly", Percent: ptr(30.0), ResetAt: &reset, WindowSeconds: 604800, ObservedAt: &observed},
		{QuotaKey: "code_5h", Percent: ptr(0.0), WindowSeconds: 18000},                         // nothing left: used 100, no per-window time
		{QuotaKey: "seven_day_opus", Percent: ptr(-5.0), WindowSeconds: 604800},                // less than nothing left is still 100% used
		{QuotaKey: "five_hour", Percent: ptr(140.0), WindowSeconds: 18000},                     // more than all left is still 0% used
		{QuotaKey: "credits", Value: "12 left"},                                                // no percentage: says nothing
		{QuotaKey: "model:gemini-pro", Percent: ptr(50.0), WindowSeconds: 86400, ResetAt: nil}, // scoped to one model
	}, &rowObserved)

	if len(got) != 5 {
		t.Fatalf("samples = %+v, want 5 (the window with no percentage is dropped)", got)
	}
	week := got[0]
	if week.Key != "code_week" || week.Label != "Weekly" || week.Span != 7*24*time.Hour || week.UsedPercent != 70 {
		t.Fatalf("week = %+v", week)
	}
	if !week.ResetAt.Equal(reset) || week.ResetAt.Location() != time.UTC {
		t.Fatalf("week reset = %v, want %v in UTC", week.ResetAt, reset)
	}
	if !week.ObservedAt.Equal(observed) {
		t.Fatalf("week observed = %v, want the window's own time %v", week.ObservedAt, observed)
	}
	if !week.AccountWide {
		t.Error("code_week limits every request of the account")
	}
	if got[1].UsedPercent != 100 || !got[1].ResetAt.IsZero() || !got[1].ObservedAt.Equal(rowObserved) {
		t.Fatalf("5h = %+v, want 100%% used, no reset and the row's observation time", got[1])
	}
	if got[2].UsedPercent != 100 || got[3].UsedPercent != 0 {
		t.Fatalf("clamping: opus used %v (want 100), five_hour used %v (want 0)", got[2].UsedPercent, got[3].UsedPercent)
	}
	if got[2].AccountWide {
		t.Error("the Opus window limits one model, not every request")
	}
	if got[4].AccountWide {
		t.Error("a per-model window must not be pooled")
	}
}

func TestSpendCells(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC) // a Wednesday
	var limits, used quota.Matrix
	limits.Set(quota.MetricCost, quota.PeriodFiveHour, 10)
	limits.Set(quota.MetricRequests, quota.PeriodDay, 100)
	limits.Set(quota.MetricTokens, quota.PeriodWeek, 5000)
	limits.Set(quota.MetricCost, quota.PeriodMonth, 200)
	limits.Set(quota.MetricRequests, quota.PeriodLifetime, 1000)
	used.Set(quota.MetricCost, quota.PeriodFiveHour, 9.5)
	used.Set(quota.MetricRequests, quota.PeriodDay, 40)
	used.Set(quota.MetricTokens, quota.PeriodWeek, 4900)
	used.Set(quota.MetricCost, quota.PeriodMonth, 20)
	used.Set(quota.MetricRequests, quota.PeriodLifetime, 999)
	windowStart := now.Add(-2 * time.Hour)

	cells := spendCells(limits, quota.UsageSnapshot{Total: used, FiveHourWindowStart: windowStart}, now)
	if len(cells) != 5 {
		t.Fatalf("cells = %+v, want the five configured limits and nothing else", cells)
	}
	byKey := map[string]alerting.SpendCell{}
	for _, c := range cells {
		byKey[c.Metric+"/"+c.Window] = c
	}
	five := byKey["cost/5h"]
	if five.Used != 9.5 || five.Limit != 10 || !five.ResetAt.Equal(windowStart.Add(quota.FiveHourWindowDuration)) {
		t.Fatalf("5h cost = %+v, want 9.5/10 renewing five hours after the window started", five)
	}
	if day := byKey["requests/day"]; day.Used != 40 || day.Limit != 100 || !day.ResetAt.After(now) || day.ResetAt.Sub(now) > 24*time.Hour {
		t.Fatalf("daily requests = %+v, want 40/100 renewing within a day", day)
	}
	if week := byKey["tokens/week"]; week.Used != 4900 || !week.ResetAt.After(now) || week.ResetAt.Sub(now) > 7*24*time.Hour {
		t.Fatalf("weekly tokens = %+v, want 4900 renewing within a week", week)
	}
	if month := byKey["cost/month"]; !month.ResetAt.After(now) || month.ResetAt.Day() != 1 {
		t.Fatalf("monthly cost = %+v, want a renewal on the 1st", month)
	}
	if life := byKey["requests/lifetime"]; life.Used != 999 || !life.ResetAt.IsZero() {
		t.Fatalf("lifetime = %+v, want 999 and no renewal", life)
	}

	// A 5h window that already ended has no renewal to announce.
	stale := spendCells(limits, quota.UsageSnapshot{Total: used, FiveHourWindowStart: now.Add(-6 * time.Hour)}, now)
	for _, c := range stale {
		if c.Metric == "cost" && c.Window == "5h" && !c.ResetAt.IsZero() {
			t.Fatalf("a 5h window that ended announced a renewal at %v", c.ResetAt)
		}
	}
	none := spendCells(limits, quota.UsageSnapshot{Total: used}, now)
	for _, c := range none {
		if c.Metric == "cost" && c.Window == "5h" && !c.ResetAt.IsZero() {
			t.Fatalf("a 5h window with no anchor announced a renewal at %v", c.ResetAt)
		}
	}
}

func TestKeyDisplayNameNeverFallsBackToTheSecret(t *testing.T) {
	cases := []struct {
		row   usage.APIKeyRow
		owner string
		want  string
	}{
		{usage.APIKeyRow{ID: "k-1", Key: "sk-super-secret", Name: " CI key "}, "", "CI key"},
		{usage.APIKeyRow{ID: "k-1", Key: "sk-super-secret", Name: "CI key"}, "Chen Long", "Chen Long / CI key"},
		{usage.APIKeyRow{ID: "0123456789abcdef", Key: "sk-super-secret"}, "", "key 01234567"},
		{usage.APIKeyRow{ID: "k-2", Key: "sk-super-secret"}, "", "key k-2"},
	}
	for _, c := range cases {
		got := keyDisplayName(c.row, c.owner)
		if got != c.want {
			t.Errorf("keyDisplayName(%+v, %q) = %q, want %q", c.row, c.owner, got, c.want)
		}
		if got == c.row.Key {
			t.Errorf("a key was named by its own secret")
		}
	}
}

func TestAlertSourcesAccounts(t *testing.T) {
	initAlertSourcesDB(t)
	const tenant = "tenant-alert-sources"
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(48 * time.Hour)

	manager := coreauth.NewManager(nil, nil, nil)
	register := func(auth *coreauth.Auth) *coreauth.Auth {
		t.Helper()
		auth.TenantID = tenant
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		return auth
	}
	healthy := register(&coreauth.Auth{
		ID: "codex-healthy", Provider: "Codex", Label: "main", FileName: "healthy.json", Status: coreauth.StatusActive,
		Metadata: map[string]any{"account_id": "acct-healthy", "chatgpt_user_id": "user-1"},
	})
	broken := register(&coreauth.Auth{
		ID: "codex-broken", Provider: "codex", FileName: "broken.json", Status: coreauth.StatusError, Unavailable: true,
		Metadata:  map[string]any{"account_id": "acct-broken", "chatgpt_user_id": "user-2"},
		LastError: &coreauth.Error{HTTPStatus: 401, Code: "token_invalidated"}, UpdatedAt: now,
	})
	register(&coreauth.Auth{
		ID: "codex-off", Provider: "codex", Label: "switched off", Disabled: true, Status: coreauth.StatusDisabled,
		Metadata: map[string]any{"account_id": "acct-off", "chatgpt_user_id": "user-3"},
	})
	register(&coreauth.Auth{
		ID: "claude:apikey:digest", Provider: "claude", Label: "claude-apikey", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "sk-ant-never-in-an-alert"},
	})
	other := &coreauth.Auth{
		ID: "codex-other-tenant", Provider: "codex", Label: "someone else's", Status: coreauth.StatusActive, TenantID: "another-tenant",
		Metadata: map[string]any{"account_id": "acct-other", "chatgpt_user_id": "user-4"},
	}
	if _, err := manager.Register(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	subject := usage.ResolveAuthSubjectIdentity(healthy).ID
	if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
		AuthSubjectID: subject, Provider: "codex", UpstreamCheckedAt: &now,
		Quotas: []usage.QuotaWindowDTO{
			{QuotaKey: "code_week", QuotaLabel: "Weekly", Percent: ptr(12.0), ResetAt: &reset, WindowSeconds: 604800},
			{QuotaKey: "additional:codex_bengalfox:week", Percent: ptr(80.0), ResetAt: &reset, WindowSeconds: 604800},
		},
	}); err != nil {
		t.Fatal(err)
	}

	sources := alertSources{authManager: func() *coreauth.Manager { return manager }}
	accounts, err := sources.Accounts(context.Background(), tenant, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]alerting.AccountSample{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	if len(accounts) != 3 {
		t.Fatalf("accounts = %+v, want the tenant's three that are switched on (not the disabled one, not another tenant's)", accounts)
	}

	h := byID["codex-healthy"]
	if h.Name != "main" || h.Provider != "codex" || h.Subject != subject || h.StaticKey || h.Failure.Class != alerting.FailureNone {
		t.Fatalf("healthy = %+v", h)
	}
	if len(h.Windows) != 2 {
		t.Fatalf("healthy windows = %+v, want both stored windows", h.Windows)
	}
	for _, w := range h.Windows {
		switch w.Key {
		case "code_week":
			if w.UsedPercent != 88 || !w.AccountWide || w.Span != 7*24*time.Hour || !w.ResetAt.Equal(reset) {
				t.Fatalf("code_week = %+v", w)
			}
		default:
			if w.AccountWide {
				t.Fatalf("%s is the Spark allowance, not an account-wide window: %+v", w.Key, w)
			}
		}
		if w.ObservedAt.IsZero() {
			t.Fatalf("%s carries no observation time", w.Key)
		}
	}

	b := byID["codex-broken"]
	if b.Failure.Class != alerting.FailureRevoked || b.Failure.HTTPStatus != 401 || b.Name != "broken.json" {
		t.Fatalf("broken = %+v, want a revoked credential named after its file", b)
	}
	if b.Subject == "" || b.Subject == h.Subject {
		t.Fatalf("broken account's subject = %q, want its own", b.Subject)
	}

	k := byID["claude:apikey:digest"]
	if !k.StaticKey || k.Name != "claude-apikey" || len(k.Windows) != 0 {
		t.Fatalf("api key credential = %+v", k)
	}
	if _, leaked := byID["codex-off"]; leaked {
		t.Fatal("an account the operator switched off must not be sampled")
	}
	if _, leaked := byID["codex-other-tenant"]; leaked {
		t.Fatal("another tenant's account must not be sampled")
	}
	_ = broken
}

func TestAlertSourcesAccountsWithoutAManager(t *testing.T) {
	for _, sources := range []alertSources{{}, {authManager: func() *coreauth.Manager { return nil }}} {
		accounts, err := sources.Accounts(context.Background(), "tenant", time.Now())
		if err != nil || len(accounts) != 0 {
			t.Fatalf("accounts = %+v err = %v, want none and no error", accounts, err)
		}
	}
}
