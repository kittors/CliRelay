package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func setUsageBodyCaptureForTest(t *testing.T, enabled bool) {
	t.Helper()
	previous := internalusage.RequestLogBodyStorageEnabled()
	internalusage.SetRequestLogBodyStorageEnabled(enabled)
	t.Cleanup(func() { internalusage.SetRequestLogBodyStorageEnabled(previous) })
}

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

func TestParseOpenAIUsageUnwrapsProviderDataEnvelope(t *testing.T) {
	data := []byte(`{"success":true,"data":{"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 9 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 9)
	}
	if detail.OutputTokens != 5 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 5)
	}
	if detail.TotalTokens != 14 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 14)
	}
}

func TestParseCodexUsageAcceptsTopLevelUsage(t *testing.T) {
	data := []byte(`{"type":"response.done","usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":3}}}`)
	detail, ok := parseCodexUsage(data)
	if !ok {
		t.Fatal("parseCodexUsage() ok = false, want true")
	}
	if detail.InputTokens != 4 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 4)
	}
	if detail.OutputTokens != 6 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 6)
	}
	if detail.TotalTokens != 10 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 10)
	}
	if detail.CachedTokens != 2 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 2)
	}
	if detail.ReasoningTokens != 3 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 3)
	}
}

func TestParseOpenAIResponseModel(t *testing.T) {
	if got := parseOpenAIResponseModel([]byte(`{"model":"gpt-5.4","usage":{"total_tokens":1}}`)); got != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4", got)
	}
	if got := parseOpenAIResponseModel([]byte(`{"success":true,"data":{"model":"cline-pass/qwen3.7-max","usage":{"total_tokens":1}}}`)); got != "cline-pass/qwen3.7-max" {
		t.Fatalf("wrapped model = %q, want cline-pass/qwen3.7-max", got)
	}
	if got := parseOpenAIStreamModel([]byte(`data: {"model":"gpt-5.4-mini-2026-03-17","usage":{"total_tokens":1}}`)); got != "gpt-5.4-mini-2026-03-17" {
		t.Fatalf("stream model = %q, want gpt-5.4-mini-2026-03-17", got)
	}
}

func TestUsageReporterSpillsLargeStreamingOutputToTempFile(t *testing.T) {
	setUsageBodyCaptureForTest(t, true)
	reporter := newUsageReporter(context.Background(), "provider", "model", "", nil)
	chunk := bytes.Repeat([]byte("x"), usageReporterOutputMemoryLimit/2)

	reporter.appendOutputChunk(chunk)
	reporter.appendOutputChunk(chunk)

	if reporter.outputPath == "" {
		t.Fatalf("expected outputPath to be set after spilling to temp file")
	}
	tempPath := reporter.outputPath

	_, output, _, outputPath := reporter.finalizeContent()
	if output != "" {
		t.Fatalf("expected large output to remain file-backed, got inline length=%d", len(output))
	}
	if outputPath != tempPath {
		t.Fatalf("output path = %q, want %q", outputPath, tempPath)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read deferred output: %v", err)
	}
	expected := string(chunk) + "\n" + string(chunk) + "\n"
	if string(data) != expected {
		t.Fatalf("unexpected output length/content: got=%d want=%d", len(data), len(expected))
	}
	if err := os.Remove(outputPath); err != nil {
		t.Fatalf("remove deferred output: %v", err)
	}
}

func TestUsageReporterPublishesContextCanceledFailure(t *testing.T) {
	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 8)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	reporter := newUsageReporter(context.Background(), "gemini", "gemini-cancel-log-test", "gemini-cancel-log-test", nil)
	err := context.Canceled
	reporter.trackFailure(context.Background(), &err)

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-usagePlugin.records:
			if record.Provider != "gemini" || record.Model != "gemini-cancel-log-test" {
				continue
			}
			if !record.Failed {
				t.Fatal("context canceled usage record should be marked failed")
			}
			return
		case <-timer.C:
			t.Fatal("timed out waiting for context canceled usage record")
		}
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

func TestRequestDetailsIncludeModerationSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"not persisted"}`))
	ginCtx.Request = req
	ctx := context.WithValue(req.Context(), util.ContextKeyGin, ginCtx)
	highestScore := 0.91
	contentmoderation.SetRuntimeSnapshot(ctx, contentmoderation.RuntimeSnapshot{
		Evaluated:        true,
		ProfileID:        "profile-1",
		ProfileName:      "Strict prompts",
		ProfileVersion:   3,
		ResolutionSource: contentmoderation.ChannelTypeProviderKey,
		ChannelType:      contentmoderation.ChannelTypeProviderKey,
		ChannelID:        "provider-key-1",
		Action:           contentmoderation.ActionAPIBlock,
		WouldBlock:       true,
		HighestCategory:  "violence",
		HighestScore:     &highestScore,
		LatencyMS:        18,
		CacheHit:         false,
	})

	var detail struct {
		Moderation contentmoderation.RuntimeSnapshot `json:"moderation"`
	}
	if err := json.Unmarshal([]byte(buildRequestDetailContent(ctx, false)), &detail); err != nil {
		t.Fatalf("unmarshal request details: %v", err)
	}
	if detail.Moderation.ProfileID != "profile-1" || detail.Moderation.ChannelID != "provider-key-1" || detail.Moderation.Action != contentmoderation.ActionAPIBlock {
		t.Fatalf("moderation detail = %#v", detail.Moderation)
	}
	if detail.Moderation.HighestScore == nil || *detail.Moderation.HighestScore != highestScore {
		t.Fatalf("moderation highest score = %#v", detail.Moderation.HighestScore)
	}
}

