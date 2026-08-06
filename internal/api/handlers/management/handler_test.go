package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestHandlerCloseIsIdempotent(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(nil, nil)
	h.Close()
	h.Close()
}

func TestMiddlewareAllowsValidKeyAfterRemoteIPIsBanned(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const managementKey = "correct-management-key"
	hashed, err := bcrypt.GenerateFromPassword([]byte(managementKey), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash test management key: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote: true,
			SecretKey:   string(hashed),
		},
	}, nil)
	defer h.Close()

	// Hold the ban open for the whole test rather than sleeping it out. Arming it
	// costs ten bcrypt comparisons, and under -race those run an order of
	// magnitude slower, so any block short enough to keep the test fast also
	// expires mid-setup and silently turns the X1 assertion into a no-op. The
	// expiry half is driven by expireArmedBlocks instead of wall-clock time.
	policies := defaultThrottlePolicies()
	held := policies[scopeManagementKey]
	held.Backoff = []time.Duration{time.Hour}
	policies[scopeManagementKey] = held
	h.loginThrottle.setPolicies(policies)

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// Missing key attempts land in scopeUnauthenticated, which never hard-blocks
	// (B2), so arming the scopeManagementKey ban requires actual wrong-credential
	// attempts, not empty ones. defaultManagementKeyFailureLimit is 10, and the
	// Nth failure itself trips the block, so only 9 attempts come back 401.
	for i := 0; i < 9; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
		req.RemoteAddr = "203.0.113.10:4321"
		req.Header.Set("Authorization", "Bearer wrong-key")
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("wrong-key attempt %d status = %d, want %d; body=%s", i+1, rr.Code, http.StatusUnauthorized, rr.Body.String())
		}
	}

	rrBanned := httptest.NewRecorder()
	reqBanned := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	reqBanned.RemoteAddr = "203.0.113.10:4321"
	reqBanned.Header.Set("Authorization", "Bearer wrong-key")
	router.ServeHTTP(rrBanned, reqBanned)
	if rrBanned.Code != http.StatusForbidden {
		t.Fatalf("banned wrong-key status = %d, want %d; body=%s", rrBanned.Code, http.StatusForbidden, rrBanned.Body.String())
	}
	if !strings.Contains(rrBanned.Body.String(), "IP banned") {
		t.Fatalf("expected IP banned response, got %s", rrBanned.Body.String())
	}

	// X1: the ban precheck runs before any credential comparison, so even the
	// correct key is rejected while the block is still active.
	rrDuringBan := httptest.NewRecorder()
	reqDuringBan := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	reqDuringBan.RemoteAddr = "203.0.113.10:4321"
	reqDuringBan.Header.Set("Authorization", "Bearer "+managementKey)
	router.ServeHTTP(rrDuringBan, reqDuringBan)
	if rrDuringBan.Code != http.StatusForbidden {
		t.Fatalf("valid-key status during active ban = %d, want %d (ban precheck must precede credential comparison); body=%s", rrDuringBan.Code, http.StatusForbidden, rrDuringBan.Body.String())
	}

	expireArmedBlocks(h.loginThrottle)

	rrValid := httptest.NewRecorder()
	reqValid := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	reqValid.RemoteAddr = "203.0.113.10:4321"
	reqValid.Header.Set("Authorization", "Bearer "+managementKey)
	router.ServeHTTP(rrValid, reqValid)
	if rrValid.Code != http.StatusOK {
		t.Fatalf("valid-key status after ban expired = %d, want %d; body=%s", rrValid.Code, http.StatusOK, rrValid.Body.String())
	}

	rrAfterClear := httptest.NewRecorder()
	reqAfterClear := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	reqAfterClear.RemoteAddr = "203.0.113.10:4321"
	router.ServeHTTP(rrAfterClear, reqAfterClear)
	if rrAfterClear.Code != http.StatusUnauthorized {
		t.Fatalf("missing-key status after valid key cleared ban = %d, want %d; body=%s", rrAfterClear.Code, http.StatusUnauthorized, rrAfterClear.Body.String())
	}
}

