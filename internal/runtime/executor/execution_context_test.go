package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestExecutionContextReporterKeepsVisionFallbackSeparateFromModelMapping(t *testing.T) {
	ctx := contextWithVisionFallbackLog(context.Background(), "alias-model", "real-model", "vision-model")
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
