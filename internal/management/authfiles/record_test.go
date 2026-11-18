package authfiles

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBuildRecordMapsMetadataToAuth(t *testing.T) {
	authDir := t.TempDir()
	now := time.Date(2026, 6, 6, 11, 0, 0, 0, time.UTC)
	lastRefresh := time.Date(2026, 6, 6, 10, 30, 0, 0, time.UTC)
	path := filepath.Join(authDir, "claude-pro.json")
	metadata := map[string]any{
		"email":        "pro@example.com",
		"prefix":       "team-a",
		"proxy-url":    "http://proxy.example",
		"proxy_id":     "premium-egress",
		"last_refresh": lastRefresh.Format(time.RFC3339),
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	auth := BuildRecord(RecordOptions{
		AuthDir:  authDir,
		Path:     path,
		Provider: "claude",
		Metadata: metadata,
		Now:      now,
	})
	if auth == nil {
		t.Fatal("BuildRecord() = nil")
	}
	if auth.ID != "claude-pro.json" || auth.FileName != "claude-pro.json" {
		t.Fatalf("ID/FileName = %q/%q, want claude-pro.json", auth.ID, auth.FileName)
	}
	if auth.Provider != "claude" || auth.Label != "pro@example.com" || auth.Status != coreauth.StatusActive {
		t.Fatalf("provider/label/status = %q/%q/%q", auth.Provider, auth.Label, auth.Status)
	}
	if auth.Prefix != "team-a" || auth.ProxyURL != "http://proxy.example" || auth.ProxyID != "premium-egress" {
		t.Fatalf("routing fields = %q/%q/%q", auth.Prefix, auth.ProxyURL, auth.ProxyID)
	}
	if auth.Attributes["path"] != path || auth.Attributes["source"] != path {
		t.Fatalf("attributes = %#v, want path/source", auth.Attributes)
	}
	auth.Metadata["test_marker"] = "kept"
	if metadata["test_marker"] != "kept" {
		t.Fatalf("metadata map was not preserved")
	}
	if !auth.CreatedAt.Equal(now) || !auth.UpdatedAt.Equal(now) || !auth.LastRefreshedAt.Equal(lastRefresh) {
		t.Fatalf("timestamps = created %v updated %v refresh %v", auth.CreatedAt, auth.UpdatedAt, auth.LastRefreshedAt)
	}
}

func TestBuildRecordPreservesExistingRuntimeFields(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	createdAt := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	lastRefresh := time.Date(2026, 1, 3, 3, 4, 0, 0, time.UTC)
	nextRefresh := time.Date(2026, 1, 4, 3, 4, 0, 0, time.UTC)
	runtime := struct{ Value string }{Value: "kept"}

	auth := BuildRecord(RecordOptions{
		AuthDir:  authDir,
		Path:     path,
		Provider: "codex",
		Metadata: map[string]any{"email": "codex@example.com"},
		Existing: &coreauth.Auth{
			CreatedAt:        createdAt,
			LastRefreshedAt:  lastRefresh,
			NextRefreshAfter: nextRefresh,
			Runtime:          runtime,
		},
	})
	if auth == nil {
		t.Fatal("BuildRecord() = nil")
	}
	if !auth.CreatedAt.Equal(createdAt) || !auth.LastRefreshedAt.Equal(lastRefresh) || !auth.NextRefreshAfter.Equal(nextRefresh) {
		t.Fatalf("existing timestamps not preserved: %#v", auth)
	}
	if auth.Runtime != runtime {
		t.Fatalf("Runtime = %#v, want %#v", auth.Runtime, runtime)
	}
}

func TestBuildRecordMetadataLastRefreshOverridesExisting(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	metadataRefresh := time.Date(2026, 2, 3, 4, 5, 0, 0, time.UTC)

	auth := BuildRecord(RecordOptions{
		AuthDir:  authDir,
		Path:     path,
		Provider: "codex",
		Metadata: map[string]any{"last_refresh": metadataRefresh.Format(time.RFC3339)},
		Existing: &coreauth.Auth{
			LastRefreshedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	if auth == nil {
		t.Fatal("BuildRecord() = nil")
	}
	if !auth.LastRefreshedAt.Equal(metadataRefresh) {
		t.Fatalf("LastRefreshedAt = %v, want metadata value %v", auth.LastRefreshedAt, metadataRefresh)
	}
}

func TestChannelLabelFromMetadataFallbacks(t *testing.T) {
	if got := ChannelLabelFromMetadata(map[string]any{"label": " Team A ", "email": "user@example.com"}, "codex"); got != "Team A" {
		t.Fatalf("label fallback = %q, want Team A", got)
	}
	if got := ChannelLabelFromMetadata(map[string]any{"email": " user@example.com "}, "codex"); got != "user@example.com" {
		t.Fatalf("email fallback = %q, want user@example.com", got)
	}
	if got := ChannelLabelFromMetadata(nil, " codex "); got != "codex" {
		t.Fatalf("provider fallback = %q, want codex", got)
	}
}

func TestBuildRecordXAIImportMapsOAuthAttributes(t *testing.T) {
	authDir := t.TempDir()
	tenantDir := filepath.Join(authDir, "tenant-b")
	if err := os.MkdirAll(tenantDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(tenantDir, "xai-user.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	metadata := map[string]any{
		"type":          "xai",
		"email":         "grok@example.com",
		"auth_kind":     "oauth",
		"base_url":      "https://api.x.ai/v1",
		"using_api":     false,
		"access_token":  "access",
		"refresh_token": "refresh",
		"sub":           "principal-1",
	}

	auth := BuildRecord(RecordOptions{
		AuthDir:  authDir,
		TenantID: "tenant-b",
		Path:     path,
		Provider: "xai",
		Metadata: metadata,
	})
	if auth == nil {
		t.Fatal("BuildRecord() = nil")
	}
	// FileName must be tenant-relative so EnsureIndex matches disk reload / quota probes.
	if auth.ID != "tenant-b/xai-user.json" || auth.FileName != "tenant-b/xai-user.json" {
		t.Fatalf("ID/FileName = %q/%q, want tenant-b/xai-user.json", auth.ID, auth.FileName)
	}
	if auth.Disabled {
		t.Fatal("Disabled = true, want false")
	}
	if auth.Attributes["auth_kind"] != "oauth" {
		t.Fatalf("auth_kind attribute = %q", auth.Attributes["auth_kind"])
	}
	if auth.Attributes["base_url"] != "https://api.x.ai/v1" {
		t.Fatalf("base_url attribute = %q", auth.Attributes["base_url"])
	}
	if auth.Attributes["using_api"] != "false" {
		t.Fatalf("using_api attribute = %q", auth.Attributes["using_api"])
	}
	if auth.Attributes["email"] != "grok@example.com" {
		t.Fatalf("email attribute = %q", auth.Attributes["email"])
	}
	if auth.Label != "grok@example.com" {
		t.Fatalf("Label = %q", auth.Label)
	}
}

func TestBuildRecordDisabledStatusFromMetadata(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "disabled.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	auth := BuildRecord(RecordOptions{
		AuthDir:  authDir,
		Path:     path,
		Provider: "claude",
		Metadata: map[string]any{"disabled": true, "email": "off@example.com"},
	})
	if auth == nil {
		t.Fatal("BuildRecord() = nil")
	}
	if !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("disabled/status = %v/%q", auth.Disabled, auth.Status)
	}
}
