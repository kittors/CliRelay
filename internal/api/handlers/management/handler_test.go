package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 5; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
		req.RemoteAddr = "203.0.113.10:4321"
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("missing-key attempt %d status = %d, want %d; body=%s", i+1, rr.Code, http.StatusUnauthorized, rr.Body.String())
		}
	}

	rrBanned := httptest.NewRecorder()
	reqBanned := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	reqBanned.RemoteAddr = "203.0.113.10:4321"
	router.ServeHTTP(rrBanned, reqBanned)
	if rrBanned.Code != http.StatusForbidden {
		t.Fatalf("banned missing-key status = %d, want %d; body=%s", rrBanned.Code, http.StatusForbidden, rrBanned.Body.String())
	}
	if !strings.Contains(rrBanned.Body.String(), "IP banned") {
		t.Fatalf("expected IP banned response, got %s", rrBanned.Body.String())
	}

	rrValid := httptest.NewRecorder()
	reqValid := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	reqValid.RemoteAddr = "203.0.113.10:4321"
	reqValid.Header.Set("Authorization", "Bearer "+managementKey)
	router.ServeHTTP(rrValid, reqValid)
	if rrValid.Code != http.StatusOK {
		t.Fatalf("valid-key status after ban = %d, want %d; body=%s", rrValid.Code, http.StatusOK, rrValid.Body.String())
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

	var banned bool
	for i := 0; i < 12; i++ {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, newRelayedRequest())
		if rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), "IP banned") {
			banned = true
			break
		}
	}
	if !banned {
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
