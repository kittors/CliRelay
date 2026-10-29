package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestExecutionContextUsesOriginalRequestAndRequestedModel(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "gpt-5",
		Payload: []byte(`{"messages":[{"role":"user","content":"translated"}]}`),
	}
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"original"}]}`),
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "user-facing-model",
		},
	}

	execCtx := newExecutionContext(
		context.Background(),
		"openai-compatibility",
		&config.Config{},
		nil,
		req,
		opts,
		ExecutionOptions{TargetFormat: sdktranslator.FromString("openai")},
	)

	if execCtx.BaseModel != "gpt-5" {
		t.Fatalf("BaseModel = %q, want %q", execCtx.BaseModel, "gpt-5")
	}
	if execCtx.RequestedModel != "user-facing-model" {
		t.Fatalf("RequestedModel = %q, want %q", execCtx.RequestedModel, "user-facing-model")
	}
	if got := string(execCtx.OriginalPayload); got != string(opts.OriginalRequest) {
		t.Fatalf("OriginalPayload = %q, want %q", got, string(opts.OriginalRequest))
	}

	translated, originalTranslated := execCtx.TranslateRequestPair(nil)
	if got := gjson.GetBytes(translated, "messages.0.content").String(); got != "translated" {
		t.Fatalf("translated content = %q, want %q", got, "translated")
	}
	if got := gjson.GetBytes(originalTranslated, "messages.0.content").String(); got != "original" {
		t.Fatalf("original translated content = %q, want %q", got, "original")
	}
}

func TestExecutionContextReporterCapturesThinkingLevel(t *testing.T) {
	tests := []struct {
		name           string
		requestedModel string
		payload        []byte
		sourceFormat   sdktranslator.Format
		wantModel      string
		wantLevel      string
	}{
		{name: "body effort", requestedModel: "gpt-5.6-sol", payload: []byte(`{"reasoning":{"effort":"max"}}`), sourceFormat: sdktranslator.FormatOpenAIResponse, wantModel: "gpt-5.6-sol", wantLevel: "max"},
		{name: "body effort trimmed", requestedModel: "gpt-5.6-sol", payload: []byte(`{"reasoning":{"effort":" high "}}`), sourceFormat: sdktranslator.FormatOpenAIResponse, wantModel: "gpt-5.6-sol", wantLevel: "high"},
		{name: "chat completions effort", requestedModel: "gpt-5.5", payload: []byte(`{"reasoning_effort":"medium"}`), sourceFormat: sdktranslator.FormatOpenAI, wantModel: "gpt-5.5", wantLevel: "medium"},
		{name: "claude budget", requestedModel: "claude-sonnet", payload: []byte(`{"thinking":{"type":"enabled","budget_tokens":4096}}`), sourceFormat: sdktranslator.FormatClaude, wantModel: "claude-sonnet", wantLevel: "4096"},
		{name: "claude adaptive effort", requestedModel: "claude-opus-5-5", payload: []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`), sourceFormat: sdktranslator.FormatClaude, wantModel: "claude-opus-5-5", wantLevel: "xhigh"},
		{name: "suffix takes priority", requestedModel: "gpt-5.6-sol(high)", payload: []byte(`{"reasoning":{"effort":"max"}}`), sourceFormat: sdktranslator.FormatOpenAIResponse, wantModel: "gpt-5.6-sol", wantLevel: "high"},
		{name: "numeric budget", requestedModel: "gpt-5.6-sol(8192)", wantModel: "gpt-5.6-sol", wantLevel: "8192"},
		{name: "no suffix or body effort", requestedModel: "gpt-5.6-sol", wantModel: "gpt-5.6-sol", wantLevel: ""},
		{name: "context marker only", requestedModel: "gpt-5.6-sol[128K]", wantModel: "gpt-5.6-sol", wantLevel: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCtx := newExecutionContext(
				context.Background(),
				"codex",
				&config.Config{},
				nil,
				cliproxyexecutor.Request{Model: tt.requestedModel, Payload: tt.payload},
				cliproxyexecutor.Options{SourceFormat: tt.sourceFormat},
				ExecutionOptions{},
			)

			reporter := execCtx.Reporter()
			if reporter.model != tt.wantModel {
				t.Fatalf("reporter.model = %q, want %q", reporter.model, tt.wantModel)
			}
			if reporter.thinkingLevel != tt.wantLevel {
				t.Fatalf("reporter.thinkingLevel = %q, want %q", reporter.thinkingLevel, tt.wantLevel)
			}
		})
	}
}