func TestMiddlewareAllowsLocalPasswordWithoutRemoteSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetLocalPassword("local-management-password")
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header.Set("Authorization", "Bearer local-management-password")
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("local-password status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestMiddlewareRejectsQueryTokenOnNormalHTTPRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const managementKey = "correct-management-key"
	hashed, err := bcrypt.GenerateFromPassword([]byte(managementKey), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash test management key: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote: true,
			SecretKey:   string(hashed),
		},
	}, nil)
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/ping?token="+managementKey, nil)
	req.RemoteAddr = "203.0.113.20:4321"
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "missing management key") {
		t.Fatalf("expected query token to be ignored on normal route, got body=%s", rr.Body.String())
	}
}

func TestMiddlewareAllowsQueryTokenOnlyForSystemStatsWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const managementKey = "correct-management-key"
	hashed, err := bcrypt.GenerateFromPassword([]byte(managementKey), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash test management key: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote: true,
			SecretKey:   string(hashed),
		},
	}, nil)
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/system-stats/ws", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/system-stats/ws?token="+managementKey, nil)
	req.RemoteAddr = "203.0.113.21:4321"
	req.Header.Set("Upgrade", "websocket")
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestMiddlewareRoutesQuerySessionTokenForSystemStatsWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const managementKey = "correct-management-key"
	hashed, err := bcrypt.GenerateFromPassword([]byte(managementKey), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash test management key: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote: true,
			SecretKey:   string(hashed),
		},
	}, nil)
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/system-stats/ws", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// cps_* query tokens must enter the identity-session path, not bcrypt management-key
	// comparison. Without an identity service this surfaces as identity_unavailable (503),
	// not invalid management key (401).
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/system-stats/ws?token=cps_test-session-token", nil)
	req.RemoteAddr = "203.0.113.22:4321"
	req.Header.Set("Upgrade", "websocket")
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusServiceUnavailable, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "identity_unavailable") {
		t.Fatalf("expected session-token routing into identity auth, got body=%s", rr.Body.String())
	}
}

func TestMiddlewareIgnoresQuerySessionTokenOnNormalHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const managementKey = "correct-management-key"
	hashed, err := bcrypt.GenerateFromPassword([]byte(managementKey), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash test management key: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote: true,
			SecretKey:   string(hashed),
		},
	}, nil)
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/system-stats", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/system-stats?token=cps_test-session-token", nil)
	req.RemoteAddr = "203.0.113.23:4321"
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "missing management key") {
		t.Fatalf("expected query session token ignored on normal HTTP, got body=%s", rr.Body.String())
	}
}

// newDefaultTrustProxyRouter mirrors the production engine for a deployment that left
// trusted-proxies empty (the shipped default): configureTrustedProxies calls
// SetTrustedProxies(nil), which makes gin's ClientIP fall back to RemoteAddr. That
// fallback is what turned a same-host reverse proxy into a local-origin bypass.
func newDefaultTrustProxyRouter(t *testing.T, h *Handler) *gin.Engine {
	t.Helper()
	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies(nil): %v", err)
	}
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return router
}

// Same root cause as issue #517, on the management surface: with trusted-proxies unset
// gin's ClientIP falls back to RemoteAddr, so a same-host reverse proxy made every
// external request look local — which skipped the allow-remote gate entirely.
func TestMiddlewareRejectsRelayedRequestWhenRemoteDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const managementKey = "correct-management-key"
	hashed, err := bcrypt.GenerateFromPassword([]byte(managementKey), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash test management key: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote: false,
			SecretKey:   string(hashed),
		},
	}, nil)
	defer h.Close()

	router := newDefaultTrustProxyRouter(t, h)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	// The proxy is the TCP peer; the real client is external.
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header.Set("X-Forwarded-For", "203.0.113.42")
	req.Header.Set("Authorization", "Bearer "+managementKey)
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("relayed request status = %d, want %d; body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "remote management disabled") {
		t.Fatalf("expected remote-management rejection, got body=%s", rr.Body.String())
	}
	// The hint must name a way out, otherwise operators just see a broken panel.
	if !strings.Contains(rr.Body.String(), "trusted-proxies") {
		t.Fatalf("expected remediation hint naming trusted-proxies, got body=%s", rr.Body.String())
	}
}

