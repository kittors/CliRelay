package authfiles

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexadmission"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestApplyStatusPatchDisablesAuth(t *testing.T) {
	now := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{ID: "codex", Status: coreauth.StatusActive}

	if err := ApplyStatusPatch(auth, true, now); err != nil {
		t.Fatalf("ApplyStatusPatch() error = %v", err)
	}
	if !auth.Disabled {
		t.Fatal("Disabled = false, want true")
	}
	if auth.Status != coreauth.StatusDisabled {
		t.Fatalf("Status = %q, want %q", auth.Status, coreauth.StatusDisabled)
	}
	if auth.StatusMessage != "disabled via management API" {
		t.Fatalf("StatusMessage = %q, want disabled message", auth.StatusMessage)
	}
	if !auth.UpdatedAt.Equal(now) {
		t.Fatalf("UpdatedAt = %v, want %v", auth.UpdatedAt, now)
	}
}

func TestApplyStatusPatchEnablesAuth(t *testing.T) {
	now := time.Date(2026, 6, 6, 11, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:            "codex",
		Disabled:      true,
		Status:        coreauth.StatusDisabled,
		StatusMessage: "disabled via management API",
	}

	if err := ApplyStatusPatch(auth, false, now); err != nil {
		t.Fatalf("ApplyStatusPatch() error = %v", err)
	}
	if auth.Disabled {
		t.Fatal("Disabled = true, want false")
	}
	if auth.Status != coreauth.StatusActive {
		t.Fatalf("Status = %q, want %q", auth.Status, coreauth.StatusActive)
	}
	if auth.StatusMessage != "" {
		t.Fatalf("StatusMessage = %q, want empty", auth.StatusMessage)
	}
	if !auth.UpdatedAt.Equal(now) {
		t.Fatalf("UpdatedAt = %v, want %v", auth.UpdatedAt, now)
	}
}

func TestApplyStatusPatchRejectsMissingAuth(t *testing.T) {
	err := ApplyStatusPatch(nil, true, time.Time{})
	if err == nil || err.Error() != "auth file not found" {
		t.Fatalf("ApplyStatusPatch() error = %v, want not found", err)
	}
}

func TestApplyFieldPatchUpdatesOAuthLabel(t *testing.T) {
	now := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:       "oauth-auth",
		Provider: "codex",
		Label:    "Old Label",
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}

	label := " Team Alpha "
	result, err := ApplyFieldPatch(auth, FieldPatch{Label: &label}, FieldPatchOptions{
		Now: now,
		ValidateLabel: func(label, excludeAuthID string) (string, error) {
			if excludeAuthID != "oauth-auth" {
				t.Fatalf("excludeAuthID = %q, want oauth-auth", excludeAuthID)
			}
			return strings.TrimSpace(label), nil
		},
	})
	if err != nil {
		t.Fatalf("ApplyFieldPatch() error = %v", err)
	}
	if auth.Label != "Team Alpha" {
		t.Fatalf("Label = %q, want Team Alpha", auth.Label)
	}
	if got, _ := auth.Metadata["label"].(string); got != "Team Alpha" {
		t.Fatalf("metadata label = %q, want Team Alpha", got)
	}
	if len(result.OldChannelIdentifiers) == 0 || result.NewChannelLabel != "Team Alpha" {
		t.Fatalf("result = %#v, want old identifiers and new label", result)
	}
	if !auth.UpdatedAt.Equal(now) {
		t.Fatalf("UpdatedAt = %v, want %v", auth.UpdatedAt, now)
	}
}

func TestApplyFieldPatchRejectsNonOAuthLabel(t *testing.T) {
	label := "Team Alpha"
	auth := &coreauth.Auth{
		ID:       "api-key-auth",
		Provider: "openai",
		Attributes: map[string]string{
			"api_key": "redacted",
		},
	}

	_, err := ApplyFieldPatch(auth, FieldPatch{Label: &label}, FieldPatchOptions{})
	if err == nil || err.Error() != "label is only supported for oauth auth files" {
		t.Fatalf("ApplyFieldPatch() error = %v, want label oauth error", err)
	}
}

