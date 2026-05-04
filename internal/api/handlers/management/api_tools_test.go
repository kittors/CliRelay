package management

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type memoryAuthStore struct {
	mu    sync.Mutex
	items map[string]*coreauth.Auth
}

func (s *memoryAuthStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, a := range s.items {
		out = append(out, a.Clone())
	}
	return out, nil
}

func (s *memoryAuthStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	_ = ctx
	if auth == nil {
		return "", nil
	}
	s.mu.Lock()
	if s.items == nil {
		s.items = make(map[string]*coreauth.Auth)
	}
	s.items[auth.ID] = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (s *memoryAuthStore) Delete(ctx context.Context, id string) error {
	_ = ctx
	s.mu.Lock()
	delete(s.items, id)
	s.mu.Unlock()
	return nil
}

func TestResolveTokenForAuth_Antigravity_RefreshesExpiredToken(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Fatalf("unexpected content-type: %s", ct)
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		values, err := url.ParseQuery(string(bodyBytes))
		if err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if values.Get("grant_type") != "refresh_token" {
			t.Fatalf("unexpected grant_type: %s", values.Get("grant_type"))
		}
		if values.Get("refresh_token") != "rt" {
			t.Fatalf("unexpected refresh_token: %s", values.Get("refresh_token"))
		}
		if values.Get("client_id") != "test-antigravity-client-id" {
			t.Fatalf("unexpected client_id: %s", values.Get("client_id"))
		}
		if values.Get("client_secret") != "test-antigravity-client-secret" {
			t.Fatalf("unexpected client_secret")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-token",
			"refresh_token": "rt2",
			"expires_in":    int64(3600),
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(srv.Close)

	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = srv.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)

	auth := &coreauth.Auth{
		ID:       "antigravity-test.json",
		FileName: "antigravity-test.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "old-token",
			"refresh_token": "rt",
			"expires_in":    int64(3600),
			"timestamp":     time.Now().Add(-2 * time.Hour).UnixMilli(),
			"expired":       time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{
		cfg: &config.Config{
			OAuthClients: config.OAuthClients{
				Antigravity: config.OAuthClient{
					ClientID:     "test-antigravity-client-id",
					ClientSecret: "test-antigravity-client-secret",
				},
			},
		},
		authManager: manager,
	}
	token, err := h.resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "new-token" {
		t.Fatalf("expected refreshed token, got %q", token)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 refresh call, got %d", callCount)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth in manager after update")
	}
	if got := tokenValueFromMetadata(updated.Metadata); got != "new-token" {
		t.Fatalf("expected manager metadata updated, got %q", got)
	}
}

func TestResolveTokenForAuth_Antigravity_SkipsRefreshWhenTokenValid(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = srv.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	auth := &coreauth.Auth{
		ID:       "antigravity-valid.json",
		FileName: "antigravity-valid.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":         "antigravity",
			"access_token": "ok-token",
			"expired":      time.Now().Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	h := &Handler{}
	token, err := h.resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "ok-token" {
		t.Fatalf("expected existing token, got %q", token)
	}
	if callCount != 0 {
		t.Fatalf("expected no refresh calls, got %d", callCount)
	}
}

type fakeClaudeOAuthRefresher struct {
	tokenData *claudeauth.ClaudeTokenData
	err       error
	calls     int
	gotRT     string
}

func (f *fakeClaudeOAuthRefresher) RefreshTokens(ctx context.Context, refreshToken string) (*claudeauth.ClaudeTokenData, error) {
	_ = ctx
	f.calls++
	f.gotRT = refreshToken
	return f.tokenData, f.err
}

func TestResolveTokenForAuth_Claude_RefreshesExpiredToken(t *testing.T) {
	refresher := &fakeClaudeOAuthRefresher{
		tokenData: &claudeauth.ClaudeTokenData{
			AccessToken:  "new-claude-token",
			RefreshToken: "new-claude-refresh",
			Email:        "claude@example.com",
			Expire:       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	originalFactory := newClaudeOAuthRefresher
	newClaudeOAuthRefresher = func(cfg *config.Config) claudeOAuthRefresher {
		_ = cfg
		return refresher
	}
	t.Cleanup(func() { newClaudeOAuthRefresher = originalFactory })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)

	auth := &coreauth.Auth{
		ID:       "claude-test.json",
		FileName: "claude-test.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":          "claude",
			"access_token":  "old-claude-token",
			"refresh_token": "old-claude-refresh",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
			"email":         "old@example.com",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	token, err := h.resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "new-claude-token" {
		t.Fatalf("expected refreshed token, got %q", token)
	}
	if refresher.calls != 1 {
		t.Fatalf("expected 1 refresh call, got %d", refresher.calls)
	}
	if refresher.gotRT != "old-claude-refresh" {
		t.Fatalf("unexpected refresh token: %q", refresher.gotRT)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth in manager after update")
	}
	if got := tokenValueFromMetadata(updated.Metadata); got != "new-claude-token" {
		t.Fatalf("expected manager metadata updated, got %q", got)
	}
	if got, _ := updated.Metadata["refresh_token"].(string); got != "new-claude-refresh" {
		t.Fatalf("expected refresh_token updated, got %q", got)
	}
	if got, _ := updated.Metadata["email"].(string); got != "claude@example.com" {
		t.Fatalf("expected email updated, got %q", got)
	}
}

func TestResolveTokenForAuth_Claude_RefreshUsesAuthProxyURL(t *testing.T) {
	refresher := &fakeClaudeOAuthRefresher{
		tokenData: &claudeauth.ClaudeTokenData{
			AccessToken: "new-claude-token",
			Expire:      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	var gotProxyURL string
	originalFactory := newClaudeOAuthRefresher
	newClaudeOAuthRefresher = func(cfg *config.Config) claudeOAuthRefresher {
		if cfg != nil {
			gotProxyURL = cfg.ProxyURL
		}
		return refresher
	}
	t.Cleanup(func() { newClaudeOAuthRefresher = originalFactory })

	auth := &coreauth.Auth{
		ID:       "claude-proxy.json",
		FileName: "claude-proxy.json",
		Provider: "claude",
		ProxyURL: "http://auth-proxy.local:8080",
		Metadata: map[string]any{
			"type":          "claude",
			"access_token":  "old-claude-token",
			"refresh_token": "old-claude-refresh",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global-proxy.local:8080"}}}

	if _, err := h.resolveTokenForAuth(context.Background(), auth); err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if gotProxyURL != "http://auth-proxy.local:8080" {
		t.Fatalf("expected Claude refresh to use auth proxy URL, got %q", gotProxyURL)
	}
}

func TestResolveTokenForAuth_Claude_RefreshUsesProxyIDBeforeProxyURL(t *testing.T) {
	refresher := &fakeClaudeOAuthRefresher{
		tokenData: &claudeauth.ClaudeTokenData{
			AccessToken: "new-claude-token",
			Expire:      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	var gotProxyURL string
	originalFactory := newClaudeOAuthRefresher
	newClaudeOAuthRefresher = func(cfg *config.Config) claudeOAuthRefresher {
		if cfg != nil {
			gotProxyURL = cfg.ProxyURL
		}
		return refresher
	}
	t.Cleanup(func() { newClaudeOAuthRefresher = originalFactory })

	auth := &coreauth.Auth{
		ID:       "claude-proxy-id.json",
		FileName: "claude-proxy-id.json",
		Provider: "claude",
		ProxyID:  "premium-egress",
		ProxyURL: "http://legacy-proxy.local:8080",
		Metadata: map[string]any{
			"type":          "claude",
			"access_token":  "old-claude-token",
			"refresh_token": "old-claude-refresh",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	h := &Handler{cfg: &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://global-proxy.local:8080"},
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "premium-egress", URL: "http://pool-proxy.local:8080", Enabled: true},
		},
	}}

	if _, err := h.resolveTokenForAuth(context.Background(), auth); err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if gotProxyURL != "http://pool-proxy.local:8080" {
		t.Fatalf("expected Claude refresh to use proxy-id URL, got %q", gotProxyURL)
	}
}

func TestResolveTokenForAuth_Claude_SkipsRefreshWhenTokenValid(t *testing.T) {
	refresher := &fakeClaudeOAuthRefresher{
		tokenData: &claudeauth.ClaudeTokenData{AccessToken: "should-not-be-used"},
	}
	originalFactory := newClaudeOAuthRefresher
	newClaudeOAuthRefresher = func(cfg *config.Config) claudeOAuthRefresher {
		_ = cfg
		return refresher
	}
	t.Cleanup(func() { newClaudeOAuthRefresher = originalFactory })

	auth := &coreauth.Auth{
		ID:       "claude-valid.json",
		FileName: "claude-valid.json",
		Provider: "claude",
		Metadata: map[string]any{
			"type":         "claude",
			"access_token": "ok-claude-token",
			"expired":      time.Now().Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	h := &Handler{cfg: &config.Config{}}
	token, err := h.resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "ok-claude-token" {
		t.Fatalf("expected existing token, got %q", token)
	}
	if refresher.calls != 0 {
		t.Fatalf("expected no refresh calls, got %d", refresher.calls)
	}
}

func TestAPICallRejectsOversizedUpstreamResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(bytes.Repeat([]byte("a"), int(managementAPICallResponseLimit)+1))
	}))
	t.Cleanup(upstream.Close)

	h := &Handler{cfg: &config.Config{}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"method":"GET","url":"` + upstream.URL + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.APICall(c)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusBadGateway, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream response too large") {
		t.Fatalf("expected upstream size error, got body=%s", rec.Body.String())
	}
}
