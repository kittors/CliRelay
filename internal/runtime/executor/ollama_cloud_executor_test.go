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

func TestOllamaCloudExecutorRoutesChatToOpenAICompatibleV1(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-oss:120b","created_at":"2026-07-07T00:00:00Z","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL + "/v1"}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-oss:120b",
		Payload: []byte(`{"model":"gpt-oss:120b","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("path = %q, want /api/chat", gotPath)
	}
	if got := gjson.GetBytes(gotBody, "keep_alive").String(); got != ollamaCloudNativeKeepAlive {
		t.Fatalf("keep_alive = %q, want %q; body=%s", got, ollamaCloudNativeKeepAlive, gotBody)
	}
	if gotText := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); gotText != "ok" {
		t.Fatalf("response text = %q, want ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOllamaCloudNativeCacheKeyScopesExplicitPromptKey(t *testing.T) {
	source := []byte(`{"model":"glm-5.2","prompt_cache_key":"shared-session","input":"hi"}`)
	authA := &cliproxyauth.Auth{ID: "ollama-cloud:one"}
	authB := &cliproxyauth.Auth{ID: "ollama-cloud:two"}

	keyA := ollamaNativeCacheKey(authA, "glm-5.2", source, nil, cliproxyexecutor.Options{})
	keyB := ollamaNativeCacheKey(authB, "glm-5.2", source, nil, cliproxyexecutor.Options{})

	if keyA == "" || keyB == "" {
		t.Fatalf("cache keys must be present: %q / %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("cache key not isolated by auth: %q", keyA)
	}
	if strings.Contains(keyA, "shared-session") || strings.Contains(keyB, "shared-session") {
		t.Fatalf("cache key leaked raw prompt key: %q / %q", keyA, keyB)
	}
}

func TestOllamaCloudExecutorRoutesResponsesToNativeChat(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-oss:120b","created_at":"2026-07-07T00:00:00Z","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-oss:120b",
		Payload: []byte(`{"model":"client-prefix/gpt-oss:120b","input":"hi"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("path = %q, want /api/chat", gotPath)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "gpt-oss:120b" {
		t.Fatalf("upstream model = %q, want gpt-oss:120b; body=%s", gotModel, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "prompt_cache_key").Exists() {
		t.Fatalf("native Ollama body must not forward OpenAI prompt_cache_key: %s", gotBody)
	}
	if gotText := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); gotText != "ok" {
		t.Fatalf("response text = %q, want ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOllamaCloudExecutorNativeResponsesReportsEstimatedCacheHit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"glm-5.2","created_at":"2026-07-07T00:00:00Z","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1000,"eval_count":1}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "ollama-cloud:apikey:one", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	longPrompt := strings.Repeat("cached-prefix ", 500)
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.SessionStickyMetadataKey: "header:x-session-id:session-1",
		},
	}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: []byte(`{"model":"glm-5.2","input":"` + longPrompt + `"}`),
	}, opts)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: []byte(`{"model":"glm-5.2","input":"` + longPrompt + ` plus one more turn"}`),
	}, opts)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("server calls = %d, want 2", calls)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.input_tokens_details.cached_tokens").Int(); got <= 0 {
		t.Fatalf("cached_tokens = %d, want positive cache estimate; payload=%s", got, resp.Payload)
	}
}

func TestOllamaCloudExecutorStreamsResponsesFromNativeChat(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"model":"gpt-oss:120b","message":{"role":"assistant","content":"ok"},"done":false}` + "\n"))
		_, _ = w.Write([]byte(`{"model":"gpt-oss:120b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}` + "\n"))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-oss:120b",
		Payload: []byte(`{"model":"gpt-oss:120b","input":"hi","stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	var chunks strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		chunks.Write(chunk.Payload)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("path = %q, want /api/chat", gotPath)
	}
	if got := chunks.String(); !strings.Contains(got, "response.completed") || !strings.Contains(got, "ok") {
		t.Fatalf("stream payload = %q, want responses stream with ok completion", got)
	}
}

func TestOllamaCloudExecutorClaudeMessagesAddsCacheControl(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"glm-5.2","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: []byte(`{"model":"glm-5.2","max_tokens":1024,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"again"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("cache_control = %q, want ephemeral; body=%s", got, gotBody)
	}
}

func TestOllamaCloudExecutorRoutesClaudeMessagesToOfficialEndpoint(t *testing.T) {
	var gotPath, gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVersion = r.Header.Get("Anthropic-Version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"gpt-oss:120b","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-oss:120b",
		Payload: []byte(`{"model":"gpt-oss:120b","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", gotPath)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q, want 2023-06-01", gotVersion)
	}
	if gotText := gjson.GetBytes(resp.Payload, "content.0.text").String(); gotText != "ok" {
		t.Fatalf("response text = %q, want ok; payload=%s", gotText, string(resp.Payload))
	}
}