func TestApplyFieldPatchUpdatesTagsPrefixAndProxyFields(t *testing.T) {
	customTags := []string{" Team ", "priority", "team"}
	hiddenTags := []string{" codex "}
	displayTags := []string{"codex", "priority"}
	prefix := " team-a "
	proxyURL := ""
	proxyID := " proxy-main "
	priority := 3
	auth := &coreauth.Auth{
		ID:       "editable",
		Provider: "codex",
		ProxyURL: "http://old-proxy.example",
		Metadata: map[string]any{
			"proxy_url": "http://old-proxy.example",
			"proxy-url": "legacy",
			"proxyUrl":  "legacy",
		},
	}

	_, err := ApplyFieldPatch(auth, FieldPatch{
		CustomTags:        &customTags,
		HiddenDefaultTags: &hiddenTags,
		DisplayTags:       &displayTags,
		Prefix:            &prefix,
		ProxyURL:          &proxyURL,
		ProxyID:           &proxyID,
		Priority:          &priority,
	}, FieldPatchOptions{})
	if err != nil {
		t.Fatalf("ApplyFieldPatch() error = %v", err)
	}
	if auth.Prefix != "team-a" || auth.ProxyURL != "" || auth.ProxyID != "proxy-main" {
		t.Fatalf("auth fields = prefix %q proxyURL %q proxyID %q", auth.Prefix, auth.ProxyURL, auth.ProxyID)
	}
	if got := auth.Metadata["custom_tags"]; !stringSliceEqual(got, []string{"team", "priority"}) {
		t.Fatalf("custom_tags = %#v, want [team priority]", got)
	}
	if got := auth.Metadata["hidden_default_tags"]; !stringSliceEqual(got, []string{"codex"}) {
		t.Fatalf("hidden_default_tags = %#v, want [codex]", got)
	}
	if got := auth.Metadata["display_tags"]; !stringSliceEqual(got, []string{"codex", "priority"}) {
		t.Fatalf("display_tags = %#v, want [codex priority]", got)
	}
	if _, ok := auth.Metadata["proxy_url"]; ok {
		t.Fatalf("proxy_url still present: %#v", auth.Metadata["proxy_url"])
	}
	if _, ok := auth.Metadata["proxy-url"]; ok {
		t.Fatalf("proxy-url still present: %#v", auth.Metadata["proxy-url"])
	}
	if _, ok := auth.Metadata["proxyUrl"]; ok {
		t.Fatalf("proxyUrl still present: %#v", auth.Metadata["proxyUrl"])
	}
	if got, _ := auth.Metadata["proxy_id"].(string); got != "proxy-main" {
		t.Fatalf("proxy_id = %q, want proxy-main", got)
	}
	if got, _ := auth.Metadata["priority"].(int); got != 3 {
		t.Fatalf("priority = %d, want 3", got)
	}
}

func TestApplyFieldPatchRejectsTooManyCustomTags(t *testing.T) {
	customTags := []string{"one", "two", "three", "four"}
	auth := &coreauth.Auth{ID: "editable", Provider: "codex"}

	_, err := ApplyFieldPatch(auth, FieldPatch{CustomTags: &customTags}, FieldPatchOptions{})
	if err == nil || err.Error() != "custom_tags supports at most 3 items" {
		t.Fatalf("ApplyFieldPatch() error = %v, want too many tags", err)
	}
}

func TestApplyFieldPatchUpdatesSubscriptionFields(t *testing.T) {
	startedAt := "2026-04-01T09:30:00Z"
	period := "year"
	auth := &coreauth.Auth{
		ID:       "subscription",
		Provider: "codex",
		Metadata: map[string]any{
			"subscriptionExpiresAt": "2026-05-01T09:30:00Z",
		},
	}

	_, err := ApplyFieldPatch(auth, FieldPatch{
		SubscriptionStartedAt: &startedAt,
		SubscriptionPeriod:    &period,
	}, FieldPatchOptions{})
	if err != nil {
		t.Fatalf("ApplyFieldPatch() error = %v", err)
	}
	if got, _ := auth.Metadata["subscription_started_at"].(string); got != "2026-04-01T09:30:00Z" {
		t.Fatalf("subscription_started_at = %q, want canonical start", got)
	}
	if got, _ := auth.Metadata["subscription_period"].(string); got != "yearly" {
		t.Fatalf("subscription_period = %q, want yearly", got)
	}
	if _, ok := auth.Metadata["subscriptionExpiresAt"]; ok {
		t.Fatalf("legacy expiration key still present")
	}
}

