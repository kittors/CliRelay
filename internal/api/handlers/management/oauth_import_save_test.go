package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// cancelSensitiveAuthStore fails a write whose context has ended, as a
// database-backed store does.
type cancelSensitiveAuthStore struct {
	memoryAuthStore
}

func (s *cancelSensitiveAuthStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.memoryAuthStore.Save(ctx, auth)
}

// A Codex refresh-token import spends the pasted token: the record holds the
// only copy of its replacement. If the client hangs up during the exchange,
// the record must still be saved.
func TestImportCredentialSavesAfterTheClientHangsUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &cancelSensitiveAuthStore{}
	h := &Handler{
		cfg:         &config.Config{AuthDir: t.TempDir()},
		authManager: coreauth.NewManager(store, nil, nil),
		tokenStore:  store,
	}
	reqCtx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	h.importKinds = map[string]credentialImportKind{credentialImportAnthropicSession: {
		normalize: internalclaude.NormalizeSessionKey,
		exchange: func(context.Context, *config.Config, credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
			hangUp() // the exchange succeeded upstream, the client is gone
			return &coreauth.Auth{
				ID:       "codex-import.json",
				Provider: "codex",
				FileName: "codex-import.json",
				Metadata: map[string]any{"type": "codex", "refresh_token": "rt-rotated"},
			}, credentialimport.Details{}, nil
		},
	}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/oauth-import/anthropic-session", strings.NewReader(`{"credential":"sessionKey=sk-ant-sid01-test"}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(reqCtx)
	c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: "tenant-acme"}})
	h.ImportAnthropicSession(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	saved, ok := h.authManager.GetByID("tenant-acme/codex-import.json")
	if !ok || saved == nil || saved.Metadata["refresh_token"] != "rt-rotated" {
		t.Fatalf("imported account was not kept after the client hung up; store = %+v", store.items)
	}
}
