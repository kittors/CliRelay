package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// OAuth credentials carry a TokenStorage, which used to bypass the disabled
// mirror entirely: the management API reported success, the file kept the old
// value, and the next reload flipped the credential back.
func TestFileTokenStoreSavesDisabledThroughTokenStorage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-account.json")
	metadata := map[string]any{
		"type":          "codex",
		"email":         "account@example.com",
		"access_token":  "at",
		"refresh_token": "rt",
		"disabled":      false,
	}
	writeJSON(t, path, metadata)

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auth := &cliproxyauth.Auth{
		ID:         "codex-account.json",
		FileName:   "codex-account.json",
		Provider:   "codex",
		Status:     cliproxyauth.StatusDisabled,
		Disabled:   true,
		Attributes: map[string]string{"path": path},
		Metadata:   metadata,
		Storage: &codexauth.CodexTokenStorage{
			AccessToken:  "at",
			RefreshToken: "rt",
			Email:        "account@example.com",
			Type:         "codex",
		},
	}
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := readJSON(t, path)["disabled"]; got != true {
		t.Fatalf("on-disk disabled = %#v, want true", got)
	}

	// Re-enabling has to clear the flag again, otherwise the account can be
	// switched off but never back on.
	auth.Disabled = false
	auth.Status = cliproxyauth.StatusActive
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatalf("Save enabled: %v", err)
	}
	if got := readJSON(t, path)["disabled"]; got != false {
		t.Fatalf("on-disk disabled = %#v, want false", got)
	}
}

func TestFileTokenStoreListRestoresDisabled(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "codex-off.json"), map[string]any{
		"type":     "codex",
		"email":    "off@example.com",
		"disabled": true,
	})
	writeJSON(t, filepath.Join(dir, "codex-on.json"), map[string]any{
		"type":     "codex",
		"email":    "on@example.com",
		"disabled": false,
	})

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := make(map[string]*cliproxyauth.Auth, len(auths))
	for _, auth := range auths {
		byID[auth.ID] = auth
	}
	off := byID["codex-off.json"]
	if off == nil || !off.Disabled || off.Status != cliproxyauth.StatusDisabled {
		t.Fatalf("codex-off.json = %+v, want disabled", off)
	}
	on := byID["codex-on.json"]
	if on == nil || on.Disabled || on.Status != cliproxyauth.StatusActive {
		t.Fatalf("codex-on.json = %+v, want active", on)
	}
}

func writeJSON(t *testing.T, path string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := map[string]any{}
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return out
}
