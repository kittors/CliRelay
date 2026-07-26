package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// newCLITestRouter wires CLIHandler exactly as routes_public.go does: a single
// wildcard route on the engine root, outside every authenticated route group. A nil
// BaseAPIHandler is safe here because every case in this file is rejected by the
// local-origin gate before the handler touches its dependencies.
func newCLITestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1internal:method", NewGeminiCLIAPIHandler(nil).CLIHandler)
	return router
}

// Issue #517: behind a same-host reverse proxy the TCP peer is the proxy, so the old
// RemoteAddr prefix check let external callers reach the credential pool.
func TestCLIHandlerRejectsReverseProxiedRequests(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"external forwarded client", "X-Forwarded-For", "203.0.113.42"},
		{"spoofed loopback first hop", "X-Forwarded-For", "127.0.0.1, 203.0.113.42"},
		{"forwarded rfc7239", "Forwarded", "for=203.0.113.42;proto=https"},
		{"cloudflare", "CF-Connecting-IP", "203.0.113.42"},
		{"proxy hop marker only", "Via", "1.1 nginx"},
		{"tls terminated upstream", "X-Forwarded-Proto", "https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newCLITestRouter()
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", strings.NewReader(`{}`))
			req.RemoteAddr = "127.0.0.1:54321"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(tc.header, tc.value)
			router.ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "only allow local access") {
				t.Fatalf("body = %s, want local-access rejection", rr.Body.String())
			}
		})
	}
}

func TestCLIHandlerRejectsNonLoopbackPeers(t *testing.T) {
	router := newCLITestRouter()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", strings.NewReader(`{}`))
	req.RemoteAddr = "203.0.113.10:54321"
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
}

// A genuine local CLI must still get past the gate. It fails later (nil dependencies),
// so the assertion is only that it was not rejected as non-local.
func TestCLIHandlerAcceptsDirectLoopbackPeers(t *testing.T) {
	for _, remoteAddr := range []string{"127.0.0.1:54321", "[::1]:54321"} {
		t.Run(remoteAddr, func(t *testing.T) {
			router := newCLITestRouter()
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", strings.NewReader(`{}`))
			req.RemoteAddr = remoteAddr
			req.Header.Set("Content-Type", "application/json")

			defer func() {
				// Reaching a nil-dependency panic proves the request passed the gate,
				// which is what this test is about.
				_ = recover()
			}()
			router.ServeHTTP(rr, req)

			if rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), "only allow local access") {
				t.Fatalf("loopback peer %s was rejected as non-local: %s", remoteAddr, rr.Body.String())
			}
		})
	}
}

// This endpoint is actively scanned and rejects before reading the body, so an
// unthrottled warning per rejected request is a cheap way to fill an operator's disk.
func TestRejectionLoggingIsThrottled(t *testing.T) {
	var lines int
	original := log.StandardLogger().Out
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		lines++
		return len(p), nil
	}))
	t.Cleanup(func() { log.SetOutput(original) })

	geminiCLIRejectLog.mu.Lock()
	geminiCLIRejectLog.lastAt = time.Time{}
	geminiCLIRejectLog.suppressed = 0
	geminiCLIRejectLog.mu.Unlock()

	router := newCLITestRouter()
	for i := 0; i < 50; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set("X-Forwarded-For", "203.0.113.42")
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("request %d: status = %d, want 403", i, rr.Code)
		}
	}

	if lines != 1 {
		t.Fatalf("50 rejected requests produced %d log line(s), want 1", lines)
	}
	geminiCLIRejectLog.mu.Lock()
	suppressed := geminiCLIRejectLog.suppressed
	geminiCLIRejectLog.mu.Unlock()
	if suppressed != 49 {
		t.Fatalf("suppressed count = %d, want 49", suppressed)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