func TestExecutionContextTranslatesSharedRequestPayloadOnce(t *testing.T) {
	var calls atomic.Int32
	from := sdktranslator.FromString("execution-context-shared-source")
	to := sdktranslator.FromString("execution-context-shared-target")
	sdktranslator.Register(from, to, func(_ string, rawJSON []byte, _ bool) []byte {
		calls.Add(1)
		return bytes.Clone(rawJSON)
	}, sdktranslator.ResponseTransform{})

	req := cliproxyexecutor.Request{
		Model:   "test-model",
		Payload: []byte(`{"input":"shared"}`),
	}
	execCtx := newExecutionContext(
		context.Background(),
		"test-provider",
		&config.Config{},
		nil,
		req,
		cliproxyexecutor.Options{SourceFormat: from},
		ExecutionOptions{TargetFormat: to},
	)

	translated, originalTranslated := execCtx.TranslateRequestPair(nil)
	if got := calls.Load(); got != 1 {
		t.Fatalf("translator calls = %d, want 1 for shared request payload", got)
	}
	if string(translated) != string(req.Payload) || string(originalTranslated) != string(req.Payload) {
		t.Fatalf("translated payloads differ: translated=%q original=%q", translated, originalTranslated)
	}
	if len(translated) > 0 && &translated[0] != &originalTranslated[0] {
		t.Fatal("shared request payload should reuse the single translated result")
	}
}

func TestExecutionContextTranslatesSharedNonEmptyOriginalRequestOnce(t *testing.T) {
	// Production handlers set OriginalRequest to the same slice as Payload (non-empty).
	var calls atomic.Int32
	from := sdktranslator.FromString("execution-context-shared-nonempty-source")
	to := sdktranslator.FromString("execution-context-shared-nonempty-target")
	sdktranslator.Register(from, to, func(_ string, rawJSON []byte, _ bool) []byte {
		calls.Add(1)
		return bytes.Clone(rawJSON)
	}, sdktranslator.ResponseTransform{})

	raw := []byte(`{"input":"production-shared"}`)
	req := cliproxyexecutor.Request{
		Model:   "test-model",
		Payload: raw,
	}
	execCtx := newExecutionContext(
		context.Background(),
		"test-provider",
		&config.Config{},
		nil,
		req,
		cliproxyexecutor.Options{
			OriginalRequest: raw,
			SourceFormat:    from,
		},
		ExecutionOptions{TargetFormat: to},
	)

	translated, originalTranslated := execCtx.TranslateRequestPair(nil)
	if got := calls.Load(); got != 1 {
		t.Fatalf("translator calls = %d, want 1 for non-empty shared OriginalRequest", got)
	}
	if string(translated) != string(raw) || string(originalTranslated) != string(raw) {
		t.Fatalf("translated payloads differ: translated=%q original=%q", translated, originalTranslated)
	}
	if len(translated) > 0 && &translated[0] != &originalTranslated[0] {
		t.Fatal("shared non-empty OriginalRequest should reuse the single translated result")
	}
}

func TestExecutionContextTranslatesDistinctOriginalRequestSeparately(t *testing.T) {
	var calls atomic.Int32
	from := sdktranslator.FromString("execution-context-distinct-source")
	to := sdktranslator.FromString("execution-context-distinct-target")
	sdktranslator.Register(from, to, func(_ string, rawJSON []byte, _ bool) []byte {
		calls.Add(1)
		return bytes.Clone(rawJSON)
	}, sdktranslator.ResponseTransform{})

	req := cliproxyexecutor.Request{
		Model:   "test-model",
		Payload: []byte(`{"input":"translated"}`),
	}
	execCtx := newExecutionContext(
		context.Background(),
		"test-provider",
		&config.Config{},
		nil,
		req,
		cliproxyexecutor.Options{
			OriginalRequest: []byte(`{"input":"original"}`),
			SourceFormat:    from,
		},
		ExecutionOptions{TargetFormat: to},
	)

	translated, originalTranslated := execCtx.TranslateRequestPair(nil)
	if got := calls.Load(); got != 2 {
		t.Fatalf("translator calls = %d, want 2 for distinct request payloads", got)
	}
	if string(translated) != string(req.Payload) {
		t.Fatalf("translated payload = %q, want %q", translated, req.Payload)
	}
	if string(originalTranslated) != string(execCtx.OriginalPayload) {
		t.Fatalf("original translated payload = %q, want %q", originalTranslated, execCtx.OriginalPayload)
	}
}

