package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func setupResetCreditTestHandler(t *testing.T, cfg *config.Config, auths ...*coreauth.Auth) (*Handler, *coreauth.Manager) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	for _, a := range auths {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatalf("register auth %q: %v", a.ID, err)
		}
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	h := NewHandler(cfg, "", manager)
	t.Cleanup(h.Close)
	return h, manager
}

func postResetCredit(h *Handler, tenantID, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if tenantID != "" {
		c.Set(managementPrincipalKey, identity.Principal{
			EffectiveTenant: identity.Tenant{ID: tenantID},
		})
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/reset-credit", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	h.PostAuthFileResetCredit(c)
	return rec
}

func withCodexResetCreditURLs(t *testing.T, listURL, consumeURL string) {
	t.Helper()
	prevList := codexResetCreditsListURL
	prevConsume := codexResetCreditsConsumeURL
	codexResetCreditsListURL = listURL
	codexResetCreditsConsumeURL = consumeURL
	t.Cleanup(func() {
		codexResetCreditsListURL = prevList
		codexResetCreditsConsumeURL = prevConsume
	})
}

func newCodexTestAuth(id, tenantID, token, accountID string) *coreauth.Auth {
	meta := map[string]any{
		"type": "codex",
	}
	if token != "" {
		meta["access_token"] = token
	}
	if accountID != "" {
		meta["account_id"] = accountID
	}
	auth := &coreauth.Auth{
		ID:       id,
		FileName: id,
		TenantID: tenantID,
		Provider: "codex",
		Metadata: meta,
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "rate_limit",
			NextRecoverAt: time.Now().Add(time.Hour),
			BackoffLevel:  2,
		},
	}
	auth.EnsureIndex()
	return auth
}

func TestPostAuthFileResetCreditFIFOConsumeAndClearQuota(t *testing.T) {
	var listHits atomic.Int32
	var consumeHits atomic.Int32
	var seenConsumePayload map[string]string
	var seenUA, seenAccountID, seenAuthHeader string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/credits":
			listHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"credits": [
					{"id": "credit-later", "status": "available", "reset_type": "codex_rate_limits", "expires_at": "2026-12-20T00:00:00Z"},
					{"id": "credit-other-type", "status": "available", "reset_type": "other_reset_type", "expires_at": "2026-11-30T00:00:00Z"},
					{"id": "credit-used", "status": "consumed", "reset_type": "codex_rate_limits", "expires_at": "2026-12-01T00:00:00Z"},
					{"id": "credit-earliest", "status": "available", "reset_type": "codex_rate_limits", "expires_at": "2026-12-05T00:00:00Z"}
				]
			}`))
		case r.Method == http.MethodPost && r.URL.Path == "/credits/consume":
			consumeHits.Add(1)
			seenUA = r.Header.Get("User-Agent")
			seenAccountID = r.Header.Get("Chatgpt-Account-Id")
			seenAuthHeader = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&seenConsumePayload)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"redeemed": true, "remaining_credits": 1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	withCodexResetCreditURLs(t, upstream.URL+"/credits", upstream.URL+"/credits/consume")

	auth := newCodexTestAuth("codex-a.json", identity.SystemTenantID, "tok-codex-123", "acct-xyz-789")
	h, manager := setupResetCreditTestHandler(t, nil, auth)

	rec := postResetCredit(h, identity.SystemTenantID, `{"auth_index":"`+auth.EnsureIndex()+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if listHits.Load() != 1 || consumeHits.Load() != 1 {
		t.Fatalf("hits list=%d consume=%d, want 1 and 1", listHits.Load(), consumeHits.Load())
	}
	if seenConsumePayload["credit_id"] != "credit-earliest" {
		t.Fatalf("consumed credit_id = %q, want credit-earliest (FIFO)", seenConsumePayload["credit_id"])
	}
	if strings.TrimSpace(seenConsumePayload["redeem_request_id"]) == "" {
		t.Fatal("expected generated redeem_request_id")
	}
	if seenUA != config.DefaultCodexFingerprintUserAgent {
		t.Fatalf("User-Agent = %q, want %q", seenUA, config.DefaultCodexFingerprintUserAgent)
	}
	if seenAccountID != "acct-xyz-789" {
		t.Fatalf("Chatgpt-Account-Id = %q, want acct-xyz-789", seenAccountID)
	}
	if seenAuthHeader != "Bearer tok-codex-123" {
		t.Fatalf("Authorization = %q, want Bearer tok-codex-123", seenAuthHeader)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "ok" || resp["credit_id"] != "credit-earliest" || resp["quota_status_cleared"] != true {
		t.Fatalf("unexpected response payload: %#v", resp)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing from manager")
	}
	if updated.Quota.Exceeded {
		t.Fatalf("expected local Quota.Exceeded to be cleared, got %+v", updated.Quota)
	}
}

