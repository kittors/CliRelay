package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
)

func TestShouldSkipMethodForRequestLogging(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		skip bool
	}{
		{
			name: "nil request",
			req:  nil,
			skip: true,
		},
		{
			name: "post request should not skip",
			req: &http.Request{
				Method: http.MethodPost,
				URL:    &url.URL{Path: "/v1/responses"},
			},
			skip: false,
		},
		{
			name: "plain get should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/models"},
				Header: http.Header{},
			},
			skip: true,
		},
		{
			name: "responses websocket upgrade should not skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			skip: false,
		},
		{
			name: "responses get without upgrade should skip",
			req: &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/v1/responses"},
				Header: http.Header{},
			},
			skip: true,
		},
	}

	for i := range tests {
		got := shouldSkipMethodForRequestLogging(tests[i].req)
		if got != tests[i].skip {
			t.Fatalf("%s: got skip=%t, want %t", tests[i].name, got, tests[i].skip)
		}
	}
}

func TestShouldCaptureRequestBody(t *testing.T) {
	tests := []struct {
		name          string
		loggerEnabled bool
		req           *http.Request
		want          bool
	}{
		{
			name:          "logger enabled unknown size skipped",
			loggerEnabled: true,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "logger enabled small known size captures",
			loggerEnabled: true,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: 2,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: true,
		},
		{
			name:          "logger enabled large known size skipped",
			loggerEnabled: true,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: maxErrorOnlyCapturedRequestBodyBytes + 1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "nil request",
			loggerEnabled: false,
			req:           nil,
			want:          false,
		},
		{
			name:          "small known size json in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("{}")),
				ContentLength: 2,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: true,
		},
		{
			name:          "large known size skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: maxErrorOnlyCapturedRequestBodyBytes + 1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "unknown size skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: -1,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
			},
			want: false,
		},
		{
			name:          "multipart skipped in error-only mode",
			loggerEnabled: false,
			req: &http.Request{
				Body:          io.NopCloser(strings.NewReader("x")),
				ContentLength: 1,
				Header:        http.Header{"Content-Type": []string{"multipart/form-data; boundary=abc"}},
			},
			want: false,
		},
	}

	for i := range tests {
		got := shouldCaptureRequestBody(tests[i].loggerEnabled, tests[i].req)
		if got != tests[i].want {
			t.Fatalf("%s: got %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

type stubStreamingLogWriter struct{}

func (stubStreamingLogWriter) WriteChunkAsync([]byte)                     {}
func (stubStreamingLogWriter) WriteStatus(int, map[string][]string) error { return nil }
func (stubStreamingLogWriter) WriteAPIRequest([]byte) error               { return nil }
func (stubStreamingLogWriter) WriteAPIResponse([]byte) error              { return nil }
func (stubStreamingLogWriter) SetFirstChunkTimestamp(time.Time)           {}
func (stubStreamingLogWriter) Close() error                               { return nil }

type stubRequestLogger struct{}

func (stubRequestLogger) LogRequest(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []*interfaces.ErrorMessage, string, time.Time, time.Time) error {
	return nil
}

func (stubRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return stubStreamingLogWriter{}, nil
}

func (stubRequestLogger) IsEnabled() bool { return true }

type captureRequestLogger struct {
	requestBody []byte
}

func (l *captureRequestLogger) LogRequest(_ string, _ string, _ map[string][]string, requestBody []byte, _ int, _ map[string][]string, _ []byte, _ []byte, _ []byte, _ []*interfaces.ErrorMessage, _ string, _ time.Time, _ time.Time) error {
	l.requestBody = bytes.Clone(requestBody)
	return nil
}

func (l *captureRequestLogger) LogStreamingRequest(_ string, _ string, _ map[string][]string, requestBody []byte, _ string) (logging.StreamingLogWriter, error) {
	l.requestBody = bytes.Clone(requestBody)
	return stubStreamingLogWriter{}, nil
}

func (l *captureRequestLogger) IsEnabled() bool { return true }

type diagnosticRequestLogger struct {
	url        string
	requestID  string
	statusCode int
	calls      int
	diagnostic diagnostics.Snapshot
}

func (l *diagnosticRequestLogger) LogRequest(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []*interfaces.ErrorMessage, string, time.Time, time.Time) error {
	return nil
}

func (l *diagnosticRequestLogger) LogRequestWithOptionsAndDiagnostics(url string, _ string, _ map[string][]string, _ []byte, statusCode int, _ map[string][]string, _ []byte, _ []byte, _ []byte, _ []*interfaces.ErrorMessage, _ bool, requestID string, _ time.Time, _ time.Time, diagnostic diagnostics.Snapshot) error {
	l.calls++
	l.url = url
	l.requestID = requestID
	l.statusCode = statusCode
	l.diagnostic = diagnostic
	return nil
}

func (l *diagnosticRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return stubStreamingLogWriter{}, nil
}

func (l *diagnosticRequestLogger) IsEnabled() bool { return false }

func gzipBodyForRequestLoggingTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestRequestLoggingMiddlewareDoesNotRejectOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var reachedHandler bool
	r := gin.New()
	r.Use(RequestLoggingMiddleware(stubRequestLogger{}))
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		reachedHandler = true
		c.Status(http.StatusNoContent)
	})

	body := bytes.Repeat([]byte("a"), int(maxErrorOnlyCapturedRequestBodyBytes)+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, w.Code)
	}
	if !reachedHandler {
		t.Fatal("request should reach handler; logging must not enforce API body limits")
	}
}

