package aiaccountstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// accountsCheckBodyForTest has the shape accounts/check answered with live on
// 2026-11-20 (ids replaced, unrelated fields trimmed): the workspace under its
// id and again under "default", the cycle in entitlement, and the renewal
// intent in last_active_subscription.
func accountsCheckBodyForTest(accountID, entitlement, willRenew string) []byte {
	entry := fmt.Sprintf(`{
		"account": {"account_id": %q, "plan_type": "pro", "structure": "personal", "is_deactivated": false},
		"features": ["a", "b"],
		"entitlement": %s,
		"last_active_subscription": {"subscription_id": "sub-1", "purchase_origin_platform": "chatgpt_web", "will_renew": %s, "cancellation_outcome": null},
		"can_access_with_session": true
	}`, accountID, entitlement, willRenew)
	return []byte(fmt.Sprintf(`{"accounts": {%q: %s, "default": %s}, "account_ordering": [%q]}`, accountID, entry, entry, accountID))
}

const liveEntitlementForTest = `{
	"subscription_id": "sub-1", "has_active_subscription": true, "is_active_subscription_gratis": false,
	"subscription_plan": "chatgptpro", "expires_at": "2026-12-10T11:03:33+00:00", "renews_at": "2026-12-10T05:03:33+00:00",
	"cancels_at": null, "billing_period": "monthly", "scheduled_plan_change": null, "billing_currency": "USD",
	"discount": null, "trial": null, "is_delinquent": false, "became_delinquent_timestamp": null,
	"grace_period_end_timestamp": null, "grace_period_id": null
}`

func TestParseCodexEntitlementReadsLiveCycle(t *testing.T) {
	checked := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	got, ok := parseCodexEntitlement(accountsCheckBodyForTest("acct-1", liveEntitlementForTest, "true"), "acct-1", checked)
	if !ok {
		t.Fatal("live response not parsed")
	}
	expires := time.Date(2026, 12, 10, 11, 3, 33, 0, time.UTC)
	renews := time.Date(2026, 12, 10, 5, 3, 33, 0, time.UTC)
	if got.Source != usage.SubscriptionSourceProbe || got.State != usage.SubscriptionStateActive || got.Period != "monthly" {
		t.Fatalf("cycle = %+v", got)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) || got.RenewsAt == nil || !got.RenewsAt.Equal(renews) || got.CancelsAt != nil {
		t.Fatalf("dates expires=%v renews=%v cancels=%v", got.ExpiresAt, got.RenewsAt, got.CancelsAt)
	}
	if got.CheckedAt == nil || !got.CheckedAt.Equal(checked) || got.StartedAt != nil {
		t.Fatalf("checked=%v started=%v", got.CheckedAt, got.StartedAt)
	}
}

func TestParseCodexEntitlementStates(t *testing.T) {
	checked := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, entitlement, willRenew, state string
	}{
		{"renewal turned off", `{"has_active_subscription": true, "expires_at": "2026-12-10T11:03:33+00:00", "renews_at": null, "cancels_at": "2026-12-10T05:03:33+00:00", "billing_period": "monthly", "is_delinquent": false}`, "false", usage.SubscriptionStateCanceling},
		{"will_renew false alone", `{"has_active_subscription": true, "expires_at": "2026-12-10T11:03:33+00:00", "billing_period": "monthly"}`, "false", usage.SubscriptionStateCanceling},
		{"renewal charge failed", `{"has_active_subscription": true, "expires_at": "2026-12-10T11:03:33+00:00", "is_delinquent": true, "grace_period_end_timestamp": 1765400000}`, "true", usage.SubscriptionStatePastDue},
		{"no paid plan", `{"has_active_subscription": false, "expires_at": null, "renews_at": null, "billing_period": null}`, "null", usage.SubscriptionStateInactive},
		{"no active flag reported", `{"expires_at": "2026-12-10T11:03:33+00:00", "renews_at": "2026-12-10T05:03:33+00:00"}`, "true", usage.SubscriptionStateActive},
	} {
		got, ok := parseCodexEntitlement(accountsCheckBodyForTest("acct-1", tc.entitlement, tc.willRenew), "acct-1", checked)
		if !ok || got.State != tc.state {
			t.Errorf("%s: ok=%v state=%q, want %q", tc.name, ok, got.State, tc.state)
		}
	}
}

// expires_at is the end of paid access; without it the cancel date, then the
// renewal date, stand in.
func TestParseCodexEntitlementEndFallbacks(t *testing.T) {
	checked := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	got, _ := parseCodexEntitlement(accountsCheckBodyForTest("acct-1", `{"has_active_subscription": true, "cancels_at": "2026-12-09T00:00:00Z", "renews_at": "2026-12-10T00:00:00Z"}`, "false"), "acct-1", checked)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end from cancel date = %v", got.ExpiresAt)
	}
	got, _ = parseCodexEntitlement(accountsCheckBodyForTest("acct-1", `{"has_active_subscription": true, "renews_at": "2026-12-10T00:00:00Z"}`, "true"), "acct-1", checked)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end from renewal date = %v", got.ExpiresAt)
	}
}

