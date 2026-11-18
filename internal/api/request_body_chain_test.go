package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

type chainStreamingLogWriter struct{}

func (chainStreamingLogWriter) WriteChunkAsync([]byte)                     {}
func (chainStreamingLogWriter) NoteDroppedChunks(int)                      {}
func (chainStreamingLogWriter) WriteStatus(int, map[string][]string) error { return nil }
func (chainStreamingLogWriter) WriteAPIRequest([]byte) error               { return nil }
func (chainStreamingLogWriter) WriteAPIResponse([]byte) error              { return nil }
func (chainStreamingLogWriter) SetFirstChunkTimestamp(time.Time)           {}
func (chainStreamingLogWriter) Close() error                               { return nil }

type chainRequestLogger struct{}

func (chainRequestLogger) LogRequest(string, string, map[string][]string, []byte, int, map[string][]string, []byte, []byte, []byte, []*interfaces.ErrorMessage, string, time.Time, time.Time) error {
	return nil
}

func (chainRequestLogger) LogStreamingRequest(string, string, map[string][]string, []byte, string) (logging.StreamingLogWriter, error) {
	return chainStreamingLogWriter{}, nil
}

func (chainRequestLogger) IsEnabled() bool { return true }

func encodeZstdForRequestBodyTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(raw, nil)
}

func TestResponsesBodyChainAllowsPayloadAboveLegacyLimitWithRequestLogging(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	t.Cleanup(func() { bodyutil.SetModelRequestBodyLimit(previousLimit) })
	bodyutil.SetModelRequestBodyLimit(32 << 20)

	input := bytes.Repeat([]byte("a"), (17<<20)+1)
	var payload bytes.Buffer
	payload.Grow(len(input) + 64)
	payload.WriteString(`{"model":"gpt-5.5","input":"`)
	payload.Write(input)
	payload.WriteString(`","stream":true}`)

	r := gin.New()
	r.Use(middleware.DecompressRequestMiddleware())
	r.Use(middleware.RequestBodyCleanupMiddleware())
	r.Use(middleware.RequestLoggingMiddleware(chainRequestLogger{}))
	r.POST("/v1/responses", func(c *gin.Context) {
		body, ok := sdkhandlers.ReadJSONRequestBody(c)
		if !ok {
			return
		}
		if gjson.GetBytes(body, "model").String() != "gpt-5.5" {
			t.Fatalf("model not parsed from body")
		}
		c.JSON(http.StatusOK, gin.H{"body_size": len(body)})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload.Bytes()))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestResponsesBodyChainDecodesZstdAndRefreshesBodyAfterSystemPrompt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	t.Cleanup(func() { bodyutil.SetModelRequestBodyLimit(previousLimit) })
	bodyutil.SetModelRequestBodyLimit(1 << 20)

	raw := []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`)
	r := gin.New()
	r.Use(middleware.DecompressRequestMiddleware())
	r.Use(middleware.RequestBodyCleanupMiddleware())
	r.Use(middleware.RequestLoggingMiddleware(chainRequestLogger{}))
	r.Use(func(c *gin.Context) {
		c.Set("accessMetadata", map[string]string{"system-prompt": "system from metadata"})
		c.Next()
	})
	r.Use(SystemPromptMiddleware())
	r.POST("/v1/responses", func(c *gin.Context) {
		body, ok := sdkhandlers.ReadJSONRequestBody(c)
		if !ok {
			return
		}
		if got := gjson.GetBytes(body, "instructions").String(); got != "system from metadata" {
			t.Fatalf("instructions = %q", got)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encodeZstdForRequestBodyTest(t, raw)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestResponsesBodyChainRejectsOversizedDecodedZstdBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	t.Cleanup(func() { bodyutil.SetModelRequestBodyLimit(previousLimit) })
	bodyutil.SetModelRequestBodyLimit(512)

	input := bytes.Repeat([]byte("a"), 4096)
	var payload bytes.Buffer
	payload.WriteString(`{"model":"gpt-5.5","input":"`)
	payload.Write(input)
	payload.WriteString(`","stream":true}`)

	r := gin.New()
	r.Use(middleware.DecompressRequestMiddleware())
	r.Use(middleware.RequestBodyCleanupMiddleware())
	r.Use(middleware.RequestLoggingMiddleware(chainRequestLogger{}))
	r.POST("/v1/responses", func(c *gin.Context) {
		_, _ = sdkhandlers.ReadJSONRequestBody(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encodeZstdForRequestBodyTest(t, payload.Bytes())))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusRequestEntityTooLarge, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("request_body_too_large")) {
		t.Fatalf("expected request_body_too_large response, got %s", w.Body.String())
	}
}
