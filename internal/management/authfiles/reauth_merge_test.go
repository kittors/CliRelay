package authfiles

import (
	"context"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestFindReauthReplacementPlanMatchesPlanUpgradeAndRespectsBoundaries(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)

	oldGo := &coreauth.Auth{
		ID:       "tenant-1/codex-alice@example.com-go.json",
		TenantID: "tenant-1",
		Provider: "codex",
		FileName: "codex-alice@example.com-go.json",
		Status:   coreauth.StatusError,
		Metadata: map[string]any{
			"type":            "codex",
			"email":           "alice@example.com",
			"account_id":      "acct-alice",
			"chatgpt_user_id": "user-alice",
			"plan_type":       "go",
		},
	}
	legacyNoUserID := &coreauth.Auth{
		ID:       "tenant-1/codex-alice@example.com.json",
		TenantID: "tenant-1",
		Provider: "codex",
		FileName: "codex-alice@example.com.json",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":       "codex",
			"email":      "alice@example.com",
			"account_id": "acct-alice",
		},
	}
	sameTeamOtherMember := &coreauth.Auth{
		ID:       "tenant-1/codex-bob@example.com-team.json",
		TenantID: "tenant-1",
		Provider: "codex",
		FileName: "codex-bob@example.com-team.json",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":            "codex",
			"email":           "bob@example.com",
			"account_id":      "acct-alice",
			"chatgpt_user_id": "user-bob",
		},
	}
	otherTenantSameAccount := &coreauth.Auth{
		ID:       "tenant-2/codex-alice@example.com-go.json",
		TenantID: "tenant-2",
		Provider: "codex",
		FileName: "codex-alice@example.com-go.json",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":            "codex",
			"email":           "alice@example.com",
			"account_id":      "acct-alice",
			"chatgpt_user_id": "user-alice",
		},
	}
	apiKeyCred := &coreauth.Auth{
		ID:       "tenant-1/codex-apikey.json",
		TenantID: "tenant-1",
		Provider: "codex",
		FileName: "codex-apikey.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key": "sk-test-XXXX-1234",
		},
		Metadata: map[string]any{
			"type":            "codex",
			"email":           "alice@example.com",
			"account_id":      "acct-alice",
			"chatgpt_user_id": "user-alice",
		},
	}
	for _, item := range []*coreauth.Auth{oldGo, legacyNoUserID, sameTeamOtherMember, otherTenantSameAccount, apiKeyCred} {
		if _, err := manager.Register(ctx, item); err != nil {
			t.Fatalf("Register(%s): %v", item.ID, err)
		}
	}

	incomingPro := &coreauth.Auth{
		ID:       "tenant-1/codex-alice@example.com-pro.json",
		TenantID: "tenant-1",
		Provider: "codex",
		FileName: "codex-alice@example.com-pro.json",
		Metadata: map[string]any{
			"type":            "codex",
			"email":           "alice@example.com",
			"account_id":      "acct-alice",
			"chatgpt_user_id": "user-alice",
			"plan_type":       "pro",
		},
	}

	plan := FindReauthReplacementPlan(manager, "tenant-1", incomingPro)
	if plan.SameID != nil {
		t.Fatalf("SameID = %v, want nil", plan.SameID.ID)
	}
	if len(plan.SupersededAliases) != 2 {
		t.Fatalf("SupersededAliases len = %d, want 2: %+v", len(plan.SupersededAliases), plan.SupersededAliases)
	}
	ids := map[string]bool{}
	for _, a := range plan.SupersededAliases {
		ids[a.ID] = true
	}
	if !ids[oldGo.ID] || !ids[legacyNoUserID.ID] {
		t.Fatalf("unexpected superseded alias set: %v", ids)
	}
	if ids[sameTeamOtherMember.ID] || ids[otherTenantSameAccount.ID] || ids[apiKeyCred.ID] {
		t.Fatalf("must not include other team member, other tenant, or api_key credential: %v", ids)
	}
}

