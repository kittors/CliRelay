package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
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
		Payload: []byte(`{"model":"gpt-image-2","input":"draw a fox","size":"4096x2304"}`),
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
	if gjson.Get(lastBody, "tools.0.size").Exists() {
		t.Fatalf("tools.0.size should be stripped before Codex upstream call; body=%s", lastBody)
	}
	if got := gjson.Get(lastBody, "input.0.content.0.text").String(); got != "draw a fox\n\nPreferred image size: 4096x2304." {
		t.Fatalf("input text = %q, want prompt with size hint; body=%s", got, lastBody)
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

	var streamBody string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamBody += string(chunk.Payload) + "\n"
	}
	if !strings.Contains(streamBody, `data: {"type":"response.completed"`) {
		t.Fatalf("stream body = %q, want response.completed data event", streamBody)
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

func TestCodexExecutorExecuteStreamPublishesUsageForResponseDone(t *testing.T) {
	const model = "gpt-response-done-log-test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"data: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_1\",\"model\":\"" + model + "\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer server.Close()

	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 8)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-stream-response-done",
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
		Model:   model,
		Payload: []byte(`{"model":"` + model + `","input":"hi","stream":true}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-usagePlugin.records:
			if record.Provider != "codex" || record.Model != model {
				continue
			}
			if record.Failed {
				t.Fatal("response.done stream with usage should not be marked failed")
			}
			if !record.Streaming {
				t.Fatal("response.done usage record should be marked streaming")
			}
			if record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 || record.Detail.TotalTokens != 5 {
				t.Fatalf("usage = %d/%d/%d, want 3/2/5", record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens)
			}
			return
		case <-timer.C:
			t.Fatal("timed out waiting for Codex response.done usage record")
		}
	}
}

func TestCodexExecutorExecutePublishesUsageRecordWithoutUsage(t *testing.T) {
	const model = "gpt-codex-no-usage-log-test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"" + model + "\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 8)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-no-usage",
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
		Model:   model,
		Payload: []byte(`{"model":"` + model + `","input":"hi"}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-usagePlugin.records:
			if record.Provider != "codex" || record.Model != model {
				continue
			}
			if record.Failed {
				t.Fatal("completed response without usage should not be marked failed")
			}
			if record.Streaming {
				t.Fatal("non-stream response should not be marked streaming")
			}
			return
		case <-timer.C:
			t.Fatal("timed out waiting for Codex no-usage success record")
		}
	}
}

func TestCodexExecutorExecuteStreamReturnsResponsesFailedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"created_at\":1710000002,\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"Rate limit reached\"}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-stream-response-failed",
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
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var streamBody string
	var chunkErr error
	for chunk := range stream.Chunks {
		streamBody += string(chunk.Payload)
		if chunk.Err != nil {
			chunkErr = chunk.Err
		}
	}
	if !strings.Contains(streamBody, "response.failed") {
		t.Fatalf("stream body = %q, want response.failed forwarded", streamBody)
	}
	if chunkErr == nil {
		t.Fatal("stream error = nil, want response.failed error")
	}
	if strings.Contains(chunkErr.Error(), "response.completed") {
		t.Fatalf("stream error = %v, should not report incomplete stream", chunkErr)
	}
	if !strings.Contains(chunkErr.Error(), "Rate limit reached") {
		t.Fatalf("stream error = %v, want upstream message", chunkErr)
	}
	statusErr, ok := chunkErr.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %v/%v, want %d", statusErr, ok, http.StatusTooManyRequests)
	}
}

func TestCodexExecutorExecuteStreamReturnsIncompleteResponsesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-stream-incomplete",
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
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var chunkErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			chunkErr = chunk.Err
		}
	}
	if chunkErr == nil {
		t.Fatal("stream error = nil, want incomplete stream error")
	}
	authErr, ok := chunkErr.(*cliproxyauth.Error)
	if !ok {
		t.Fatalf("stream error type = %T, want *cliproxyauth.Error", chunkErr)
	}
	if authErr.Code != "response_stream_incomplete" {
		t.Fatalf("stream error code = %q, want response_stream_incomplete", authErr.Code)
	}
	if authErr.StatusCode() != http.StatusBadGateway {
		t.Fatalf("StatusCode() = %d, want %d", authErr.StatusCode(), http.StatusBadGateway)
	}
}

func TestCodexExecutorExecuteReturnsResponsesFailedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"created_at\":1710000002,\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"Rate limit reached\"}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-response-failed",
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
		Payload: []byte(`{"model":"gpt-5.4","input":"hi"}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want response.failed error")
	}
	if strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("Execute() error = %v, should not report incomplete stream", err)
	}
	if !strings.Contains(err.Error(), "Rate limit reached") {
		t.Fatalf("Execute() error = %v, want upstream message", err)
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %v/%v, want %d", statusErr, ok, http.StatusTooManyRequests)
	}
}

func TestCodexExecutorExecuteReturnsTopLevelResponsesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"error\",\"code\":\"internal_server_error\",\"message\":\"upstream exploded\"}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-top-level-error",
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
		Payload: []byte(`{"model":"gpt-5.4","input":"hi"}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want top-level error")
	}
	if strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("Execute() error = %v, should not report incomplete stream", err)
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("Execute() error = %v, want upstream message", err)
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
