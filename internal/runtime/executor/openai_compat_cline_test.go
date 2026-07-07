package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorClineUsesVisionFallbackForImageRequests(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"cline-pass/mimo-v2.5-pro","choices":[{"index":0,"message":{"role":"assistant","content":"vision ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("cline", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":              server.URL + "/api/v1",
		"api_key":               "test-key",
		"vision_fallback_model": "cline-pass/mimo-v2.5-pro",
	}}
	payload := []byte(`{"model":"cline-pass/deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "cline-pass/deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/api/v1/chat/completions" {
		t.Fatalf("path = %q, want /api/v1/chat/completions", gotPath)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "cline-pass/mimo-v2.5-pro" {
		t.Fatalf("upstream model = %q, want cline-pass/mimo-v2.5-pro; body=%s", gotModel, string(gotBody))
	}
	if !strings.Contains(string(gotBody), `"image_url"`) {
		t.Fatalf("image should be preserved for vision fallback model; body=%s", string(gotBody))
	}
	if gotModel := gjson.GetBytes(resp.Payload, "model").String(); gotModel != "cline-pass/deepseek-v4-flash" {
		t.Fatalf("response model = %q, want cline-pass/deepseek-v4-flash; payload=%s", gotModel, string(resp.Payload))
	}
}

func TestOpenAICompatExecutorClineUnwrapsDataEnvelopeForClaudeMessages(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"chatcmpl_wrapped","object":"chat.completion","created":1,"model":"cline-pass/qwen3.7-max","choices":[{"index":0,"message":{"role":"assistant","content":"cline wrapped ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("cline", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/api/v1",
		"api_key":  "test-key",
	}}
	payload := []byte(`{"model":"qwen3.7-max","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "cline-pass/qwen3.7-max",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/api/v1/chat/completions" {
		t.Fatalf("path = %q, want /api/v1/chat/completions", gotPath)
	}
	if gotText := gjson.GetBytes(resp.Payload, "content.0.text").String(); gotText != "cline wrapped ok" {
		t.Fatalf("Claude message text = %q, want cline wrapped ok; payload=%s", gotText, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "usage.output_tokens").Int(); got != 5 {
		t.Fatalf("output tokens = %d, want 5; payload=%s", got, string(resp.Payload))
	}
}

func TestOpenAICompatExecutorClineUnwrapsDataEnvelopeForOpenAIChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"chatcmpl_wrapped","object":"chat.completion","created":1,"model":"cline-pass/qwen3.7-max","choices":[{"index":0,"message":{"role":"assistant","content":"cline chat ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("cline", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/api/v1",
		"api_key":  "test-key",
	}}
	payload := []byte(`{"model":"qwen3.7-max","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "cline-pass/qwen3.7-max",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gjson.GetBytes(resp.Payload, "success").Exists() || gjson.GetBytes(resp.Payload, "data").Exists() {
		t.Fatalf("OpenAI chat response was not unwrapped: %s", string(resp.Payload))
	}
	if gotText := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); gotText != "cline chat ok" {
		t.Fatalf("OpenAI chat text = %q, want cline chat ok; payload=%s", gotText, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "usage.completion_tokens").Int(); got != 5 {
		t.Fatalf("completion tokens = %d, want 5; payload=%s", got, string(resp.Payload))
	}
}

func TestOpenAICompatExecutorClineUnwrapsDataEnvelopeForOpenAIChatStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"success":true,"data":{"id":"chatcmpl_wrapped","object":"chat.completion.chunk","created":1,"model":"cline-pass/qwen3.7-max","choices":[{"index":0,"delta":{"role":"assistant","content":"OK"},"finish_reason":null}]}}`,
			`data: [DONE]`,
			``,
		}, "\n\n")))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("cline", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/api/v1",
		"api_key":  "test-key",
	}}
	payload := []byte(`{"model":"qwen3.7-max","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "cline-pass/qwen3.7-max",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}

	var out strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		out.Write(chunk.Payload)
	}
	got := out.String()
	if strings.Contains(got, `"success"`) || strings.Contains(got, `"data"`) {
		t.Fatalf("OpenAI stream response was not unwrapped: %s", got)
	}
	if !strings.Contains(got, `"content":"OK"`) {
		t.Fatalf("OpenAI stream response missing content: %s", got)
	}
}