func TestApplyReauthOperationalInheritancePreservesSettingsAndDropsStaleState(t *testing.T) {
	oldCred := &coreauth.Auth{
		ID:            "tenant-1/codex-alice@example.com-go.json",
		TenantID:      "tenant-1",
		Provider:      "codex",
		FileName:      "codex-alice@example.com-go.json",
		ProxyID:       "proxy-us-1",
		Prefix:        "vip",
		Label:         "Alice Pro",
		Disabled:      true,
		Status:        coreauth.StatusError,
		StatusMessage: "re-login required: token_revoked",
		Metadata: map[string]any{
			"type":                           "codex",
			"email":                          "alice@example.com",
			"access_token":                   "old-dead-access",
			"refresh_token":                  "old-dead-refresh",
			"proxy_id":                       "proxy-us-1",
			"prefix":                         "vip",
			"label":                          "Alice Pro",
			"custom_tags":                    []any{"prod", "tier1"},
			"hidden_default_tags":            []any{"codex"},
			"display_tags":                   []any{"prod"},
			"priority":                       float64(20),
			"subscription_started_at":        "2026-11-08T00:00:00Z",
			"subscription_period":            "monthly",
			"concurrency_limit":              float64(4),
			"codex_convergence_mode":         "session",
			"codex_service_tier":             "priority",
			"codex_cli_only":                 true,
			"codex_cli_only_allowed_clients": []any{"codex-cli"},
			"codex_image_generation_bridge":  false,
			"codex_image_generation_model":   "gpt-image-1.5",
			"disable_cooling":                true,
			"disabled":                       true,
			coreauth.CodexOAuthHealthMetadataKey: map[string]any{
				"status":        "reauth_required",
				"reauth_reason": "token_revoked",
			},
		},
	}

	incoming := &coreauth.Auth{
		ID:       "tenant-1/codex-alice@example.com-pro.json",
		TenantID: "tenant-1",
		Provider: "codex",
		FileName: "codex-alice@example.com-pro.json",
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "alice@example.com",
			"access_token":  "fresh-access",
			"refresh_token": "fresh-refresh",
			"plan_type":     "pro",
		},
	}

	ApplyReauthOperationalInheritance(incoming, ReauthReplacementPlan{
		SupersededAliases: []*coreauth.Auth{oldCred},
	})

	if incoming.ProxyID != "proxy-us-1" || incoming.Metadata["proxy_id"] != "proxy-us-1" {
		t.Fatalf("proxy_id not inherited: field=%q meta=%v", incoming.ProxyID, incoming.Metadata["proxy_id"])
	}
	if incoming.Prefix != "vip" || incoming.Metadata["prefix"] != "vip" {
		t.Fatalf("prefix not inherited: field=%q meta=%v", incoming.Prefix, incoming.Metadata["prefix"])
	}
	if incoming.Label != "Alice Pro" || incoming.Metadata["label"] != "Alice Pro" {
		t.Fatalf("label not inherited: field=%q meta=%v", incoming.Label, incoming.Metadata["label"])
	}
	if !reflect.DeepEqual(incoming.Metadata["custom_tags"], []string{"prod", "tier1"}) {
		t.Fatalf("custom_tags = %#v", incoming.Metadata["custom_tags"])
	}
	if incoming.Metadata["priority"] != 20 {
		t.Fatalf("priority = %#v, want 20", incoming.Metadata["priority"])
	}
	if incoming.Metadata["subscription_started_at"] != "2026-11-08T00:00:00Z" || incoming.Metadata["subscription_period"] != "monthly" {
		t.Fatalf("subscription = %v / %v", incoming.Metadata["subscription_started_at"], incoming.Metadata["subscription_period"])
	}
	if incoming.Metadata["concurrency_limit"] != 4 || incoming.Attributes["concurrency_limit"] != "4" {
		t.Fatalf("concurrency_limit = %v / attr=%q", incoming.Metadata["concurrency_limit"], incoming.Attributes["concurrency_limit"])
	}
	if incoming.Metadata["codex_convergence_mode"] != "session" || incoming.Attributes["codex_convergence_mode"] != "session" {
		t.Fatalf("codex_convergence_mode = %v / attr=%q", incoming.Metadata["codex_convergence_mode"], incoming.Attributes["codex_convergence_mode"])
	}
	if incoming.Metadata["codex_service_tier"] != "priority" || incoming.Attributes["codex_service_tier"] != "priority" {
		t.Fatalf("codex_service_tier = %v / attr=%q", incoming.Metadata["codex_service_tier"], incoming.Attributes["codex_service_tier"])
	}
	if incoming.Metadata["codex_cli_only"] != true || !reflect.DeepEqual(incoming.Metadata["codex_cli_only_allowed_clients"], []string{"codex-cli"}) {
		t.Fatalf("codex_cli_only = %v / %#v", incoming.Metadata["codex_cli_only"], incoming.Metadata["codex_cli_only_allowed_clients"])
	}
	if incoming.Metadata["codex_image_generation_bridge"] != false || incoming.Metadata["codex_image_generation_model"] != "gpt-image-1.5" {
		t.Fatalf("image bridge = %v / %v", incoming.Metadata["codex_image_generation_bridge"], incoming.Metadata["codex_image_generation_model"])
	}
	if incoming.Metadata["disable_cooling"] != true {
		t.Fatalf("disable_cooling = %v, want true", incoming.Metadata["disable_cooling"])
	}
	// Stale tokens, disabled flags, and OAuth error health must never be carried over.
	if incoming.Metadata["access_token"] != "fresh-access" || incoming.Metadata["refresh_token"] != "fresh-refresh" {
		t.Fatalf("new tokens overwritten: %#v", incoming.Metadata)
	}
	if incoming.Disabled || incoming.Status != coreauth.StatusActive {
		t.Fatalf("reauth must be active and enabled, got disabled=%v status=%q", incoming.Disabled, incoming.Status)
	}
	if _, exists := incoming.Metadata[coreauth.CodexOAuthHealthMetadataKey]; exists {
		t.Fatal("stale codex_oauth_health leaked onto incoming record")
	}
}

func TestInheritOperationalSettingsRespectsExplicitNewProxyID(t *testing.T) {
	oldCred := &coreauth.Auth{
		ID:       "tenant-1/codex-alice-go.json",
		Provider: "codex",
		ProxyID:  "old-proxy",
		ProxyURL: "http://old-proxy:8080",
		Metadata: map[string]any{
			"proxy_id":  "old-proxy",
			"proxy_url": "http://old-proxy:8080",
		},
		UpdatedAt: time.Now(),
	}
	incoming := &coreauth.Auth{
		ID:       "tenant-1/codex-alice-pro.json",
		Provider: "codex",
		ProxyID:  "new-proxy",
		Metadata: map[string]any{
			"proxy_id": "new-proxy",
		},
	}

	InheritOperationalSettings(incoming, oldCred)

	if incoming.ProxyID != "new-proxy" || incoming.Metadata["proxy_id"] != "new-proxy" {
		t.Fatalf("explicit new proxy_id overwritten: field=%q meta=%v", incoming.ProxyID, incoming.Metadata["proxy_id"])
	}
	if incoming.ProxyURL != "" || incoming.Metadata["proxy_url"] != nil {
		t.Fatalf("old proxy_url should not be inherited when a new proxy_id was chosen: field=%q meta=%v", incoming.ProxyURL, incoming.Metadata["proxy_url"])
	}
}
