package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestUsageReporterSpillsLargeStreamingOutputToTempFile(t *testing.T) {
	reporter := newUsageReporter(context.Background(), "provider", "model", nil)
	chunk := bytes.Repeat([]byte("x"), usageReporterOutputMemoryLimit/2)

	reporter.appendOutputChunk(chunk)
	reporter.appendOutputChunk(chunk)

	if reporter.outputPath == "" {
		t.Fatalf("expected outputPath to be set after spilling to temp file")
	}
	tempPath := reporter.outputPath

	_, output := reporter.finalizeContent()
	expected := string(chunk) + "\n" + string(chunk) + "\n"
	if output != expected {
		t.Fatalf("unexpected output length/content: got=%d want=%d", len(output), len(expected))
	}

	if reporter.outputPath != "" {
		t.Fatalf("expected outputPath to be cleared after finalizeContent")
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("expected temp file to be removed, stat err=%v", err)
	}
}

func TestShouldSuppressUsageFailureForContextCanceled(t *testing.T) {
	if !shouldSuppressUsageFailure(context.Canceled, "") {
		t.Fatal("context.Canceled should not be published as a failed usage record")
	}
	wrapped := &urlErrorForTest{err: context.Canceled}
	if !shouldSuppressUsageFailure(wrapped, "") {
		t.Fatal("wrapped context.Canceled should not be published as a failed usage record")
	}
	if !shouldSuppressUsageFailure(nil, `Post "https://chatgpt.com/backend-api/codex/responses": context canceled`) {
		t.Fatal("context canceled output text should not be published as a failed usage record")
	}
	if shouldSuppressUsageFailure(errors.New("upstream 500"), "") {
		t.Fatal("ordinary upstream errors should still be published as failed usage records")
	}
}

type urlErrorForTest struct {
	err error
}

func (e *urlErrorForTest) Error() string {
	return `Post "https://chatgpt.com/backend-api/codex/responses": ` + e.err.Error()
}

func (e *urlErrorForTest) Unwrap() error {
	return e.err
}

func TestRequestDetailsCaptureUpstreamLogsWhenOnlyContentStorageEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("User-Agent", "codex-cli-test")
	req.RemoteAddr = "203.0.113.9:45678"
	ginCtx.Request = req
	ctx := context.WithValue(req.Context(), util.ContextKeyGin, ginCtx)
	cfg := &config.Config{}
	cfg.RequestLog = false
	cfg.RequestLogStorage.StoreContent = true

	recordAPIRequest(ctx, cfg, upstreamRequestLog{
		URL:     "https://api.example.test/v1/responses",
		Method:  http.MethodPost,
		Headers: http.Header{"X-Codex-Session-Id": []string{"session-plaintext"}},
		Body:    []byte(`{"model":"gpt-test"}`),
	})
	recordAPIResponseMetadata(ctx, cfg, http.StatusOK, http.Header{"X-Request-Id": []string{"req-plaintext"}})
	appendAPIResponseChunk(ctx, cfg, []byte(`{"id":"resp-test"}`))

	var detail struct {
		Upstream struct {
			RequestLog string `json:"request_log"`
		} `json:"upstream"`
		Response struct {
			UpstreamLog string `json:"upstream_log"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(buildRequestDetailContent(ctx)), &detail); err != nil {
		t.Fatalf("unmarshal request details: %v", err)
	}
	if !strings.Contains(detail.Upstream.RequestLog, "https://api.example.test/v1/responses") {
		t.Fatalf("upstream request log missing URL: %q", detail.Upstream.RequestLog)
	}
	if !strings.Contains(detail.Response.UpstreamLog, "X-Request-Id: req-plaintext") {
		t.Fatalf("upstream response log missing headers: %q", detail.Response.UpstreamLog)
	}
	if !strings.Contains(detail.Response.UpstreamLog, `{"id":"resp-test"}`) {
		t.Fatalf("upstream response log missing body: %q", detail.Response.UpstreamLog)
	}
}

func TestFirstTokenLatencyMsFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(nil)
	requestedAt := time.Date(2026, 4, 8, 10, 0, 0, 0, time.UTC)
	firstResponseAt := requestedAt.Add(183 * time.Millisecond)
	ginCtx.Set(util.GinKeyFirstResponseAt, firstResponseAt)

	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)

	if got := firstTokenLatencyMsFromContext(ctx, requestedAt); got != 183 {
		t.Fatalf("firstTokenLatencyMsFromContext() = %d, want %d", got, 183)
	}
}
