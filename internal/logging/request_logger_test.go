package logging

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
)

func TestSanitizeForFilenameNormalizesUnsafeCharacters(t *testing.T) {
	t.Parallel()

	logger := &FileRequestLogger{}
	got := logger.sanitizeForFilename("/v1/responses: latest?test")
	if got != "v1-responses-latest-test" {
		t.Fatalf("sanitizeForFilename = %q, want %q", got, "v1-responses-latest-test")
	}
}

func TestDecompressResponseHandlesGzip(t *testing.T) {
	t.Parallel()

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte("hello gzip")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	logger := &FileRequestLogger{}
	got, err := logger.decompressResponse(map[string][]string{
		"Content-Encoding": {"gzip"},
	}, compressed.Bytes())
	if err != nil {
		t.Fatalf("decompressResponse returned error: %v", err)
	}
	if string(got) != "hello gzip" {
		t.Fatalf("decompressed body = %q, want %q", string(got), "hello gzip")
	}
}

func TestLogRequestWithOptionsCleansUpOldErrorLogs(t *testing.T) {
	t.Parallel()

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 1)

	for i := 0; i < 2; i++ {
		if err := logger.LogRequestWithOptions(
			"/v1/responses",
			"POST",
			map[string][]string{"Authorization": {"Bearer secret-token-value"}},
			[]byte("request"),
			500,
			nil,
			[]byte("response"),
			nil,
			nil,
			nil,
			true,
			"",
			timeZero(),
			timeZero(),
		); err != nil {
			t.Fatalf("LogRequestWithOptions() error = %v", err)
		}
	}

	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", logsDir, err)
	}

	var errorLogs []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "error-") && filepath.Ext(entry.Name()) == ".log" {
			errorLogs = append(errorLogs, entry.Name())
		}
	}
	if len(errorLogs) != 1 {
		t.Fatalf("error log files = %d, want 1; files=%v", len(errorLogs), errorLogs)
	}
}

func TestLogRequestWithDiagnosticsUsesOriginalURL(t *testing.T) {
	t.Parallel()

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	diagnostic := diagnostics.Snapshot{
		RequestID:    "reqabc123",
		OriginalURL:  "/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses",
		EffectiveURL: "/v1/responses",
		Route: &diagnostics.RouteSnapshot{
			RoutePath: "/deepseekv4flash-chatgpt/cs_3e13ca9880fc",
			Group:     "deepseekv4flash-chatgpt",
		},
		Quota: &diagnostics.QuotaSnapshot{
			Rejected:     true,
			RejectedBy:   "rpm",
			RPMLimit:     10,
			Current:      11,
			ErrorCode:    "rpm_limit_exceeded",
			ErrorType:    "rate_limit_exceeded",
			ErrorMessage: "RPM limit (10 requests/min) exceeded for this API key",
		},
		Response: &diagnostics.ResponseSnapshot{
			Status:       429,
			ErrorCode:    "rpm_limit_exceeded",
			ErrorType:    "rate_limit_exceeded",
			ErrorMessage: "RPM limit (10 requests/min) exceeded for this API key",
			Source:       "local_quota",
		},
	}

	if err := logger.LogRequestWithOptionsAndDiagnostics(
		"/v1/responses",
		"POST",
		map[string][]string{"Authorization": {"Bearer secret-token-value"}},
		[]byte(`{"model":"deepseek","input":"hello"}`),
		429,
		nil,
		[]byte(`{"error":{"code":"rpm_limit_exceeded"}}`),
		nil,
		nil,
		nil,
		true,
		"reqabc123",
		timeZero(),
		timeZero(),
		diagnostic,
	); err != nil {
		t.Fatalf("LogRequestWithOptionsAndDiagnostics() error = %v", err)
	}

	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", logsDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("log files = %d, want 1", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasPrefix(name, "error-deepseekv4flash-chatgpt-cs_3e13ca9880fc-v1-responses-") {
		t.Fatalf("error log name = %q, want original custom route prefix", name)
	}
	content, err := os.ReadFile(filepath.Join(logsDir, name))
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", name, err)
	}
	text := string(content)
	for _, want := range []string{
		"URL: /deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses",
		"Effective URL: /v1/responses",
		"=== DIAGNOSTIC METADATA ===",
		`"original_url": "/deepseekv4flash-chatgpt/cs_3e13ca9880fc/v1/responses"`,
		`"rejected_by": "rpm"`,
		`"source": "local_quota"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("log content missing %q:\n%s", want, text)
		}
	}
}

func timeZero() time.Time {
	return time.Time{}
}
