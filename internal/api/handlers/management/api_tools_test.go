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
	"sync/atomic"
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
	token, err := h.APITools().resolveTokenForAuth(context.Background(), auth)
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
	token, err := h.APITools().resolveTokenForAuth(context.Background(), auth)
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
	token, err := h.APITools().resolveTokenForAuth(context.Background(), auth)
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

	if _, err := h.APITools().resolveTokenForAuth(context.Background(), auth); err != nil {
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

	if _, err := h.APITools().resolveTokenForAuth(context.Background(), auth); err != nil {
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
	token, err := h.APITools().resolveTokenForAuth(context.Background(), auth)
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

func TestResolveTokenForAuth_XAI_RefreshesExpiredToken(t *testing.T) {
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
		if values.Get("refresh_token") != "xai-refresh" {
			t.Fatalf("unexpected refresh_token: %s", values.Get("refresh_token"))
		}
		if values.Get("client_id") == "" {
			t.Fatal("expected client_id")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-xai-token",
			"refresh_token": "xai-refresh-2",
			"expires_in":    int64(3600),
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(srv.Close)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "xai-test.json",
		FileName: "xai-test.json",
		Provider: "xai",
		Metadata: map[string]any{
			"type":           "xai",
			"auth_kind":      "oauth",
			"access_token":   "old-xai-token",
			"refresh_token":  "xai-refresh",
			"token_endpoint": srv.URL,
			"expired":        time.Now().Add(-time.Hour).Format(time.RFC3339),
			"email":          "old@example.com",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	token, err := h.APITools().resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "new-xai-token" {
		t.Fatalf("expected refreshed token, got %q", token)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 refresh call, got %d", callCount)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth in manager after update")
	}
	if got := tokenValueFromMetadata(updated.Metadata); got != "new-xai-token" {
		t.Fatalf("expected manager metadata updated, got %q", got)
	}
	if updated.Metadata["auth_kind"] != "oauth" {
		t.Fatalf("auth_kind = %#v", updated.Metadata["auth_kind"])
	}
}

func TestResolveTokenForAuth_XAI_SkipsRefreshWhenTokenValid(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	auth := &coreauth.Auth{
		ID:       "xai-valid.json",
		FileName: "xai-valid.json",
		Provider: "xai",
		Metadata: map[string]any{
			"type":           "xai",
			"auth_kind":      "oauth",
			"access_token":   "ok-xai-token",
			"refresh_token":  "xai-refresh",
			"token_endpoint": srv.URL,
			"expired":        time.Now().Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	h := &Handler{cfg: &config.Config{}}
	token, err := h.APITools().resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "ok-xai-token" {
		t.Fatalf("expected existing token, got %q", token)
	}
	if callCount != 0 {
		t.Fatalf("expected no refresh calls, got %d", callCount)
	}
}

func TestResolveTokenForAuth_XAI_APIKeyDoesNotRefresh(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	auth := &coreauth.Auth{
		ID:       "xai-key.json",
		FileName: "xai-key.json",
		Provider: "xai",
		Attributes: map[string]string{
			"api_key": "xai-api-key-value",
		},
		Metadata: map[string]any{
			"type":      "xai",
			"auth_kind": "api_key",
			"api_key":   "xai-api-key-value",
		},
	}
	h := &Handler{cfg: &config.Config{}}
	token, err := h.APITools().resolveTokenForAuth(context.Background(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "xai-api-key-value" {
		t.Fatalf("expected api key, got %q", token)
	}
	if callCount != 0 {
		t.Fatalf("expected no refresh calls, got %d", callCount)
	}
}

func TestAPICallReconcilesCodexWhamUsagePlanType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "93.184.216.34" {
			t.Fatalf("unexpected proxy host: %s", r.Host)
		}
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email":     "aleckmason123@gmail.com",
			"plan_type": "free",
		})
	}))
	t.Cleanup(proxy.Close)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-plan.json",
		FileName: "codex-plan.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "access-token",
			"email":        "aleckmason123@gmail.com",
			"plan_type":    "plus",
			"display_tags": []string{"codex", "plus"},
		},
	}
	registered, err := manager.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registered.EnsureIndex()

	requestBody, err := json.Marshal(map[string]any{
		"authIndex": registered.Index,
		"method":    "GET",
		"url":       "http://93.184.216.34/backend-api/wham/usage",
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
		},
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxy.URL}}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(requestBody))
	c.Request.Header.Set("Content-Type", "application/json")

	h.APITools().APICall(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if got, _ := updated.Metadata["plan_type"].(string); got != "free" {
		t.Fatalf("plan_type = %q, want free", got)
	}
	displayTags, ok := updated.Metadata["display_tags"].([]string)
	if !ok {
		t.Fatalf("display_tags type = %T, want []string", updated.Metadata["display_tags"])
	}
	if len(displayTags) != 2 || displayTags[0] != "codex" || displayTags[1] != "free" {
		t.Fatalf("display_tags = %#v, want [codex free]", displayTags)
	}
	saved := store.items[auth.ID]
	if saved == nil {
		t.Fatal("expected auth store to persist reconciled auth")
	}
	if got, _ := saved.Metadata["plan_type"].(string); got != "free" {
		t.Fatalf("saved plan_type = %q, want free", got)
	}
}

func TestAPICallRejectsLoopbackTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{cfg: &config.Config{}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"method":"GET","url":"http://127.0.0.1:1/metadata"}`)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.APITools().APICall(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != "blocked_private_address" || !strings.Contains(env.Error.Message, "127.0.0.1") {
		t.Fatalf("expected blocked_private_address error mentioning 127.0.0.1, got body=%s", rec.Body.String())
	}
}

// postAPICallWithTokenPlaceholder sends a GET with "Authorization: Bearer
// $TOKEN$" through the api-call tool. Egress is routed through a stand-in proxy
// that counts every request reaching it, so a regression fails here instead of
// dialing the public address in the URL.
func postAPICallWithTokenPlaceholder(t *testing.T, authIndex string, manager *coreauth.Manager) (*httptest.ResponseRecorder, int32) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var upstreamHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		t.Errorf("upstream reached with Authorization=%q", r.Header.Get("Authorization"))
		http.Error(w, "upstream must not be reached", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxy.URL}}, authManager: manager}

	payload := map[string]any{
		"method": "GET",
		"url":    "http://93.184.216.34/backend-api/wham/usage",
		"header": map[string]string{"Authorization": "Bearer $TOKEN$"},
	}
	if authIndex != "" {
		payload["authIndex"] = authIndex
	}
	requestBody, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(requestBody))
	c.Request.Header.Set("Content-Type", "application/json")

	h.APITools().APICall(c)
	return rec, upstreamHits.Load()
}

// TestAPICallRejectsUnknownAuthIndexTokenPlaceholder covers a stale or
// cross-tenant auth_index, which resolves to no credential. The tool used to
// forward the header with the literal "$TOKEN$" still in it, and the upstream's
// 401 then read as an expired credential.
func TestAPICallRejectsUnknownAuthIndexTokenPlaceholder(t *testing.T) {
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	rec, hits := postAPICallWithTokenPlaceholder(t, "stale-or-cross-tenant-index", manager)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d %s, want 400", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != "auth_not_found" || !strings.Contains(env.Error.Message, "stale-or-cross-tenant-index") {
		t.Fatalf("got %d %s, want auth_not_found mentioning index", rec.Code, rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("upstream was reached %d time(s)", hits)
	}
}

func TestAPICallRejectsMissingAuthIndexTokenPlaceholder(t *testing.T) {
	rec, hits := postAPICallWithTokenPlaceholder(t, "", nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d %s, want 400", rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != "missing_auth_index" || !strings.Contains(env.Error.Message, "auth_index") {
		t.Fatalf("got %d %s, want missing_auth_index", rec.Code, rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("upstream was reached %d time(s)", hits)
	}
}

func TestAPICallRejectsRedirectToLoopbackTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/metadata", http.StatusFound)
	}))
	t.Cleanup(proxy.Close)

	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxy.URL}}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"method":"GET","url":"http://93.184.216.34/redirect"}`)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.APITools().APICall(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != "blocked_redirect_target" || !strings.Contains(env.Error.Message, "重定向") {
		t.Fatalf("expected blocked_redirect_target error, got body=%s", rec.Body.String())
	}
}

func TestAPICallRejectsOversizedUpstreamResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(bytes.Repeat([]byte("a"), int(managementAPICallResponseLimit)+1))
	}))
	t.Cleanup(proxy.Close)

	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxy.URL}}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"method":"GET","url":"http://93.184.216.34/large"}`)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.APITools().APICall(c)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusBadGateway, rec.Code, rec.Body.String())
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env.Error.Code != "upstream_response_too_large" || !strings.Contains(env.Error.Message, "4 MiB") {
		t.Fatalf("expected upstream_response_too_large mentioning 4 MiB, got body=%s", rec.Body.String())
	}
}

func TestAPICallAllowsDomainThroughExplicitProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenHost string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(proxy.Close)

	h := &Handler{cfg: &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxy.URL}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	// Use a synthetic .invalid domain that cannot resolve on the host's local DNS;
	// with an explicit proxy configured, DNS resolution belongs to the proxy.
	body := []byte(`{"method":"GET","url":"http://chatgpt.proxy-only.invalid/backend-api/wham/rate-limit-reset-credits"}`)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.APITools().APICall(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via explicit proxy, got %d body=%s", rec.Code, rec.Body.String())
	}
	if seenHost != "chatgpt.proxy-only.invalid" {
		t.Fatalf("proxy saw host %q, want chatgpt.proxy-only.invalid", seenHost)
	}
}

func TestAPICallRejectsFakeIPLiteralAndBadSchemeClearly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{}}

	cases := []struct {
		name     string
		body     string
		wantCode string
		wantHint string
	}{
		{
			name:     "fake ip literal",
			body:     `{"method":"GET","url":"http://198.18.0.42/test"}`,
			wantCode: "blocked_fake_ip_address",
			wantHint: "Fake-IP",
		},
		{
			name:     "unsupported scheme",
			body:     `{"method":"GET","url":"file:///etc/passwd"}`,
			wantCode: "unsupported_url_scheme",
			wantHint: "file",
		},
		{
			name:     "missing host",
			body:     `{"method":"GET","url":"https:///path-only"}`,
			wantCode: "missing_url_host",
			wantHint: "主机名",
		},
		{
			name:     "invalid json body",
			body:     `{"method":`,
			wantCode: "empty_request_body",
			wantHint: "请求体",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req

			h.APITools().APICall(c)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			env := decodeErrorEnvelope(t, rec.Body.Bytes())
			if env.Error.Code != tc.wantCode || !strings.Contains(env.Error.Message, tc.wantHint) {
				t.Fatalf("got code=%q message=%q, want code=%q containing %q", env.Error.Code, env.Error.Message, tc.wantCode, tc.wantHint)
			}
		})
	}
}
