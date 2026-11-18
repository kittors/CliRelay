package aiaccountstatus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v8/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

	started := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
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
	if item.PlanType != "plus" || item.SubscriptionSource != "signed_claims" {
		t.Fatalf("shared asserted fields=%+v", item)
	}
	if item.SubscriptionStartedAt == nil || !item.SubscriptionStartedAt.Equal(started) || item.SubscriptionExpiresAt == nil || !item.SubscriptionExpiresAt.Equal(expires) {
		t.Fatalf("subscription start=%v expires=%v", item.SubscriptionStartedAt, item.SubscriptionExpiresAt)
	}
}

func TestProviderClaimsMustMatchStableAccountID(t *testing.T) {
	token := makeProviderJWTForTest(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "different-account",
			"chatgpt_plan_type":                 "plus",
			"chatgpt_subscription_active_until": "2026-08-01T00:00:00Z",
		},
	})
	auth := &coreauth.Auth{Provider: "codex", TenantID: "tenant-a", Metadata: map[string]any{"account_id": "expected-account", "id_token": token}}
	facts := readProviderAssertedAccountFacts(auth)
	if facts.PlanType != "" || facts.SubscriptionStarted != nil || facts.SubscriptionExpires != nil || facts.SubscriptionSource != "" {
		t.Fatalf("mismatched claims were trusted: %+v", facts)
	}
}
