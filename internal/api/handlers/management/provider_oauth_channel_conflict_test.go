package management

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func newProviderConfigConflictHandler(t *testing.T, cfg *config.Config) *Handler {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-oauth.json",
		FileName: "claude-oauth.json",
		Provider: "claude",
		Label:    "claude-oauth",
		Metadata: map[string]any{
			"email": "yuan364299311@gmail.com",
		},
	})
	if err != nil {
		t.Fatalf("register oauth auth: %v", err)
	}
	return &Handler{cfg: cfg, configFilePath: configPath, authManager: manager}
}

func TestPutClaudeKeysDropsOAuthDisplayRowsBeforeChannelValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newProviderConfigConflictHandler(t, &config.Config{})

	body := []byte(`[
		{"name":"yuan364299311@gmail.com","api-key":"","account_type":"oauth","runtime_only":true},
		{"name":"opusclaw1","api-key":" claude-api-key "}
	]`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/claude-api-key", bytes.NewReader(body))

	h.PutClaudeKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ClaudeKey) != 1 {
		t.Fatalf("ClaudeKey length = %d, want 1: %+v", len(h.cfg.ClaudeKey), h.cfg.ClaudeKey)
	}
	if h.cfg.ClaudeKey[0].Name != "opusclaw1" || h.cfg.ClaudeKey[0].APIKey != "claude-api-key" {
		t.Fatalf("ClaudeKey[0] = %+v", h.cfg.ClaudeKey[0])
	}
}

func TestPutOpenCodeGoKeysIgnoresStaleEmptyClaudeRowsDuringChannelValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newProviderConfigConflictHandler(t, &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{Name: "yuan364299311@gmail.com"},
		},
	})

	body := []byte(`[{"name":"opencode-go","api-key":" go-api-key "}]`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/opencode-go-api-key", bytes.NewReader(body))

	h.PutOpenCodeGoKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.OpenCodeGoKey) != 1 || h.cfg.OpenCodeGoKey[0].APIKey != "go-api-key" {
		t.Fatalf("OpenCodeGoKey = %+v", h.cfg.OpenCodeGoKey)
	}
}

func TestPutOpenCodeGoKeysIgnoresDuplicateOAuthEmailAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{
			ID:       "codex-yuan.json",
			FileName: "codex-yuan.json",
			Provider: "codex",
			Label:    "GptPlus4",
			Metadata: map[string]any{
				"email": "yuan364299311@gmail.com",
			},
		},
		{
			ID:       "gemini-yuan.json",
			FileName: "gemini-yuan.json",
			Provider: "gemini-cli",
			Metadata: map[string]any{
				"email": "yuan364299311@gmail.com",
			},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register oauth auth %s: %v", auth.ID, err)
		}
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath, authManager: manager}

	body := []byte(`[{"name":"opencode-go","api-key":" go-api-key "}]`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/opencode-go-api-key", bytes.NewReader(body))

	h.PutOpenCodeGoKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
}