// The local-password path is reachable without any remote secret, so it must not be
// exposed to relayed requests.
func TestMiddlewareRejectsRelayedRequestUsingLocalPassword(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetLocalPassword("local-management-password")
	defer h.Close()

	router := newDefaultTrustProxyRouter(t, h)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header.Set("X-Forwarded-For", "203.0.113.42")
	req.Header.Set("Authorization", "Bearer local-management-password")
	router.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("relayed request authenticated with the local password; body=%s", rr.Body.String())
	}
}

// Relayed requests must also be subject to the per-IP login throttle that the
// localClient shortcut used to skip.
func TestMiddlewareThrottlesRelayedFailedAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	hashed, err := bcrypt.GenerateFromPassword([]byte("correct-management-key"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash test management key: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote: true,
			SecretKey:   string(hashed),
		},
	}, nil)
	defer h.Close()

	router := newDefaultTrustProxyRouter(t, h)

	newRelayedRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
		req.RemoteAddr = "127.0.0.1:4321"
		req.Header.Set("X-Forwarded-For", "203.0.113.42")
		req.Header.Set("Authorization", "Bearer wrong-key")
		return req
	}

	// The relayed key is Shared (untrusted proxy), so applySharedKeyDowngrade forces
	// HardBlock=false: it must be rate-limited (429), never hard-banned (403).
	var throttled bool
	for i := 0; i < 15; i++ {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, newRelayedRequest())
		if rr.Code == http.StatusForbidden {
			t.Fatalf("relayed (shared-key) attempt %d hard-banned with 403; want soft 429 only, body=%s", i+1, rr.Body.String())
		}
		if rr.Code == http.StatusTooManyRequests && strings.Contains(rr.Body.String(), "too_many_requests") {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("relayed brute-force attempts were never throttled")
	}
}

// A genuine local client (no relay headers) must keep working unchanged.
func TestMiddlewareAllowsDirectLoopbackLocalPassword(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetLocalPassword("local-management-password")
	defer h.Close()

	router := newDefaultTrustProxyRouter(t, h)

	for _, remoteAddr := range []string{"127.0.0.1:4321", "[::1]:4321"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("Authorization", "Bearer local-management-password")
		router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("direct loopback %s status = %d, want %d; body=%s", remoteAddr, rr.Code, http.StatusOK, rr.Body.String())
		}
	}
}

func TestResolveSessionToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("bearer session token", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/system-stats", nil)
		c.Request.Header.Set("Authorization", "Bearer cps_from_header")
		if got := resolveSessionToken(c); got != "cps_from_header" {
			t.Fatalf("resolveSessionToken = %q, want cps_from_header", got)
		}
	})

	t.Run("websocket query session token", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/system-stats/ws?token=cps_from_query", nil)
		c.Request.Header.Set("Upgrade", "websocket")
		c.Params = nil
		// FullPath is empty in unit tests; shouldReadManagementTokenFromQuery falls back to URL.Path.
		if got := resolveSessionToken(c); got != "cps_from_query" {
			t.Fatalf("resolveSessionToken = %q, want cps_from_query", got)
		}
	})

	t.Run("non-session query token ignored for session resolution", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/system-stats/ws?token=mgmt-key", nil)
		c.Request.Header.Set("Upgrade", "websocket")
		if got := resolveSessionToken(c); got != "" {
			t.Fatalf("resolveSessionToken = %q, want empty", got)
		}
	})
}