func TestApplyFieldPatchRejectsInvalidSubscriptionExpiration(t *testing.T) {
	expiresAt := "not-a-time"
	auth := &coreauth.Auth{ID: "subscription", Provider: "codex"}

	_, err := ApplyFieldPatch(auth, FieldPatch{SubscriptionExpiresAt: &expiresAt}, FieldPatchOptions{})
	if err == nil || err.Error() != "subscription_expires_at must be a valid time" {
		t.Fatalf("ApplyFieldPatch() error = %v, want invalid expiration", err)
	}
}

func TestApplyFieldPatchUpdatesCodexOAuthAdmission(t *testing.T) {
	now := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	enabled := true
	allowedClients := []string{" claude_code ", "CLAUDE_CODE"}
	auth := &coreauth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "codex@example.com",
		},
	}

	_, err := ApplyFieldPatch(auth, FieldPatch{
		CodexCLIOnly:               &enabled,
		CodexCLIOnlyAllowedClients: &allowedClients,
	}, FieldPatchOptions{Now: now})
	if err != nil {
		t.Fatalf("ApplyFieldPatch() error = %v", err)
	}
	if got, _ := auth.Metadata["codex_cli_only"].(bool); !got {
		t.Fatalf("codex_cli_only = %#v, want true", auth.Metadata["codex_cli_only"])
	}
	gotAllowed, ok := auth.Metadata["codex_cli_only_allowed_clients"].([]string)
	if !ok {
		t.Fatalf("codex_cli_only_allowed_clients = %#v, want []string", auth.Metadata["codex_cli_only_allowed_clients"])
	}
	if len(gotAllowed) != 1 || gotAllowed[0] != codexadmission.AllowedClientClaudeCode {
		t.Fatalf("allowed clients = %#v, want [claude_code]", gotAllowed)
	}
	if !auth.UpdatedAt.Equal(now) {
		t.Fatalf("UpdatedAt = %v, want %v", auth.UpdatedAt, now)
	}
}

func TestApplyFieldPatchClearsCodexOAuthAllowedClients(t *testing.T) {
	allowedClients := []string{}
	auth := &coreauth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Metadata: map[string]any{
			"codex_cli_only_allowed_clients": []string{codexadmission.AllowedClientClaudeCode},
		},
	}

	_, err := ApplyFieldPatch(auth, FieldPatch{
		CodexCLIOnlyAllowedClients: &allowedClients,
	}, FieldPatchOptions{})
	if err != nil {
		t.Fatalf("ApplyFieldPatch() error = %v", err)
	}
	if _, ok := auth.Metadata["codex_cli_only_allowed_clients"]; ok {
		t.Fatalf("codex_cli_only_allowed_clients still present: %#v", auth.Metadata)
	}
}

func TestApplyFieldPatchRejectsCodexOAuthAdmissionForAPIKeyAuth(t *testing.T) {
	enabled := true
	auth := &coreauth.Auth{
		ID:       "codex-api-key",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "sk-test",
		},
	}

	_, err := ApplyFieldPatch(auth, FieldPatch{CodexCLIOnly: &enabled}, FieldPatchOptions{})
	if err == nil || !strings.Contains(err.Error(), "only supported for Codex OAuth") {
		t.Fatalf("ApplyFieldPatch() error = %v, want Codex OAuth restriction", err)
	}
}

func TestApplyFieldPatchRejectsUnknownCodexAllowedClientPreset(t *testing.T) {
	allowedClients := []string{"unknown_client"}
	auth := &coreauth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "codex@example.com",
		},
	}

	_, err := ApplyFieldPatch(auth, FieldPatch{
		CodexCLIOnlyAllowedClients: &allowedClients,
	}, FieldPatchOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown codex allowed client preset") {
		t.Fatalf("ApplyFieldPatch() error = %v, want unknown preset error", err)
	}
}

func TestApplyFieldPatchRejectsNoFields(t *testing.T) {
	auth := &coreauth.Auth{ID: "empty", Provider: "codex"}

	_, err := ApplyFieldPatch(auth, FieldPatch{}, FieldPatchOptions{})
	if err == nil || err.Error() != "no fields to update" {
		t.Fatalf("ApplyFieldPatch() error = %v, want no fields", err)
	}
}

func stringSliceEqual(value any, want []string) bool {
	got, ok := value.([]string)
	if !ok || len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