func TestRequestDetailsRedactSensitiveHeadersAndOmitEmptyExchangeSections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?api_key=query-secret&safe=value", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer downstream-secret")
	req.Header.Set("Proxy-Authorization", "Basic proxy-secret")
	req.Header.Set("Cookie", "session=browser-secret")
	req.Header.Set("X-Api-Key", "api-key-secret")
	req.Header.Set("X-Auth-Token", "token-secret")
	req.Header.Set("X-Codex-Session-Id", "session-diagnostic")
	req.Header.Set("User-Agent", "codex-cli-test")
	ginCtx.Request = req
	ctx := context.WithValue(req.Context(), util.ContextKeyGin, ginCtx)

	var detail struct {
		Client struct {
			URL                string              `json:"url"`
			Query              map[string][]string `json:"query"`
			Headers            map[string][]string `json:"headers"`
			FingerprintHeaders map[string][]string `json:"fingerprint_headers"`
		} `json:"client"`
		Upstream *struct {
			RequestLog string `json:"request_log"`
		} `json:"upstream"`
		Response *struct {
			UpstreamLog string `json:"upstream_log"`
		} `json:"response"`
	}
	raw := buildRequestDetailContent(ctx, false)
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("unmarshal request details: %v", err)
	}
	for _, key := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Auth-Token"} {
		values := detail.Client.Headers[key]
		if len(values) != 1 || values[0] != redactedRequestDetailHeaderValue {
			t.Fatalf("client.headers[%q] = %#v, want redacted", key, values)
		}
	}
	for _, key := range []string{"X-Api-Key", "X-Auth-Token"} {
		values := detail.Client.FingerprintHeaders[key]
		if len(values) != 1 || values[0] != redactedRequestDetailHeaderValue {
			t.Fatalf("client.fingerprint_headers[%q] = %#v, want redacted", key, values)
		}
	}
	if got := detail.Client.Headers["X-Codex-Session-Id"]; len(got) != 1 || got[0] != "session-diagnostic" {
		t.Fatalf("non-sensitive diagnostic header = %#v, want preserved", got)
	}
	if got := detail.Client.Headers["User-Agent"]; len(got) != 1 || got[0] != "codex-cli-test" {
		t.Fatalf("user-agent = %#v, want preserved", got)
	}
	if strings.Contains(detail.Client.URL, "query-secret") || strings.Contains(strings.Join(detail.Client.Query["api_key"], ""), "query-secret") {
		t.Fatalf("request detail retained sensitive query value: url=%q query=%#v", detail.Client.URL, detail.Client.Query)
	}
	if got := detail.Client.Query["safe"]; len(got) != 1 || got[0] != "value" {
		t.Fatalf("safe query value = %#v, want preserved", got)
	}
	for _, secret := range []string{"downstream-secret", "proxy-secret", "browser-secret", "api-key-secret", "token-secret", "query-secret"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("request detail retained sensitive value %q", secret)
		}
	}
	if detail.Upstream != nil || detail.Response != nil {
		t.Fatalf("empty exchange sections should be omitted: upstream=%#v response=%#v", detail.Upstream, detail.Response)
	}
}