// Only the workspace that was asked about is trusted, so another workspace's
// cycle is never stored on this account.
func TestParseCodexEntitlementTrustsOnlyTheSameAccount(t *testing.T) {
	checked := time.Now()
	if _, ok := parseCodexEntitlement(accountsCheckBodyForTest("acct-other", liveEntitlementForTest, "true"), "acct-mine", checked); ok {
		t.Fatal("another workspace's cycle was trusted")
	}
	if _, ok := parseCodexEntitlement([]byte(`{"accounts": {"acct-mine": {"account": {"account_id": "acct-mine"}}}}`), "acct-mine", checked); ok {
		t.Fatal("an entry without entitlement was parsed")
	}
	// Codex's own wham/accounts/check: a list, with the plan type only.
	if _, ok := parseCodexEntitlement([]byte(`{"accounts": [{"id": "acct-mine", "plan_type": "pro"}]}`), "acct-mine", checked); ok {
		t.Fatal("a response without a billing cycle was parsed")
	}
	if _, ok := parseCodexEntitlement([]byte(`<html>`), "acct-mine", checked); ok {
		t.Fatal("a non-JSON body was parsed")
	}
}

func TestCodexSubscriptionDue(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	live := func(checked, renews *time.Time) usage.AIAccountSubscription {
		return usage.AIAccountSubscription{Source: usage.SubscriptionSourceProbe, CheckedAt: checked, RenewsAt: renews, ExpiresAt: renews}
	}
	for _, tc := range []struct {
		name   string
		stored usage.AIAccountSubscription
		failed *time.Time
		due    bool
	}{
		{"never read", usage.AIAccountSubscription{}, nil, true},
		{"only a token snapshot", usage.AIAccountSubscription{Source: usage.SubscriptionSourceSignedClaims, ExpiresAt: at(240 * time.Hour)}, nil, true},
		{"read two hours ago", live(at(-2*time.Hour), at(240*time.Hour)), nil, false},
		{"read a day ago", live(at(-25*time.Hour), at(240*time.Hour)), nil, true},
		{"renewal passed since the read", live(at(-2*time.Hour), at(-30*time.Minute)), nil, true},
		{"renewal passed, read under an hour ago", live(at(-50*time.Minute), at(-30*time.Minute)), nil, false},
		{"renewal already past at the read", live(at(-2*time.Hour), at(-3*time.Hour)), nil, false},
		{"failed an hour ago", usage.AIAccountSubscription{}, at(-time.Hour), false},
		{"failed four hours ago", usage.AIAccountSubscription{}, at(-4 * time.Hour), true},
	} {
		subjectID := "due-" + tc.name
		if tc.failed != nil {
			rememberCodexSubscriptionFailure(subjectID, *tc.failed)
		}
		if got := codexSubscriptionDue(subjectID, tc.stored, now); got != tc.due {
			t.Errorf("%s: due=%v, want %v", tc.name, got, tc.due)
		}
		forgetCodexSubscriptionFailure(subjectID)
	}
}

func codexSubscriptionTestAuth(accountID string) *coreauth.Auth {
	return &coreauth.Auth{
		ID: "sub-auth-" + accountID, TenantID: "tenant-sub", Provider: "codex", FileName: accountID + ".json",
		Metadata: map[string]any{"account_id": accountID, "chatgpt_user_id": "user-" + accountID, "email": accountID + "@example.com"},
	}
}