func TestRequestLoggingMiddlewareUsesCachedDecodedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	previousThreshold := bodyutil.RequestBodyDiskThreshold()
	t.Cleanup(func() {
		bodyutil.SetModelRequestBodyLimit(previousLimit)
		bodyutil.SetRequestBodyDiskThreshold(previousThreshold)
	})
	bodyutil.SetModelRequestBodyLimit(1 << 20)
	bodyutil.SetRequestBodyDiskThreshold(1 << 20)

	raw := []byte(`{"model":"gpt-5.5","input":"hello","stream":false}`)
	logger := &captureRequestLogger{}
	r := gin.New()
	r.Use(DecompressRequestMiddleware())
	r.Use(RequestBodyCleanupMiddleware())
	r.Use(RequestLoggingMiddleware(logger))
	r.POST("/v1/responses", func(c *gin.Context) {
		body, err := bodyutil.ReadRequestBody(c, bodyutil.ModelRequestBodyLimit())
		if err != nil {
			t.Fatalf("ReadRequestBody: %v", err)
		}
		if string(body) != string(raw) {
			t.Fatalf("decoded body = %s", body)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(gzipBodyForRequestLoggingTest(t, raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusNoContent, w.Code, w.Body.String())
	}
	if string(logger.requestBody) != string(raw) {
		t.Fatalf("logged request body = %s, want %s", logger.requestBody, raw)
	}
}

func TestRequestLoggingMiddlewarePreservesOriginalURLAcrossRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logger := &diagnosticRequestLogger{}
	r := gin.New()
	r.Use(RequestLoggingMiddleware(logger))
	r.POST("/v1/responses", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error": map[string]any{
				"message": "RPM limit (10 requests/min) exceeded for this API key",
				"type":    "rate_limit_exceeded",
				"code":    "rpm_limit_exceeded",
			},
		})
	})
	r.NoRoute(func(c *gin.Context) {
		if _, rewritten := c.Get("test.rewritten"); rewritten {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Set("test.rewritten", true)
		c.Request.URL.Path = "/v1/responses"
		c.Request.RequestURI = "/v1/responses"
		diagnostics.SetEffectiveURL(c, "/v1/responses")
		r.HandleContext(c)
		c.Abort()
	})

	req := httptest.NewRequest(http.MethodPost, "/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses", strings.NewReader(`{"model":"deepseek","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusTooManyRequests, w.Body.String())
	}
	if logger.calls != 1 {
		t.Fatalf("log calls = %d, want 1", logger.calls)
	}
	if logger.url != "/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses" {
		t.Fatalf("logged url = %q", logger.url)
	}
	if logger.requestID == "" {
		t.Fatal("request ID should be generated for rewritten requests")
	}
	if logger.statusCode != http.StatusTooManyRequests {
		t.Fatalf("logged status = %d, want %d", logger.statusCode, http.StatusTooManyRequests)
	}
	if logger.diagnostic.OriginalURL != "/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses" {
		t.Fatalf("diagnostic original URL = %q", logger.diagnostic.OriginalURL)
	}
	if logger.diagnostic.EffectiveURL != "/v1/responses" {
		t.Fatalf("diagnostic effective URL = %q", logger.diagnostic.EffectiveURL)
	}
	if logger.diagnostic.Response == nil || logger.diagnostic.Response.ErrorCode != "rpm_limit_exceeded" {
		t.Fatalf("diagnostic response = %#v", logger.diagnostic.Response)
	}
}
