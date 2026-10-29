package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Made-up credentials in the shapes the scrubber recognises.
const (
	scrubTestKey         = "sk-proj-Zq4XyL8mN2vB6cR1tK9wP3sD7fG5hJ0aE4uI8oY2"
	scrubTestCookieValue = "9f8e7d6c5b4a3210"
)

var (
	scrubTestHeaders = map[string][]string{
		"Authorization": {"Bearer " + scrubTestKey},
		"Cookie":        {"session=" + scrubTestCookieValue},
		"X-Debug-Note":  {"rotated " + scrubTestKey},
	}
	scrubTestBody        = []byte(`{"messages":[{"role":"user","content":"OPENAI_API_KEY=` + scrubTestKey + `"}]}`)
	scrubTestAPIRequest  = []byte("POST https://api.openai.com/v1/responses\nAuthorization: Bearer " + scrubTestKey + "\nCookie: session=" + scrubTestCookieValue + "\n\n" + string(scrubTestBody))
	scrubTestAPIResponse = []byte(`{"error":{"message":"Incorrect API key provided: ` + scrubTestKey + `"}}`)
)

// onlyLogFile returns the content of the single log file in dir and checks
// that no temp file was left behind.
func onlyLogFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", dir, err)
	}
	var logs []string
	for _, entry := range entries {
		switch filepath.Ext(entry.Name()) {
		case ".log":
			logs = append(logs, entry.Name())
		case ".tmp":
			t.Errorf("temp file left behind: %s", entry.Name())
		}
	}
	if len(logs) != 1 {
		t.Fatalf("log files = %v, want one", logs)
	}
	data, err := os.ReadFile(filepath.Join(dir, logs[0]))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return string(data)
}

func assertLogScrubbed(t *testing.T, content string) {
	t.Helper()
	for _, secret := range []string{scrubTestKey, scrubTestCookieValue} {
		if strings.Contains(content, secret) {
			t.Errorf("log file still holds %q:\n%s", secret, content)
		}
	}
	for _, want := range []string{"[REDACTED:", "=== REQUEST BODY ===", "=== API REQUEST ===", "=== API RESPONSE ===", "OPENAI_API_KEY="} {
		if !strings.Contains(content, want) {
			t.Errorf("log file lacks %q:\n%s", want, content)
		}
	}
}

// Error logs are written even with request logging off; every section of
// them, headers included, is scrubbed.
func TestErrorLogFileIsScrubbed(t *testing.T) {
	t.Parallel()

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 10)
	if err := logger.LogRequestWithOptions(
		"/v1/responses",
		"POST",
		scrubTestHeaders,
		scrubTestBody,
		401,
		map[string][]string{"Set-Cookie": {"session=" + scrubTestCookieValue + "; Path=/; HttpOnly"}},
		scrubTestAPIResponse,
		scrubTestAPIRequest,
		scrubTestAPIResponse,
		nil,
		true,
		"",
		timeZero(),
		timeZero(),
	); err != nil {
		t.Fatalf("LogRequestWithOptions() error = %v", err)
	}
	assertLogScrubbed(t, onlyLogFile(t, logsDir))
}

func TestStreamingLogFileIsScrubbed(t *testing.T) {
	t.Parallel()

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(true, logsDir, "", 10)
	writer, err := logger.LogStreamingRequest("/v1/responses", "POST", scrubTestHeaders, scrubTestBody, "")
	if err != nil {
		t.Fatalf("LogStreamingRequest() error = %v", err)
	}
	if err := writer.WriteStatus(200, map[string][]string{"Content-Type": {"text/event-stream"}}); err != nil {
		t.Fatalf("WriteStatus() error = %v", err)
	}
	if err := writer.WriteAPIRequest(scrubTestAPIRequest); err != nil {
		t.Fatalf("WriteAPIRequest() error = %v", err)
	}
	if err := writer.WriteAPIResponse(scrubTestAPIResponse); err != nil {
		t.Fatalf("WriteAPIResponse() error = %v", err)
	}
	writer.WriteChunkAsync([]byte(`data: {"delta":"Using ` + scrubTestKey + ` now"}` + "\n\n"))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	assertLogScrubbed(t, onlyLogFile(t, logsDir))
}
