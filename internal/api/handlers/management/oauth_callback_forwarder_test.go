package management

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestRequestAnthropicTokenUsesAvailableLocalCallbackWhenPreferredPortBusy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	busy, err := net.Listen("tcp", "127.0.0.1:54545")
	if err != nil {
		t.Skipf("anthropic callback port already unavailable: %v", err)
	}
	defer func() { _ = busy.Close() }()

	useIsolatedOAuthSessions(t, oauthCallbackWaitTimeout)

	h := &Handler{
		cfg: &config.Config{
			AuthDir: t.TempDir(),
			Port:    8317,
		},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/anthropic-auth-url?is_webui=true", nil)
	c.Request = req

	h.RequestAnthropicToken(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.State == "" {
		t.Fatalf("expected state in response")
	}
	authURL, err := url.Parse(payload.URL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	redirectURI := authURL.Query().Get("redirect_uri")
	if strings.TrimSpace(redirectURI) == "" {
		t.Fatalf("auth URL missing redirect_uri: %s", payload.URL)
	}
	callbackURL, err := url.Parse(redirectURI)
	if err != nil {
		t.Fatalf("parse redirect_uri: %v", err)
	}
	if callbackURL.Scheme != "http" || callbackURL.Hostname() != "localhost" || callbackURL.Path != "/callback" {
		t.Fatalf("redirect_uri = %q, want local callback URL", redirectURI)
	}
	if callbackURL.Port() == "54545" {
		t.Fatalf("redirect_uri should use a free callback port when 54545 is busy, got %q", redirectURI)
	}
	if callbackURL.Port() == "" {
		t.Fatalf("redirect_uri missing callback port: %q", redirectURI)
	}
	SetOAuthSessionError(payload.State, "test shutdown")
}

func TestRequestAntigravityTokenUsesAvailableLocalCallbackWhenPreferredPortBusy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	busy, err := net.Listen("tcp", "127.0.0.1:51121")
	if err != nil {
		t.Skipf("antigravity callback port already unavailable: %v", err)
	}
	defer func() { _ = busy.Close() }()

	useIsolatedOAuthSessions(t, oauthCallbackWaitTimeout)

	h := &Handler{
		cfg: &config.Config{
			AuthDir: t.TempDir(),
			Port:    8317,
		},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/antigravity-auth-url?is_webui=true", nil)
	c.Request = req

	h.RequestAntigravityToken(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.State == "" {
		t.Fatalf("expected state in response")
	}
	authURL, err := url.Parse(payload.URL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	redirectURI := authURL.Query().Get("redirect_uri")
	if strings.TrimSpace(redirectURI) == "" {
		t.Fatalf("auth URL missing redirect_uri: %s", payload.URL)
	}
	callbackURL, err := url.Parse(redirectURI)
	if err != nil {
		t.Fatalf("parse redirect_uri: %v", err)
	}
	if callbackURL.Scheme != "http" || callbackURL.Hostname() != "localhost" || callbackURL.Path != "/oauth-callback" {
		t.Fatalf("redirect_uri = %q, want local antigravity callback URL", redirectURI)
	}
	if callbackURL.Port() == "51121" {
		t.Fatalf("redirect_uri should use a free callback port when 51121 is busy, got %q", redirectURI)
	}
	if callbackURL.Port() == "" {
		t.Fatalf("redirect_uri missing callback port: %q", redirectURI)
	}

	SetOAuthSessionError(payload.State, "test shutdown")
}

func TestRequestCodexTokenDoesNotRequireLocalCallbackForwarder(t *testing.T) {
	gin.SetMode(gin.TestMode)

	busy, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		t.Skipf("codex callback port already unavailable: %v", err)
	}
	defer func() { _ = busy.Close() }()

	useIsolatedOAuthSessions(t, oauthCallbackWaitTimeout)

	h := &Handler{
		cfg: &config.Config{
			AuthDir: t.TempDir(),
			Port:    8317,
		},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/codex-auth-url?is_webui=true", nil)
	c.Request = req

	h.RequestCodexToken(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.State == "" {
		t.Fatalf("expected state in response")
	}
	authURL, err := url.Parse(payload.URL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if redirectURI := authURL.Query().Get("redirect_uri"); redirectURI != codex.RedirectURI {
		t.Fatalf("redirect_uri = %q, want %q", redirectURI, codex.RedirectURI)
	}
	SetOAuthSessionError(payload.State, "test shutdown")
}
