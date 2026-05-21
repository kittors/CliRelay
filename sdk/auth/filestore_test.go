package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestExtractAccessToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata map[string]any
		expected string
	}{
		{
			"antigravity top-level access_token",
			map[string]any{"access_token": "tok-abc"},
			"tok-abc",
		},
		{
			"gemini nested token.access_token",
			map[string]any{
				"token": map[string]any{"access_token": "tok-nested"},
			},
			"tok-nested",
		},
		{
			"top-level takes precedence over nested",
			map[string]any{
				"access_token": "tok-top",
				"token":        map[string]any{"access_token": "tok-nested"},
			},
			"tok-top",
		},
		{
			"empty metadata",
			map[string]any{},
			"",
		},
		{
			"whitespace-only access_token",
			map[string]any{"access_token": "   "},
			"",
		},
		{
			"wrong type access_token",
			map[string]any{"access_token": 12345},
			"",
		},
		{
			"token is not a map",
			map[string]any{"token": "not-a-map"},
			"",
		},
		{
			"nested whitespace-only",
			map[string]any{
				"token": map[string]any{"access_token": "  "},
			},
			"",
		},
		{
			"fallback to nested when top-level empty",
			map[string]any{
				"access_token": "",
				"token":        map[string]any{"access_token": "tok-fallback"},
			},
			"tok-fallback",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractAccessToken(tt.metadata)
			if got != tt.expected {
				t.Errorf("extractAccessToken() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFileTokenStoreListAppliesRoutingMetadata(t *testing.T) {
	dir := t.TempDir()
	fileName := "claude-max.json"
	data := []byte(`{"type":"claude","email":"max@example.com","prefix":"team-b","proxy_url":"http://auth-proxy.local:8080","proxy_id":"premium-egress"}`)
	if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Prefix != "team-b" {
		t.Fatalf("Prefix = %q, want team-b", auth.Prefix)
	}
	if auth.ProxyURL != "http://auth-proxy.local:8080" {
		t.Fatalf("ProxyURL = %q, want auth proxy", auth.ProxyURL)
	}
	if auth.ProxyID != "premium-egress" {
		t.Fatalf("ProxyID = %q, want premium-egress", auth.ProxyID)
	}
}

func TestFileTokenStoreListInfersCodexProviderForOpenAIOAuthJSON(t *testing.T) {
	dir := t.TempDir()
	fileName := "openai-oauth.json"
	data := []byte(`{"chatgpt_account_id":"acct-123","client_id":"app_test","access_token":"access-token","id_token":"id-token","email":"subscriber@example.com","plan_type":"plus"}`)
	if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if auths[0].Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", auths[0].Provider)
	}
	if auths[0].Metadata["type"] != "codex" {
		t.Fatalf("metadata type = %#v, want codex", auths[0].Metadata["type"])
	}
}

func TestFileTokenStoreListNormalizesOpenAIBundleJSONForCodex(t *testing.T) {
	dir := t.TempDir()
	fileName := "openai-bundle.json"
	accountID := "acct-bundle"
	issuedAt := int64(1_779_210_280)
	expiresAt := int64(1_780_074_280)
	accessToken := makeJWTForTest(t, map[string]any{
		"iat": issuedAt,
		"exp": expiresAt,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	idToken := makeJWTForTest(t, map[string]any{
		"email": "bundle@example.com",
		"iat":   issuedAt,
		"exp":   expiresAt,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	data, err := json.Marshal(map[string]any{
		"version":              1,
		"platform":             "openai",
		"account_claims_email": "bundle@example.com",
		"access_token":         accessToken,
		"id_token":             idToken,
		"refresh_token":        "",
		"client_id":            "app_test",
		"chatgpt_account_id":   accountID,
		"disabled":             false,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	wantExpired := time.Unix(expiresAt, 0).UTC().Format(time.RFC3339)
	wantLastRefresh := time.Unix(issuedAt, 0).UTC().Format(time.RFC3339)
	for key, want := range map[string]any{
		"type":         "codex",
		"account_id":   accountID,
		"email":        "bundle@example.com",
		"expired":      wantExpired,
		"last_refresh": wantLastRefresh,
		"plan_type":    "plus",
	} {
		if got := auths[0].Metadata[key]; got != want {
			t.Fatalf("metadata[%s] = %#v, want %#v", key, got, want)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("Unmarshal persisted: %v", err)
	}
	if persisted["account_id"] != accountID || persisted["type"] != "codex" {
		t.Fatalf("persisted normalized fields = %#v", persisted)
	}
}

func TestFileTokenStoreSavePersistsRoutingMetadata(t *testing.T) {
	dir := t.TempDir()
	fileName := "claude-pro.json"
	store := NewFileTokenStore()
	store.SetBaseDir(dir)

	_, err := store.Save(context.Background(), &cliproxyauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "claude",
		Prefix:   "team-c",
		ProxyURL: "http://auth-proxy.local:8080",
		ProxyID:  "premium-egress",
		Metadata: map[string]any{
			"type":  "claude",
			"email": "pro@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if metadata["prefix"] != "team-c" {
		t.Fatalf("prefix = %#v, want team-c", metadata["prefix"])
	}
	if metadata["proxy_url"] != "http://auth-proxy.local:8080" {
		t.Fatalf("proxy_url = %#v, want auth proxy", metadata["proxy_url"])
	}
	if metadata["proxy_id"] != "premium-egress" {
		t.Fatalf("proxy_id = %#v, want premium-egress", metadata["proxy_id"])
	}
}