// The read goes out like the quota probe (same header set, the workspace
// pinned), a stored fresh read means no request, and a failure waits out the
// backoff instead of asking again every quota round.
func TestReadCodexSubscriptionIfDue(t *testing.T) {
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(usage.GetDBPath()) })
	auth := codexSubscriptionTestAuth("acct-read")
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil || !identity.ShareEligible {
		t.Fatalf("identity = %+v", identity)
	}
	if err := usage.UpsertAIAccountSubject(identity); err != nil {
		t.Fatalf("upsert subject: %v", err)
	}
	t.Cleanup(func() { forgetCodexSubscriptionFailure(identity.ID) })

	var requests []*http.Request
	var body []byte
	var replyErr error
	reply := func(b []byte, err error) { body, replyErr = b, err }
	codexSubscriptionGET = func(_ context.Context, _ *managementapitools.Service, _ *coreauth.Auth, url string, headers map[string]string, mutate func(*http.Request)) ([]byte, error) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		mutate(req)
		requests = append(requests, req)
		return body, replyErr
	}
	t.Cleanup(func() { codexSubscriptionGET = doAuthGET })

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	reply(accountsCheckBodyForTest("acct-read", liveEntitlementForTest, "true"), nil)
	got, ok := readCodexSubscriptionIfDue(context.Background(), nil, auth, now)
	if !ok || got.State != usage.SubscriptionStateActive || len(requests) != 1 {
		t.Fatalf("first read ok=%v state=%q requests=%d", ok, got.State, len(requests))
	}
	req := requests[0]
	if req.URL.String() != codexAccountsCheckURL || req.Header.Get("Chatgpt-Account-Id") != "acct-read" ||
		req.Header.Get("User-Agent") != config.DefaultCodexFingerprintUserAgent || req.Header.Get("Originator") != "Codex Desktop" {
		t.Fatalf("request %s headers %v", req.URL, req.Header)
	}

	// Stored and fresh: the next round sends nothing.
	if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
		AuthSubjectID: identity.ID, Provider: "codex", LastProbeState: "success", Subscription: got, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("store read: %v", err)
	}
	if _, ok := readCodexSubscriptionIfDue(context.Background(), nil, auth, now.Add(2*time.Hour)); ok || len(requests) != 1 {
		t.Fatalf("fresh cycle re-read: ok=%v requests=%d", ok, len(requests))
	}

	// A day later it is read again; that read fails, and the next rounds wait.
	reply(nil, errors.New("upstream http 403"))
	if _, ok := readCodexSubscriptionIfDue(context.Background(), nil, auth, now.Add(25*time.Hour)); ok || len(requests) != 2 {
		t.Fatalf("daily re-read: ok=%v requests=%d", ok, len(requests))
	}
	if _, ok := readCodexSubscriptionIfDue(context.Background(), nil, auth, now.Add(26*time.Hour)); ok || len(requests) != 2 {
		t.Fatalf("read inside the failure backoff: ok=%v requests=%d", ok, len(requests))
	}
	reply(accountsCheckBodyForTest("acct-read", liveEntitlementForTest, "true"), nil)
	if _, ok := readCodexSubscriptionIfDue(context.Background(), nil, auth, now.Add(29*time.Hour)); !ok || len(requests) != 3 {
		t.Fatalf("read after the backoff: ok=%v requests=%d", ok, len(requests))
	}
}

// End to end through a refresh: a live read reaches the status API with every
// cycle field, and a later round without a read (token claims only) keeps it.
func TestRefreshStoresLiveSubscriptionOverTokenClaims(t *testing.T) {
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(usage.GetDBPath()) })

	claimEnd := time.Now().UTC().Add(5 * 24 * time.Hour).Truncate(time.Second)
	token := makeProviderJWTForTest(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-e2e", "chatgpt_plan_type": "pro",
			"chatgpt_subscription_active_until": claimEnd.Unix(),
		},
	})
	auth := &coreauth.Auth{
		ID: "e2e-auth", TenantID: "tenant-e2e", Provider: "codex", FileName: "e2e.json",
		Metadata: map[string]any{"account_id": "acct-e2e", "id_token": token},
	}
	manager := newTestManager(t, "tenant-e2e", auth)
	svc := New(&config.Config{}, manager, func(tenantID string) *managementapitools.Service {
		return managementapitools.NewForTenant(tenantID, &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	checked := time.Now().UTC().Truncate(time.Second)
	renews := checked.Add(20 * 24 * time.Hour)
	expires := renews.Add(6 * time.Hour)
	live := usage.AIAccountSubscription{
		ExpiresAt: &expires, Source: usage.SubscriptionSourceProbe, RenewsAt: &renews,
		Period: "monthly", State: usage.SubscriptionStateActive, CheckedAt: &checked,
	}
	probeSubscription := live
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		return ProbeResult{Quotas: []usage.QuotaWindowDTO{}, Subscription: probeSubscription}, nil
	})

	refresh := func() AccountStatusView {
		t.Helper()
		accepted := svc.StartRefresh("tenant-e2e", RefreshRequest{Force: true})
		if accepted.Accepted != 1 {
			t.Fatalf("accepted=%+v", accepted)
		}
		waitJob(t, svc, "tenant-e2e", accepted.JobID)
		response, err := svc.ListStatus("tenant-e2e", nil, nil)
		if err != nil || len(response.Items) != 1 {
			t.Fatalf("response=%+v err=%v", response, err)
		}
		return response.Items[0]
	}
	assertLive := func(stage string, item AccountStatusView) {
		t.Helper()
		got := item.AIAccountSubscription
		if got.Source != usage.SubscriptionSourceProbe || got.State != usage.SubscriptionStateActive || got.Period != "monthly" ||
			got.RenewsAt == nil || !got.RenewsAt.Equal(renews) || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) ||
			got.CheckedAt == nil || !got.CheckedAt.Equal(checked) || item.ExpiresAt == nil || !item.ExpiresAt.Equal(expires) {
			t.Fatalf("%s: subscription %+v expires_at %v", stage, got, item.ExpiresAt)
		}
	}

	item := refresh()
	assertLive("live read", item)
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"subscription_expires_at", "subscription_source", "subscription_renews_at", "subscription_period",
		"subscription_state", "subscription_checked_at", "expires_at",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("status JSON lacks %q: %s", key, raw)
		}
	}

	probeSubscription = usage.AIAccountSubscription{}
	assertLive("round with token claims only", refresh())
}
