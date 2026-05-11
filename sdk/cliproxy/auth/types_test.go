package auth

import "testing"

func TestToolPrefixDisabled(t *testing.T) {
	var a *Auth
	if a.ToolPrefixDisabled() {
		t.Error("nil auth should return false")
	}

	a = &Auth{}
	if a.ToolPrefixDisabled() {
		t.Error("empty auth should return false")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to true")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": "true"}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to string 'true'")
	}

	a = &Auth{Metadata: map[string]any{"tool-prefix-disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true with kebab-case key")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": false}}
	if a.ToolPrefixDisabled() {
		t.Error("should return false when set to false")
	}
}

func TestKimiTokenAuthReportsOAuthAccountInfo(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		Provider: "kimi",
		Metadata: map[string]any{
			"refresh_token": "kimi-refresh-token",
		},
	}

	accountType, account := auth.AccountInfo()
	if accountType != "oauth" {
		t.Fatalf("accountType = %q, want oauth", accountType)
	}
	if account != "kimi" {
		t.Fatalf("account = %q, want kimi", account)
	}
}