func TestRequestDetailsPreferForwardedClientIPForCDNRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, engine := gin.CreateTestContext(httptest.NewRecorder())
	if err := engine.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies(nil): %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hi"}`))
	req.RemoteAddr = "198.51.100.10:45678"
	req.Header.Set("X-Forwarded-For", "203.0.113.24, 198.51.100.10")
	ginCtx.Request = req
	ctx := context.WithValue(req.Context(), util.ContextKeyGin, ginCtx)

	var detail struct {
		Client struct {
			IP         string `json:"ip"`
			RemoteAddr string `json:"remote_addr"`
		} `json:"client"`
	}
	if err := json.Unmarshal([]byte(buildRequestDetailContent(ctx)), &detail); err != nil {
		t.Fatalf("unmarshal request details: %v", err)
	}
	if detail.Client.IP != "203.0.113.24" {
		t.Fatalf("client.ip = %q, want real forwarded client IP", detail.Client.IP)
	}
	if detail.Client.RemoteAddr != "198.51.100.10:45678" {
		t.Fatalf("client.remote_addr = %q, want CDN connection address preserved", detail.Client.RemoteAddr)
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

func TestUsageReporterPreservesDirectContentBeforeSpilledChunks(t *testing.T) {
	setUsageBodyCaptureForTest(t, true)
	reporter := newUsageReporter(context.Background(), "provider", "model", "", nil)
	chunk := bytes.Repeat([]byte("x"), usageReporterOutputMemoryLimit+1)
	reporter.appendOutputChunk(chunk)
	reporter.outputContent = "prefix\n"

	_, inlineOutput, _, outputPath := reporter.finalizeContent()
	if inlineOutput != "" || outputPath == "" {
		t.Fatalf("inline=%d path=%q, want file-backed output", len(inlineOutput), outputPath)
	}
	t.Cleanup(func() { _ = os.Remove(outputPath) })
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("prefix\n")) {
		t.Fatalf("combined output does not preserve prefix ordering")
	}
	if !bytes.Contains(data, chunk[:1024]) {
		t.Fatalf("combined output lost streamed chunk content")
	}
}

func TestUsageReporterDefersLargeInputContent(t *testing.T) {
	setUsageBodyCaptureForTest(t, true)
	reporter := newUsageReporter(context.Background(), "provider", "model", "", nil)
	input := strings.Repeat("i", usageReporterOutputMemoryLimit+1)
	reporter.setInputContent(input)

	inlineInput, _, inputPath, _ := reporter.finalizeContent()
	if inlineInput != "" || inputPath == "" {
		t.Fatalf("inline=%d path=%q, want file-backed input", len(inlineInput), inputPath)
	}
	t.Cleanup(func() { _ = os.Remove(inputPath) })
	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != input {
		t.Fatalf("deferred input content mismatch")
	}
}

func TestUsageReporterPreservesStreamingClassificationWithoutBodyCapture(t *testing.T) {
	setUsageBodyCaptureForTest(t, false)
	reporter := newUsageReporter(context.Background(), "provider", "model", "", nil)
	reporter.setInputContent(`{"model":"gpt-5.4","stream":true}`)

	if !reporter.streamingRequest {
		t.Fatal("streaming request classification should survive disabled body storage")
	}
	if reporter.inputContent != "" || reporter.inputPath != "" {
		t.Fatal("request body must not be retained when body storage is disabled")
	}
}

func TestUsageRequestMetadataSnapshotsRequestWithoutRetainingGinContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Status(http.StatusTooManyRequests)
	ctx := context.WithValue(ginCtx.Request.Context(), util.ContextKeyGin, ginCtx)

	identifier, _, status := usageRequestMetadata(ctx)
	if identifier != "POST /v1/responses" {
		t.Fatalf("identifier = %q", identifier)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", status, http.StatusTooManyRequests)
	}
}