func BenchmarkExecutionContextTranslateSharedRequestPayload(b *testing.B) {
	from := sdktranslator.FromString("execution-context-benchmark-source")
	to := sdktranslator.FromString("execution-context-benchmark-target")
	sdktranslator.Register(from, to, func(_ string, rawJSON []byte, _ bool) []byte {
		return bytes.Clone(rawJSON)
	}, sdktranslator.ResponseTransform{})

	for _, size := range []int{512 << 10, 2 << 20, 8 << 20, 16 << 20} {
		b.Run(byteSizeLabel(size), func(b *testing.B) {
			payload := bytes.Repeat([]byte("x"), size)
			execCtx := newExecutionContext(
				context.Background(),
				"test-provider",
				&config.Config{},
				nil,
				cliproxyexecutor.Request{Model: "test-model", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: from},
				ExecutionOptions{TargetFormat: to},
			)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				translated, originalTranslated := execCtx.TranslateRequestPair(nil)
				runtime.KeepAlive(translated)
				runtime.KeepAlive(originalTranslated)
			}
		})
	}
}

func byteSizeLabel(size int) string {
	if size%(1<<20) == 0 {
		return strconv.Itoa(size/(1<<20)) + "MiB"
	}
	return strconv.Itoa(size/(1<<10)) + "KiB"
}

func TestExecutionContextReporterKeepsVisionFallbackSeparateFromModelMapping(t *testing.T) {
	ctx := contextWithVisionFallbackLog(context.Background(), "alias-model(max)", "real-model", "vision-model")
	req := cliproxyexecutor.Request{
		Model:   "vision-model",
		Payload: []byte(`{"messages":[]}`),
	}
	execCtx := newExecutionContext(
		ctx,
		"openai-compatibility",
		&config.Config{},
		nil,
		req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")},
		ExecutionOptions{TargetFormat: sdktranslator.FromString("openai")},
	)

	reporter := execCtx.Reporter()
	if reporter.model != "alias-model" {
		t.Fatalf("reporter.model = %q, want alias-model", reporter.model)
	}
	if reporter.thinkingLevel != "max" {
		t.Fatalf("reporter.thinkingLevel = %q, want max", reporter.thinkingLevel)
	}
	if reporter.upstreamModel != "real-model" {
		t.Fatalf("reporter.upstreamModel = %q, want real-model", reporter.upstreamModel)
	}
	if reporter.visionFallbackModel != "vision-model" {
		t.Fatalf("reporter.visionFallbackModel = %q, want vision-model", reporter.visionFallbackModel)
	}
}

func TestExecutionContextApplyPayloadConfigUsesProtocolOverride(t *testing.T) {
	cfg := &config.Config{
		Payload: config.PayloadConfig{
			Default: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{
						{Name: "gemini-2.5-pro", Protocol: "gemini"},
					},
					Params: map[string]any{
						"temperature": 0.1,
					},
				},
			},
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gemini-2.5-pro",
		Payload: []byte(`{"request":{}}`),
	}
	execCtx := newExecutionContext(
		context.Background(),
		"gemini-cli",
		cfg,
		nil,
		req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")},
		ExecutionOptions{
			TargetFormat:        sdktranslator.FromString("gemini-cli"),
			PayloadConfigRoot:   "request",
			PayloadConfigFormat: "gemini",
		},
	)

	out := execCtx.ApplyPayloadConfig(req.Payload, req.Payload)
	if got := gjson.GetBytes(out, "request.temperature").Float(); got != 0.1 {
		t.Fatalf("request.temperature = %v, want %v", got, 0.1)
	}
}

func TestUpstreamRecorderCapturesAuthMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)

	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Provider: "openai-compatibility",
		Label:    "Compat",
		Attributes: map[string]string{
			"api_key": "sk-test-key",
		},
	}

	upstream := newUpstreamRecorder(ctx, &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}, "openai-compatibility", auth)
	upstream.RecordRequest(
		"https://example.com/v1/chat/completions",
		http.MethodPost,
		http.Header{"Authorization": []string{"Bearer sk-test-key"}},
		[]byte(`{"ok":true}`),
	)

	raw, exists := ginCtx.Get(apiRequestKey)
	if !exists {
		t.Fatal("expected API request log to be captured")
	}
	requestLog, ok := raw.([]byte)
	if !ok {
		t.Fatalf("api request log type = %T, want []byte", raw)
	}
	logText := string(requestLog)
	for _, want := range []string{
		"provider=openai-compatibility",
		"auth_id=auth-1",
		"label=Compat",
		"type=api_key",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("request log missing %q in %s", want, logText)
		}
	}
}
