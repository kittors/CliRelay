package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecutePreservesResponsesImageBridgeModel(t *testing.T) {
	var lastBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		lastBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1710000002,\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
		Metadata: map[string]any{
			"access_token": "token",
			"account_id":   "account-1",
		},
	}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-image-2",
		Payload: []byte(`{"model":"gpt-image-2","input":"draw a fox","size":"1024x1024"}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.Get(lastBody, "model").String(); got != "gpt-5.4-mini" {
		t.Fatalf("top-level model = %q, want %q; body=%s", got, "gpt-5.4-mini", lastBody)
	}
	if got := gjson.Get(lastBody, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want %q; body=%s", got, "image_generation", lastBody)
	}
	if got := gjson.Get(lastBody, "tools.0.model").String(); got != "gpt-image-2" {
		t.Fatalf("tools.0.model = %q, want %q; body=%s", got, "gpt-image-2", lastBody)
	}
}

func TestCodexExecutorExecuteStreamPreservesResponsesImageBridgeModel(t *testing.T) {
	var lastBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		lastBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1710000002,\"status\":\"in_progress\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1710000002,\"status\":\"completed\",\"output\":[]}}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-stream",
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
		Metadata: map[string]any{
			"access_token": "token",
			"account_id":   "account-1",
		},
	}

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-image-2",
		Payload: []byte(`{"model":"gpt-image-2","input":"draw a fox","stream":true,"quality":"low"}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	for range stream.Chunks {
	}

	if got := gjson.Get(lastBody, "model").String(); got != "gpt-5.4-mini" {
		t.Fatalf("top-level model = %q, want %q; body=%s", got, "gpt-5.4-mini", lastBody)
	}
	if got := gjson.Get(lastBody, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want %q; body=%s", got, "image_generation", lastBody)
	}
	if got := gjson.Get(lastBody, "tools.0.model").String(); got != "gpt-image-2" {
		t.Fatalf("tools.0.model = %q, want %q; body=%s", got, "gpt-image-2", lastBody)
	}
}

func TestCodexExecutorExecuteMergesResponsesImageOutputFromPriorSSEItem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1710000001,\"status\":\"in_progress\"}}\n\n" +
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"ZmFrZS1pbWFnZQ==\",\"revised_prompt\":\"cute fox icon\",\"output_format\":\"png\",\"quality\":\"low\",\"size\":\"1024x1024\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1710000002,\"status\":\"completed\",\"output\":[]}}\n\n",
		))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-image-output",
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
		Metadata: map[string]any{
			"access_token": "token",
			"account_id":   "account-1",
		},
	}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-image-2",
		Payload: []byte(`{"model":"gpt-image-2","input":"draw a fox","size":"1024x1024"}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "output.0.type").String(); got != "image_generation_call" {
		t.Fatalf("output.0.type = %q, want %q; payload=%s", got, "image_generation_call", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.result").String(); got != "ZmFrZS1pbWFnZQ==" {
		t.Fatalf("output.0.result = %q, want %q; payload=%s", got, "ZmFrZS1pbWFnZQ==", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.revised_prompt").String(); got != "cute fox icon" {
		t.Fatalf("output.0.revised_prompt = %q, want %q; payload=%s", got, "cute fox icon", resp.Payload)
	}
}

func TestCodexExecutorExecuteStripsUnsupportedTokenLimitFields(t *testing.T) {
	var lastBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		lastBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1710000002,\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-token-limits",
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
		Metadata: map[string]any{
			"access_token": "token",
			"account_id":   "account-1",
		},
	}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"max_output_tokens":1024,"max_completion_tokens":2048,"max_tokens":4096}`),
		Format:  sdktranslator.FromString("codex"),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("codex"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if gjson.Get(lastBody, field).Exists() {
			t.Fatalf("%s should be stripped before codex upstream call; body=%s", field, lastBody)
		}
	}
}
