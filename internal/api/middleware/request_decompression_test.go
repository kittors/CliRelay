package middleware

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
)

func zstdRequestBody(t *testing.T, raw []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(raw, nil)
}

func gzipRequestBody(t *testing.T, raw []byte) []byte {
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

func brotliRequestBody(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := brotli.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

func deflateRequestBody(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zlib.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		t.Fatalf("deflate write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("deflate close: %v", err)
	}
	return buf.Bytes()
}

func TestDecompressRequestMiddlewareDecodesSupportedContentEncodings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	t.Cleanup(func() { bodyutil.SetModelRequestBodyLimit(previousLimit) })
	bodyutil.SetModelRequestBodyLimit(1 << 20)

	raw := []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`)
	tests := []struct {
		name     string
		encoding string
		body     []byte
	}{
		{name: "gzip", encoding: "gzip", body: gzipRequestBody(t, raw)},
		{name: "x-gzip", encoding: "x-gzip", body: gzipRequestBody(t, raw)},
		{name: "br", encoding: "br", body: brotliRequestBody(t, raw)},
		{name: "zstd", encoding: "zstd", body: zstdRequestBody(t, raw)},
		{name: "deflate", encoding: "deflate", body: deflateRequestBody(t, raw)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			r.Use(DecompressRequestMiddleware())
			r.POST("/v1/responses", func(c *gin.Context) {
				body, err := bodyutil.ReadRequestBody(c, bodyutil.ModelRequestBodyLimit())
				if err != nil {
					t.Fatalf("ReadRequestBody: %v", err)
				}
				if got := c.GetHeader("Content-Encoding"); got != "" {
					t.Fatalf("Content-Encoding = %q, want empty", got)
				}
				if string(body) != string(raw) {
					t.Fatalf("decoded body = %q, want %q", body, raw)
				}
				c.Status(http.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Encoding", tt.encoding)
			w := httptest.NewRecorder()

			r.ServeHTTP(w, req)

			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDecompressRequestMiddlewareDetectsZstdWithoutHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLimit := bodyutil.ModelRequestBodyLimit()
	t.Cleanup(func() { bodyutil.SetModelRequestBodyLimit(previousLimit) })
	bodyutil.SetModelRequestBodyLimit(1 << 20)

	raw := []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`)
	r := gin.New()
	r.Use(DecompressRequestMiddleware())
	r.POST("/v1/responses", func(c *gin.Context) {
		body, err := bodyutil.ReadRequestBody(c, bodyutil.ModelRequestBodyLimit())
		if err != nil {
			t.Fatalf("ReadRequestBody: %v", err)
		}
		if c.GetHeader("Content-Encoding") != "" {
			t.Fatalf("Content-Encoding was not cleared")
		}
		if string(body) != string(raw) {
			t.Fatalf("decoded body = %q, want %q", body, raw)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(zstdRequestBody(t, raw)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestDecompressRequestMiddlewareRejectsUnsupportedEncoding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(DecompressRequestMiddleware())
	r.POST("/v1/responses", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Encoding", "snappy")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusUnsupportedMediaType, w.Body.String())
	}
}
