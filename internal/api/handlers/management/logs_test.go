package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestReadErrorLogSummaryParsesDiagnosticMetadata(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "error-deepseek-v1-responses-2026-07-03T103816-reqabc123.log")
	content := `=== REQUEST INFO ===
Version: dev-test
URL: /deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses
Effective URL: /v1/responses
Request ID: reqabc123
Method: POST
Timestamp: 2026-07-03T10:38:16Z

=== DIAGNOSTIC METADATA ===
{
  "request_id": "reqabc123",
  "original_url": "/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses",
  "effective_url": "/v1/responses",
  "route": {
    "route_path": "/deepseekv4flash-chatgpt/cs_3e13ca9880fc",
    "group": "deepseekv4flash-chatgpt"
  },
  "auth": {
    "provider": "api-key",
    "api_key": "sk-t...test",
    "api_key_id": "key-1",
    "api_key_name": "Codex Desktop"
  },
  "quota": {
    "rpm_limit": 10,
    "rejected": true,
    "rejected_by": "rpm",
    "current": 11,
    "error_code": "rpm_limit_exceeded",
    "error_type": "rate_limit_exceeded"
  },
  "response": {
    "status": 429,
    "error_code": "rpm_limit_exceeded",
    "error_type": "rate_limit_exceeded",
    "source": "local_quota"
  },
  "body": {
    "model": "deepseek-v4-flash",
    "captured_bytes": 48,
    "redacted": true
  }
}

=== HEADERS ===
Authorization: Bearer ***
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}

	summary := readErrorLogSummary(path)
	if summary.RequestID != "reqabc123" {
		t.Fatalf("request ID = %q", summary.RequestID)
	}
	if summary.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", summary.Status, http.StatusTooManyRequests)
	}
	if summary.ErrorCode != "rpm_limit_exceeded" || summary.ErrorType != "rate_limit_exceeded" {
		t.Fatalf("error summary = %#v", summary)
	}
	if summary.OriginalURL != "/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses" {
		t.Fatalf("original URL = %q", summary.OriginalURL)
	}
	if summary.EffectiveURL != "/v1/responses" {
		t.Fatalf("effective URL = %q", summary.EffectiveURL)
	}
	if summary.RouteGroup != "deepseekv4flash-chatgpt" || summary.RoutePath != "/deepseekv4flash-chatgpt/cs_3e13ca9880fc" {
		t.Fatalf("route summary = %#v", summary)
	}
	if summary.Provider != "api-key" || summary.Model != "deepseek-v4-flash" || summary.RejectedBy != "rpm" {
		t.Fatalf("metadata summary = %#v", summary)
	}
}

func TestGetRequestErrorLogsIncludesDiagnosticSummary(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	name := "error-deepseek-v1-responses-2026-07-03T103816-reqabc123.log"
	path := filepath.Join(dir, name)
	content := `=== REQUEST INFO ===
Version: dev-test
URL: /deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses
Effective URL: /v1/responses
Request ID: reqabc123

=== DIAGNOSTIC METADATA ===
{"request_id":"reqabc123","original_url":"/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses","effective_url":"/v1/responses","response":{"status":429,"error_code":"rpm_limit_exceeded"},"quota":{"rejected_by":"rpm"}}

=== RESPONSE ===
Status: 429
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	modified := time.Date(2026, 7, 3, 10, 38, 16, 0, time.UTC)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("Chtimes(%s) error = %v", path, err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{LoggingToFile: true}, nil)
	h.SetLogDirectory(dir)
	defer h.Close()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/request-error-logs", nil)
	h.GetRequestErrorLogs(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload struct {
		Files []struct {
			Name         string `json:"name"`
			RequestID    string `json:"request_id"`
			Status       int    `json:"status"`
			ErrorCode    string `json:"error_code"`
			OriginalURL  string `json:"original_url"`
			EffectiveURL string `json:"effective_url"`
			RejectedBy   string `json:"rejected_by"`
		} `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(payload.Files))
	}
	file := payload.Files[0]
	if file.Name != name || file.RequestID != "reqabc123" || file.Status != http.StatusTooManyRequests || file.ErrorCode != "rpm_limit_exceeded" || file.RejectedBy != "rpm" {
		t.Fatalf("file summary = %#v", file)
	}
	if file.OriginalURL != "/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses" || file.EffectiveURL != "/v1/responses" {
		t.Fatalf("file URLs = %#v", file)
	}
}
