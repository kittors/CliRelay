package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	codexprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/codex"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type failingSaveStore struct {
	saveErr error
	deleted []string
}

func (s *failingSaveStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (s *failingSaveStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", s.saveErr
}
func (s *failingSaveStore) Delete(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func TestSaveTenantTokenRecordReplacesPlanAliasInheritsSettingsAndKeepsRouting(t *testing.T) {
	ctx := context.Background()
	authDir := t.TempDir()
	tenantID := "tenant-potato"

	fileStore := sdkAuth.NewFileTokenStore()
	fileStore.SetBaseDir(authDir)
	manager := coreauth.NewManager(fileStore, nil, nil)

	oldFileName := "codex-rojasteresita717@gmail.com-go.json"
	oldID := tenantID + "/" + oldFileName
	oldPath := managementauthfiles.TenantFilePath(authDir, tenantID, oldFileName)
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte(`{"type":"codex","email":"rojasteresita717@gmail.com"}`), 0o600); err != nil {
		t.Fatalf("write old auth: %v", err)
	}

	oldAuth := &coreauth.Auth{
		ID:       oldID,
		TenantID: tenantID,
		Provider: "codex",
		FileName: oldFileName,
		ProxyID:  "proxy-204-1-126-176-443",
		Label:    "rojasteresita717@gmail.com",
		Status:   coreauth.StatusError,
		Attributes: map[string]string{
			"path": oldPath,
		},
		Metadata: map[string]any{
			"type":                    "codex",
			"email":                   "rojasteresita717@gmail.com",
			"account_id":              "acct-rojas",
			"chatgpt_user_id":         "user-rojas",
			"plan_type":               "go",
			"proxy_id":                "proxy-204-1-126-176-443",
			"concurrency_limit":       float64(4),
			"codex_convergence_mode":  "session",
			"subscription_started_at": "2026-11-08T00:00:00Z",
			"subscription_period":     "monthly",
			"access_token":            "dead-access",
			"refresh_token":           "dead-refresh",
			coreauth.CodexOAuthHealthMetadataKey: map[string]any{
				"status":        "reauth_required",
				"reauth_reason": "token_revoked",
			},
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), oldAuth); err != nil {
		t.Fatalf("register old auth: %v", err)
	}

	// Register an identical account in a different tenant to verify strict tenant isolation.
	otherTenantID := "tenant-other"
	otherOldPath := managementauthfiles.TenantFilePath(authDir, otherTenantID, oldFileName)
	if err := os.MkdirAll(filepath.Dir(otherOldPath), 0o700); err != nil {
		t.Fatalf("mkdir other tenant: %v", err)
	}
	if err := os.WriteFile(otherOldPath, []byte(`{"type":"codex","email":"rojasteresita717@gmail.com"}`), 0o600); err != nil {
		t.Fatalf("write other tenant auth: %v", err)
	}
	otherAuth := oldAuth.Clone()
	otherAuth.ID = otherTenantID + "/" + oldFileName
	otherAuth.TenantID = otherTenantID
	otherAuth.Attributes["path"] = otherOldPath
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), otherAuth); err != nil {
		t.Fatalf("register other tenant auth: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{AuthDir: authDir},
		authManager: manager,
		tokenStore:  fileStore,
	}

	newStorage := &internalcodex.CodexTokenStorage{
		AccessToken:  "fresh-pro-access",
		RefreshToken: "fresh-pro-refresh",
		AccountID:    "acct-rojas",
		Email:        "rojasteresita717@gmail.com",
		Expire:       time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339),
		LastRefresh:  time.Now().UTC().Format(time.RFC3339),
	}
	newRecord := codexprovider.RecordFromTokenStorage(newStorage)
	newRecord.FileName = "codex-rojasteresita717@gmail.com-pro.json"
	newRecord.ID = newRecord.FileName
	newRecord.Metadata["plan_type"] = "pro"
	newRecord.Metadata["chatgpt_user_id"] = "user-rojas"

	savedPath, err := h.saveTenantTokenRecord(ctx, tenantID, newRecord)
	if err != nil {
		t.Fatalf("saveTenantTokenRecord() error = %v", err)
	}

	// Old -go.json must be removed from disk and manager; only -pro.json remains in tenant-potato.
	if _, statErr := os.Stat(oldPath); !os.IsNotExist(statErr) {
		t.Fatalf("old -go.json file should be deleted, stat err = %v", statErr)
	}
	if _, exists := manager.GetByID(oldID); exists {
		t.Fatalf("old -go.json still present in manager")
	}
	tenantAuths := manager.ListForTenant(tenantID)
	if len(tenantAuths) != 1 {
		t.Fatalf("tenant auth count = %d, want 1", len(tenantAuths))
	}
	got := tenantAuths[0]
	if got.FileName != "codex-rojasteresita717@gmail.com-pro.json" {
		t.Fatalf("remaining file = %q, want -pro.json", got.FileName)
	}
	if got.ProxyID != "proxy-204-1-126-176-443" {
		t.Fatalf("ProxyID = %q, want inherited proxy", got.ProxyID)
	}
	if got.Metadata["concurrency_limit"] != 4 || got.Metadata["codex_convergence_mode"] != "session" {
		t.Fatalf("inherited operational settings missing: %#v", got.Metadata)
	}
	if got.Status != coreauth.StatusActive || got.Disabled {
		t.Fatalf("new record status=%q disabled=%v, want active/false", got.Status, got.Disabled)
	}

	// Verify persisted JSON file on disk also has the inherited operational settings and new tokens.
	raw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal saved file: %v", err)
	}
	if persisted["proxy_id"] != "proxy-204-1-126-176-443" || persisted["codex_convergence_mode"] != "session" || persisted["access_token"] != "fresh-pro-access" {
		t.Fatalf("persisted JSON mismatch: %#v", persisted)
	}
	if _, hasHealth := persisted[coreauth.CodexOAuthHealthMetadataKey]; hasHealth {
		t.Fatalf("persisted JSON carried stale codex_oauth_health: %#v", persisted)
	}

	// Other tenant's credential must be completely untouched.
	if _, exists := manager.GetByID(otherAuth.ID); !exists {
		t.Fatal("other tenant credential was unexpectedly removed")
	}
	if _, statErr := os.Stat(otherOldPath); statErr != nil {
		t.Fatalf("other tenant file was unexpectedly removed: %v", statErr)
	}
}