func TestPostAuthFileResetCreditExplicitCreditIDSkipsLookupAndSupportsNameLookup(t *testing.T) {
	var listHits atomic.Int32
	var seenPayload map[string]string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/credits" {
			listHits.Add(1)
			http.Error(w, "should not query list when credit_id is explicit", http.StatusInternalServerError)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&seenPayload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	withCodexResetCreditURLs(t, upstream.URL+"/credits", upstream.URL+"/credits/consume")

	auth := newCodexTestAuth("codex-named.json", identity.SystemTenantID, "tok-named", "")
	h, _ := setupResetCreditTestHandler(t, nil, auth)

	rec := postResetCredit(h, identity.SystemTenantID, `{"name":"codex-named.json","credit_id":"credit-explicit","redeem_request_id":"req-fixed-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if listHits.Load() != 0 {
		t.Fatalf("expected 0 list requests when credit_id is provided, got %d", listHits.Load())
	}
	if seenPayload["credit_id"] != "credit-explicit" || seenPayload["redeem_request_id"] != "req-fixed-1" {
		t.Fatalf("unexpected consume payload: %#v", seenPayload)
	}
}

func TestPostAuthFileResetCreditLookupFailureFallsBackToUntargetedConsume(t *testing.T) {
	var seenPayload map[string]string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/credits" {
			http.Error(w, "temporary list outage", http.StatusInternalServerError)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&seenPayload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	withCodexResetCreditURLs(t, upstream.URL+"/credits", upstream.URL+"/credits/consume")

	auth := newCodexTestAuth("codex-fallback.json", identity.SystemTenantID, "tok-fb", "")
	h, _ := setupResetCreditTestHandler(t, nil, auth)

	rec := postResetCredit(h, identity.SystemTenantID, `{"id":"codex-fallback.json"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, hasCreditID := seenPayload["credit_id"]; hasCreditID {
		t.Fatalf("expected untargeted consume without credit_id, got %#v", seenPayload)
	}
}

func TestPostAuthFileResetCreditUpstreamStatusClassification(t *testing.T) {
	cases := []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
		wantStatus     int
		wantCode       string
		wantSubstring  string
	}{
		{
			name:           "no credits available",
			upstreamStatus: http.StatusBadRequest,
			upstreamBody:   `{"detail":"No available rate limit reset credits for this account"}`,
			wantStatus:     http.StatusConflict,
			wantCode:       "no_reset_credits_available",
			wantSubstring:  "没有可用的额度重置次数",
		},
		{
			// Must return 502, never 401: admin panel auth-fetch logs the operator out on 401.
			name:           "upstream 401 mapped to 502",
			upstreamStatus: http.StatusUnauthorized,
			upstreamBody:   `{"error":{"message":"token expired"}}`,
			wantStatus:     http.StatusBadGateway,
			wantCode:       "upstream_auth_expired",
			wantSubstring:  "上游登录态已失效",
		},
		{
			name:           "upstream 403 forbidden",
			upstreamStatus: http.StatusForbidden,
			upstreamBody:   `{"detail":"workspace restricted"}`,
			wantStatus:     http.StatusBadGateway,
			wantCode:       "upstream_forbidden",
			wantSubstring:  "拒绝了本次额度重置请求",
		},
		{
			name:           "upstream 429 rate limited",
			upstreamStatus: http.StatusTooManyRequests,
			upstreamBody:   `{"detail":"too many requests"}`,
			wantStatus:     http.StatusTooManyRequests,
			wantCode:       "upstream_rate_limited",
			wantSubstring:  "频率限制",
		},
		{
			name:           "upstream 500 server error",
			upstreamStatus: http.StatusInternalServerError,
			upstreamBody:   `{"detail":"internal error"}`,
			wantStatus:     http.StatusBadGateway,
			wantCode:       "upstream_server_error",
			wantSubstring:  "HTTP 500",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/credits" {
					_, _ = w.Write([]byte(`{"credits":[]}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstreamStatus)
				_, _ = w.Write([]byte(tc.upstreamBody))
			}))
			t.Cleanup(upstream.Close)
			withCodexResetCreditURLs(t, upstream.URL+"/credits", upstream.URL+"/credits/consume")

			auth := newCodexTestAuth("codex-err.json", identity.SystemTenantID, "tok-err", "")
			h, _ := setupResetCreditTestHandler(t, nil, auth)

			rec := postResetCredit(h, identity.SystemTenantID, `{"auth_index":"`+auth.EnsureIndex()+`"}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			env := decodeErrorEnvelope(t, rec.Body.Bytes())
			if env.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q; body=%s", env.Error.Code, tc.wantCode, rec.Body.String())
			}
			if !strings.Contains(env.Error.Message, tc.wantSubstring) {
				t.Fatalf("message = %q, want substring %q", env.Error.Message, tc.wantSubstring)
			}
		})
	}
}

func TestPostAuthFileResetCreditValidationAndTenantIsolation(t *testing.T) {
	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	withCodexResetCreditURLs(t, upstream.URL+"/credits", upstream.URL+"/credits/consume")

	codexAuth := newCodexTestAuth("codex-a.json", tenantA, "tok-a", "")
	noTokenAuth := newCodexTestAuth("codex-notoken.json", tenantA, "", "")
	claudeAuth := &coreauth.Auth{
		ID:       "claude-a.json",
		FileName: "claude-a.json",
		TenantID: tenantA,
		Provider: "claude",
		Metadata: map[string]any{"type": "claude", "access_token": "tok-claude"},
	}
	claudeAuth.EnsureIndex()

	h, manager := setupResetCreditTestHandler(t, nil, codexAuth, noTokenAuth, claudeAuth)

	// 1. Cross-tenant access must return 404 without touching upstream or tenant A's quota.
	rec := postResetCredit(h, tenantB, `{"auth_index":"`+codexAuth.EnsureIndex()+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if env := decodeErrorEnvelope(t, rec.Body.Bytes()); env.Error.Code != "auth_not_found" {
		t.Fatalf("cross-tenant code = %q, want auth_not_found", env.Error.Code)
	}
	if got, _ := manager.GetByID(codexAuth.ID); !got.Quota.Exceeded {
		t.Fatal("cross-tenant request cleared tenant A quota")
	}

	// 2. Non-codex credential rejected.
	rec = postResetCredit(h, tenantA, `{"auth_index":"`+claudeAuth.EnsureIndex()+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-codex status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if env := decodeErrorEnvelope(t, rec.Body.Bytes()); env.Error.Code != "unsupported_provider" {
		t.Fatalf("non-codex code = %q, want unsupported_provider", env.Error.Code)
	}

	// 3. Missing token rejected cleanly.
	rec = postResetCredit(h, tenantA, `{"auth_index":"`+noTokenAuth.EnsureIndex()+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-token status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if env := decodeErrorEnvelope(t, rec.Body.Bytes()); env.Error.Code != "auth_token_not_found" {
		t.Fatalf("no-token code = %q, want auth_token_not_found", env.Error.Code)
	}

	// 4. Missing identifier rejected with missing_required_field.
	rec = postResetCredit(h, tenantA, `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty payload status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if env := decodeErrorEnvelope(t, rec.Body.Bytes()); env.Error.Code != "missing_required_field" {
		t.Fatalf("empty payload code = %q, want missing_required_field", env.Error.Code)
	}

	// 5. No authManager returns 503 service_unavailable before parsing body.
	nilMgrHandler := &Handler{cfg: &config.Config{}}
	rec = postResetCredit(nilMgrHandler, tenantA, `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil authManager status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if env := decodeErrorEnvelope(t, rec.Body.Bytes()); env.Error.Code != "service_unavailable" {
		t.Fatalf("nil authManager code = %q, want service_unavailable", env.Error.Code)
	}

	if upstreamHits.Load() != 0 {
		t.Fatalf("expected 0 upstream hits across rejected cases, got %d", upstreamHits.Load())
	}
}

func TestPostAuthFileResetCreditRoutesViaConfiguredProxy(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"credits":[{"id":"cr-proxy-1","status":"available","reset_type":"codex_rate_limits"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"redeemed":true}`))
	}))
	t.Cleanup(proxy.Close)

	// Target a synthetic HTTP domain that can only succeed through the explicit proxy.
	withCodexResetCreditURLs(
		t,
		"http://chatgpt.proxy-only.invalid/backend-api/wham/rate-limit-reset-credits",
		"http://chatgpt.proxy-only.invalid/backend-api/wham/rate-limit-reset-credits/consume",
	)

	auth := newCodexTestAuth("codex-proxy.json", identity.SystemTenantID, "tok-proxy", "acct-proxy")
	auth.ProxyURL = proxy.URL
	h, _ := setupResetCreditTestHandler(t, nil, auth)

	rec := postResetCredit(h, identity.SystemTenantID, `{"auth_index":"`+auth.EnsureIndex()+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if proxyHits.Load() != 2 {
		t.Fatalf("expected 2 requests through proxy (list + consume), got %d", proxyHits.Load())
	}
}
