package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestIsLocalGeminiCLIRequestAllowsLoopback(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	if !isLocalGeminiCLIRequest(req) {
		t.Fatal("expected loopback request to be allowed")
	}
}

func TestIsLocalGeminiCLIRequestAllowsIPv6Loopback(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", nil)
	req.RemoteAddr = "[::1]:12345"

	if !isLocalGeminiCLIRequest(req) {
		t.Fatal("expected IPv6 loopback request to be allowed")
	}
}

func TestIsLocalGeminiCLIRequestRejectsRemoteForwardedClient(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.42")

	if isLocalGeminiCLIRequest(req) {
		t.Fatal("expected reverse-proxied remote client to be rejected")
	}
}

func TestIsLocalGeminiCLIRequestRejectsSpoofedForwardedChain(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "127.0.0.1, 203.0.113.42")

	if isLocalGeminiCLIRequest(req) {
		t.Fatal("expected forwarded chain containing a remote client to be rejected")
	}
}

func TestGeminiCLIHandlerRejectsReverseProxiedRemoteClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGeminiCLIAPIHandler(nil)
	router.POST("/v1internal:method", handler.CLIHandler)

	req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", strings.NewReader(`{"model":"test"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.42")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
}
