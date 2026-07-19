package logging

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func TestGinLogrusRecoveryRepanicsErrAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/abort", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	recorder := httptest.NewRecorder()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic, got nil")
		}
		err, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T", recovered)
		}
		if !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("expected ErrAbortHandler, got %v", err)
		}
		if err != http.ErrAbortHandler {
			t.Fatalf("expected exact ErrAbortHandler sentinel, got %v", err)
		}
	}()

	engine.ServeHTTP(recorder, req)
}

func TestGinLogrusRecoveryHandlesRegularPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
}

func TestGinLogrusLoggerPrefersForwardedClientIPForCDNRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	originalOutput := log.StandardLogger().Out
	defer log.SetOutput(originalOutput)
	log.SetOutput(&buf)

	engine := gin.New()
	if err := engine.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies(nil): %v", err)
	}
	engine.Use(GinLogrusLogger())
	engine.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.RemoteAddr = "198.51.100.10:4321"
	req.Header.Set("CF-Connecting-IP", "203.0.113.24")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	line := buf.String()
	if !strings.Contains(line, "203.0.113.24") {
		t.Fatalf("log line = %q, want real forwarded client IP", line)
	}
	if strings.Contains(line, "198.51.100.10") {
		t.Fatalf("log line = %q, should not use proxy connection IP", line)
	}
}

func TestGinLogrusLoggerAssignsRequestIDForManagementPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var got string
	engine := gin.New()
	engine.Use(GinLogrusLogger())
	engine.GET("/v0/management/request-log-storage/store-content", func(c *gin.Context) {
		got = GetGinRequestID(c)
		if ctxID := GetRequestID(c.Request.Context()); ctxID != got {
			t.Errorf("context request id = %q, gin = %q", ctxID, got)
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v0/management/request-log-storage/store-content", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got == "" {
		t.Fatal("expected management path to receive a request ID")
	}
	if len(got) != 8 {
		t.Fatalf("request ID length = %d, want 8 (got %q)", len(got), got)
	}
}

func TestGinLogrusLoggerLogsNestedRouteRewriteOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	originalOutput := log.StandardLogger().Out
	defer log.SetOutput(originalOutput)
	log.SetOutput(&buf)

	engine := gin.New()
	engine.Use(GinLogrusLogger())
	engine.POST("/v1/responses", func(c *gin.Context) {
		if GetGinRequestID(c) == "" {
			t.Error("rewritten AI route should receive a request ID")
		}
		c.Status(http.StatusNoContent)
	})
	engine.NoRoute(func(c *gin.Context) {
		if _, rewritten := c.Get("test.rewritten"); rewritten {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Set("test.rewritten", true)
		c.Request.URL.Path = "/v1/responses"
		c.Request.RequestURI = "/v1/responses"
		engine.HandleContext(c)
		c.Abort()
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/deepseek/group/v1/responses", nil)
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("gin log lines = %d, want 1: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "/deepseek/group/v1/responses") {
		t.Fatalf("log line = %q, want original public path", lines[0])
	}
}

func TestShouldTrackRequestID(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/messages", true},
		{"/v0/management/audit-logs", true},
		{"/management/health", true},
		{"/healthz", false},
		{"/api/other", false},
	}
	for _, tc := range cases {
		if got := shouldTrackRequestID(tc.path); got != tc.want {
			t.Errorf("shouldTrackRequestID(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
