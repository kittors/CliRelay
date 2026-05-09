package management

import (
	"net/http"
	"net/http/httptest"
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

	w = performManagementRequest(router, http.MethodGet, "/v0/management/ping", "secret")
	if w.Code != http.StatusForbidden {
		t.Fatalf("valid key while banned status = %d, want %d; body=%s", w.Code, http.StatusForbidden, w.Body.String())
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
			MaxAuthFailures: -1, // disable ban
		},
	}, nil)
	defer h.Close()

	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Exhaust many more than the default threshold
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