func TestSaveTenantTokenRecordSameNameReauthClears401AndKeepsSettings(t *testing.T) {
	ctx := context.Background()
	authDir := t.TempDir()
	tenantID := "tenant-potato"

	fileStore := sdkAuth.NewFileTokenStore()
	fileStore.SetBaseDir(authDir)
	manager := coreauth.NewManager(fileStore, nil, nil)

	fileName := "codex-rojasteresita717@gmail.com-pro.json"
	authID := tenantID + "/" + fileName
	existing := &coreauth.Auth{
		ID:       authID,
		TenantID: tenantID,
		Provider: "codex",
		FileName: fileName,
		ProxyID:  "proxy-existing",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":              "codex",
			"email":             "rojasteresita717@gmail.com",
			"account_id":        "acct-rojas",
			"chatgpt_user_id":   "user-rojas",
			"proxy_id":          "proxy-existing",
			"concurrency_limit": float64(6),
			"access_token":      "old-access",
			"refresh_token":     "old-refresh",
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), existing); err != nil {
		t.Fatalf("register existing: %v", err)
	}
	manager.MarkResult(ctx, coreauth.Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    "gpt-5-codex",
		Error:    &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: `{"error":{"code":"token_revoked"}}`},
	})

	h := &Handler{
		cfg:         &config.Config{AuthDir: authDir},
		authManager: manager,
		tokenStore:  fileStore,
	}

	relogin := &coreauth.Auth{
		Provider: "codex",
		FileName: fileName,
		Metadata: map[string]any{
			"type":            "codex",
			"email":           "rojasteresita717@gmail.com",
			"account_id":      "acct-rojas",
			"chatgpt_user_id": "user-rojas",
			"access_token":    "new-access",
			"refresh_token":   "new-refresh",
		},
	}
	if _, err := h.saveTenantTokenRecord(ctx, tenantID, relogin); err != nil {
		t.Fatalf("saveTenantTokenRecord() error = %v", err)
	}

	got, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("expected updated auth in manager")
	}
	if got.ProxyID != "proxy-existing" || got.Metadata["concurrency_limit"] != 6 {
		t.Fatalf("settings lost on same-name reauth: proxy=%q meta=%#v", got.ProxyID, got.Metadata)
	}
	if got.Status != coreauth.StatusActive || got.Unavailable {
		t.Fatalf("reauth stayed degraded: status=%q unavailable=%v msg=%q", got.Status, got.Unavailable, got.StatusMessage)
	}
	if len(got.ModelStates) != 0 {
		t.Fatalf("401 model cooldowns were not cleared on same-name reauth: %+v", got.ModelStates)
	}
	if _, hasHealth := got.Metadata[coreauth.CodexOAuthHealthMetadataKey]; hasHealth {
		t.Fatalf("stale codex_oauth_health was not cleared on same-name reauth: %#v", got.Metadata[coreauth.CodexOAuthHealthMetadataKey])
	}
}

func TestSaveTenantTokenRecordDoesNotDeleteOldAliasWhenSaveFails(t *testing.T) {
	ctx := context.Background()
	authDir := t.TempDir()
	tenantID := "tenant-potato"

	store := &failingSaveStore{saveErr: errors.New("disk full")}
	manager := coreauth.NewManager(nil, nil, nil)
	oldID := tenantID + "/codex-alice@example.com-go.json"
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
		ID:       oldID,
		TenantID: tenantID,
		Provider: "codex",
		FileName: "codex-alice@example.com-go.json",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":            "codex",
			"email":           "alice@example.com",
			"account_id":      "acct-alice",
			"chatgpt_user_id": "user-alice",
		},
	}); err != nil {
		t.Fatalf("register old: %v", err)
	}

	h := &Handler{
		cfg:         &config.Config{AuthDir: authDir},
		authManager: manager,
		tokenStore:  store,
	}
	_, err := h.saveTenantTokenRecord(ctx, tenantID, &coreauth.Auth{
		Provider: "codex",
		FileName: "codex-alice@example.com-pro.json",
		Metadata: map[string]any{
			"type":            "codex",
			"email":           "alice@example.com",
			"account_id":      "acct-alice",
			"chatgpt_user_id": "user-alice",
		},
	})
	if err == nil {
		t.Fatal("expected save failure")
	}
	if _, exists := manager.GetByID(oldID); !exists {
		t.Fatal("old alias was deleted even though saving the new record failed")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("store.Delete called despite save failure: %v", store.deleted)
	}
}
