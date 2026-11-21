package aiaccountstatus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func makeProviderJWTForTest(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestRefreshPersistsOnlyProviderAssertedSubscription(t *testing.T) {
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(usage.GetDBPath()) })

	// A snapshot still ahead of its end; one past it is dropped (see
	// TestProviderClaimsPastTheirEndAreNotUsed).
	started := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	expires := started.Add(30 * 24 * time.Hour)
	token := makeProviderJWTForTest(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "acct-asserted",
			"chatgpt_plan_type":                 "PLUS",
			"chatgpt_subscription_active_start": started.Format(time.RFC3339),
			"chatgpt_subscription_active_until": expires.Unix(),
		},
	})
	auth := &coreauth.Auth{
		ID: "asserted-auth", TenantID: "tenant-asserted", Provider: "codex", FileName: "asserted.json",
		Metadata: map[string]any{
			"account_id":              "acct-asserted",
			"id_token":                token,
			"subscription_started_at": "2099-01-01T00:00:00Z", // tenant-private manual override; ignored here
			"subscription_expires_at": "2099-02-01T00:00:00Z",
		},
	}
	manager := newTestManager(t, "tenant-asserted", auth)
	svc := New(&config.Config{}, manager, func(tenantID string) *managementapitools.Service {
		return managementapitools.NewForTenant(tenantID, &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(context.Context, *managementapitools.Service, *config.Config, *coreauth.Auth) (ProbeResult, error) {
		return ProbeResult{Quotas: []usage.QuotaWindowDTO{}}, nil
	})

	accepted := svc.StartRefresh("tenant-asserted", RefreshRequest{Force: true})
	if accepted.Accepted != 1 {
		t.Fatalf("accepted=%+v", accepted)
	}
	waitJob(t, svc, "tenant-asserted", accepted.JobID)
	response, err := svc.ListStatus("tenant-asserted", nil, nil)
	if err != nil || len(response.Items) != 1 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	item := response.Items[0]
	subscription := item.AIAccountSubscription
	if item.PlanType != "plus" || subscription.Source != "signed_claims" {
		t.Fatalf("shared asserted fields=%+v", item)
	}
	if subscription.StartedAt == nil || !subscription.StartedAt.Equal(started) || subscription.ExpiresAt == nil || !subscription.ExpiresAt.Equal(expires) {
		t.Fatalf("subscription start=%v expires=%v", subscription.StartedAt, subscription.ExpiresAt)
	}
}

func TestProviderClaimsMustMatchStableAccountID(t *testing.T) {
	token := makeProviderJWTForTest(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "different-account",
			"chatgpt_plan_type":                 "plus",
			"chatgpt_subscription_active_until": "2099-08-01T00:00:00Z",
		},
	})
	auth := &coreauth.Auth{Provider: "codex", TenantID: "tenant-a", Metadata: map[string]any{"account_id": "expected-account", "id_token": token}}
	facts := readProviderAssertedAccountFacts(auth, time.Now())
	if facts.PlanType != "" || facts.Subscription != (usage.AIAccountSubscription{}) {
		t.Fatalf("mismatched claims were trusted: %+v", facts)
	}
}

// Codex re-issues id_tokens on refresh without updating the subscription
// claims, so a snapshot past its end says nothing about the plan today: the
// plan type is still read, the dates are not.
func TestProviderClaimsPastTheirEndAreNotUsed(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	token := makeProviderJWTForTest(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "acct-stale",
			"chatgpt_plan_type":                 "pro",
			"chatgpt_subscription_active_start": "2026-09-10T05:02:42+00:00",
			"chatgpt_subscription_active_until": "2026-10-10T05:03:33+00:00",
		},
	})
	auth := &coreauth.Auth{Provider: "codex", TenantID: "tenant-a", Metadata: map[string]any{"account_id": "acct-stale", "id_token": token}}
	facts := readProviderAssertedAccountFacts(auth, now)
	if facts.PlanType != "pro" {
		t.Fatalf("plan type = %q, want pro", facts.PlanType)
	}
	if facts.Subscription != (usage.AIAccountSubscription{}) {
		t.Fatalf("stale snapshot was used: %+v", facts.Subscription)
	}
}

// A live billing read reported by the probe is never replaced by the token
// snapshot, even one still ahead of its end.
func TestMergeProviderAssertedFactsKeepsLiveRead(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	token := makeProviderJWTForTest(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "acct-live",
			"chatgpt_plan_type":                 "pro",
			"chatgpt_subscription_active_until": "2026-12-01T00:00:00Z",
		},
	})
	auth := &coreauth.Auth{Provider: "codex", TenantID: "tenant-a", Metadata: map[string]any{"account_id": "acct-live", "id_token": token}}
	renews := time.Date(2026, 12, 10, 5, 3, 33, 0, time.UTC)
	probe := ProbeResult{Subscription: usage.AIAccountSubscription{Source: usage.SubscriptionSourceProbe, RenewsAt: &renews, ExpiresAt: &renews}}
	mergeProviderAssertedFacts(&probe, auth, now)
	if probe.Subscription.Source != usage.SubscriptionSourceProbe || !probe.Subscription.ExpiresAt.Equal(renews) {
		t.Fatalf("live read replaced: %+v", probe.Subscription)
	}
	if probe.PlanType != "pro" {
		t.Fatalf("plan type not filled from claims: %q", probe.PlanType)
	}

	probe = ProbeResult{}
	mergeProviderAssertedFacts(&probe, auth, now)
	if probe.Subscription.Source != usage.SubscriptionSourceSignedClaims {
		t.Fatalf("claims not used without a live read: %+v", probe.Subscription)
	}
}
