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
			AllowRemote:     true,
			SecretKey:       string(hashed),
			MaxAuthFailures: 5,
			AuthBanDuration: "1s",
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

func TestMiddlewareMissingManagementKeyDoesNotBanClient(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash management key: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote: true,
			SecretKey:   string(hash),
		},
	}, nil)
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	for i := 0; i < 6; i++ {
		w := performManagementRequest(router, http.MethodGet, "/v0/management/ping", "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("missing key attempt %d status = %d, want %d; body=%s", i+1, w.Code, http.StatusUnauthorized, w.Body.String())
		}
	}

	w := performManagementRequest(router, http.MethodGet, "/v0/management/ping", "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("valid key after missing-key attempts status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestMiddlewareInvalidManagementKeyStillBansClient(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash management key: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote:     true,
			SecretKey:       string(hash),
			MaxAuthFailures: 5,
			AuthBanDuration: "1s",
		},
	}, nil)
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	for i := 0; i < 4; i++ {
		w := performManagementRequest(router, http.MethodGet, "/v0/management/ping", "wrong")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid key attempt %d status = %d, want %d; body=%s", i+1, w.Code, http.StatusUnauthorized, w.Body.String())
		}
	}

	w := performManagementRequest(router, http.MethodGet, "/v0/management/ping", "wrong")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("fifth invalid key status = %d, want %d; body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}

	w = performManagementRequest(router, http.MethodGet, "/v0/management/ping", "wrong")
	if w.Code != http.StatusForbidden {
		t.Fatalf("sixth invalid key after ban triggered status = %d, want %d; body=%s", w.Code, http.StatusForbidden, w.Body.String())
	}

	w = performManagementRequest(router, http.MethodGet, "/v0/management/ping", "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("valid key should unban and succeed status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestMiddlewareMaxAuthFailuresZeroDisablesBan(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash management key: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		RemoteManagement: config.RemoteManagement{
			AllowRemote:     true,
			SecretKey:       string(hash),
			MaxAuthFailures: -1,
		},
	}, nil)
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	for i := 0; i < 50; i++ {
		w := performManagementRequest(router, http.MethodGet, "/v0/management/ping", "wrong")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid key attempt %d status = %d, want %d; body=%s", i+1, w.Code, http.StatusUnauthorized, w.Body.String())
		}
	}

	w := performManagementRequest(router, http.MethodGet, "/v0/management/ping", "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("valid key after many invalid attempts (ban disabled) status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}

func performManagementRequest(router http.Handler, method, target, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "203.0.113.10:12345"
	if key != "" {
		req.Header.Set("X-Management-Key", key)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}
