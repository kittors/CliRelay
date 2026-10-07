package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// newImportTestHandler wires a handler with an in-memory store and a canned
// exchange, so the HTTP path is exercised without dialling any upstream.
func newImportTestHandler(t *testing.T, kind credentialImportKind, proxy []config.ProxyPoolEntry) (*Handler, *memoryAuthStore) {
	t.Helper()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := &Handler{
		cfg:         &config.Config{AuthDir: t.TempDir(), ProxyPool: proxy},
		authManager: manager,
		tokenStore:  store,
		importKinds: map[string]credentialImportKind{credentialImportAnthropicSession: kind},
	}
	return h, store
}

func postImport(t *testing.T, h *Handler, tenantID, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/oauth-import/anthropic-session", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	if tenantID != "" {
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
	}
	h.ImportAnthropicSession(c)
	return rec
}

func TestImportCredentialSavesAndBindsProxy(t *testing.T) {
	var gotInput credentialImportInput
	kind := credentialImportKind{
		normalize: internalclaude.NormalizeSessionKey,
		exchange: func(_ context.Context, _ *config.Config, in credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
			gotInput = in
			return &coreauth.Auth{
					ID:       "claude-import.json",
					Provider: "claude",
					FileName: "claude-import.json",
					Label:    "team.member@example.com",
					Metadata: map[string]any{"type": "claude"},
				}, credentialimport.Details{
					Email:        "team.member@example.com",
					Organization: "Acme Team",
				}, nil
		},
	}
	h, store := newImportTestHandler(t, kind, []config.ProxyPoolEntry{{ID: "pp-1", URL: "http://proxy.internal:8080", Enabled: true}})

	rec := postImport(t, h, "tenant-acme", `{"credential":"sessionKey=sk-ant-sid01-test; other=1","proxy_id":"pp-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["email"] != "team.member@example.com" || resp["organization"] != "Acme Team" || resp["provider"] != "claude" {
		t.Fatalf("response = %+v", resp)
	}
	// The handler must hand the exchange the normalized key and the resolved
	// proxy URL, not the raw cookie string.
	if gotInput.Credential != "sk-ant-sid01-test" {
		t.Fatalf("exchange got credential %q, want the normalized key", gotInput.Credential)
	}
	if gotInput.ProxyURL != "http://proxy.internal:8080" {
		t.Fatalf("exchange got proxy %q, want the resolved pool URL", gotInput.ProxyURL)
	}

	saved, ok := h.authManager.GetByID("tenant-acme/claude-import.json")
	if !ok || saved == nil {
		t.Fatalf("account not saved under tenant; store = %+v", store.items)
	}
	if saved.ProxyID != "pp-1" || saved.Metadata["proxy_id"] != "pp-1" {
		t.Fatalf("proxy binding = %q / %v", saved.ProxyID, saved.Metadata["proxy_id"])
	}
	if saved.TenantID != "tenant-acme" {
		t.Fatalf("tenant = %q", saved.TenantID)
	}
	wantPath := filepath.Join(h.cfg.AuthDir, "tenant-acme", "claude-import.json")
	if saved.Attributes["path"] != wantPath {
		t.Fatalf("path = %q, want %q", saved.Attributes["path"], wantPath)
	}
}

func TestImportCredentialRejectsMissingAndMalformed(t *testing.T) {
	exchangeCalled := false
	kind := credentialImportKind{
		normalize: internalclaude.NormalizeSessionKey,
		exchange: func(context.Context, *config.Config, credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
			exchangeCalled = true
			return nil, credentialimport.Details{}, nil
		},
	}
	h, _ := newImportTestHandler(t, kind, nil)

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"empty body", ``, http.StatusBadRequest, importCodeRequestInvalid},
		{"blank credential", `{"credential":"   "}`, http.StatusBadRequest, importCodeCredentialRequired},
		{"not a session key", `{"credential":"_ga=GA1; cf_clearance=x"}`, http.StatusUnprocessableEntity, importCodeCredentialMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postImport(t, h, "", tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var resp map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp["code"] != tc.wantCode {
				t.Fatalf("code = %v, want %q", resp["code"], tc.wantCode)
			}
		})
	}
	if exchangeCalled {
		t.Fatal("a credential rejected locally must never reach the upstream")
	}
}

func TestImportCredentialMapsUpstreamFailures(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"invalid", fmt.Errorf("%w: refused", credentialimport.ErrInvalidCredential), http.StatusUnprocessableEntity, importCodeCredentialInvalid},
		{"no org", fmt.Errorf("%w", credentialimport.ErrNoOrganization), http.StatusUnprocessableEntity, importCodeNoOrganization},
		{"denied", fmt.Errorf("%w", credentialimport.ErrDenied), http.StatusUnprocessableEntity, importCodeAuthorizationDenied},
		{"blocked", fmt.Errorf("%w", credentialimport.ErrBlocked), http.StatusBadGateway, importCodeUpstreamBlocked},
		{"timeout", context.DeadlineExceeded, http.StatusGatewayTimeout, importCodeUpstreamTimeout},
		{"other", errors.New("socket closed"), http.StatusBadGateway, importCodeUpstreamError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind := credentialImportKind{
				normalize: internalclaude.NormalizeSessionKey,
				exchange: func(context.Context, *config.Config, credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
					return nil, credentialimport.Details{}, tc.err
				},
			}
			h, _ := newImportTestHandler(t, kind, nil)
			rec := postImport(t, h, "", `{"credential":"sk-ant-sid01-test"}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var resp map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp["code"] != tc.wantCode {
				t.Fatalf("code = %v, want %q", resp["code"], tc.wantCode)
			}
		})
	}
}

// A refused credential must never be echoed back in the error the panel shows.
func TestImportCredentialNeverEchoesTheSecret(t *testing.T) {
	const secret = "sk-ant-sid01-super-secret-value"
	kind := credentialImportKind{
		normalize: internalclaude.NormalizeSessionKey,
		exchange: func(context.Context, *config.Config, credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
			// Upstreams sometimes echo what they were sent; the handler must mask it.
			return nil, credentialimport.Details{}, fmt.Errorf("%w: upstream said %s is bad", credentialimport.ErrInvalidCredential, secret)
		},
	}
	h, _ := newImportTestHandler(t, kind, nil)
	rec := postImport(t, h, "", `{"credential":"`+secret+`"}`)
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("response leaked the credential: %s", rec.Body.String())
	}
}

func TestImportCredentialUnknownProvider(t *testing.T) {
	h, _ := newImportTestHandler(t, credentialImportKind{}, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/oauth-import/codex-refresh-token", strings.NewReader(`{"credential":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ImportCodexRefreshToken(c) // not in the injected map
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
