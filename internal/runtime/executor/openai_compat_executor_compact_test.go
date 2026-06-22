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
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorStreamAddsKimiReasoningForAssistantToolCalls(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("opencode-go", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"kimi-k2.6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"Bash:3","name":"Bash","input":{"cmd":"pwd"}},
				{"type":"tool_use","id":"Read:2","name":"Read","input":{"file_path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"Bash:3","content":"cwd"},
				{"type":"tool_result","tool_use_id":"Read:2","content":"readme"}
			]}
		],
		"tools":[
			{"name":"Bash","description":"Run command","input_schema":{"type":"object"}},
			{"name":"Read","description":"Read file","input_schema":{"type":"object"}}
		]
	}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "kimi-k2.6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}

	reasoning := gjson.GetBytes(gotBody, "messages.1.reasoning_content")
	if !reasoning.Exists() {
		t.Fatalf("messages.1.reasoning_content should exist in upstream body: %s", string(gotBody))
	}
	if reasoning.String() == "" {
		t.Fatalf("messages.1.reasoning_content should not be empty")
	}
}

func TestOpenAICompatExecutorNormalizesResponsesParallelToolCalls(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"deepseek-v4-flash",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"run checks"}]},
			{"type":"function_call","call_id":"call_a","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call","call_id":"call_b","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_a","output":"ok-a"},
			{"type":"function_call_output","call_id":"call_b","output":"ok-b"},
			{"role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/chat/completions")
	}
	messages := gjson.GetBytes(gotBody, "messages").Array()
	if len(messages) != 5 {
		t.Fatalf("messages len = %d, want 5: %s", len(messages), gotBody)
	}
	calls := messages[1].Get("tool_calls").Array()
	if len(calls) != 2 {
		t.Fatalf("assistant tool_calls len = %d, want 2: %s", len(calls), gotBody)
	}
	if got := messages[2].Get("tool_call_id").String(); got != "call_a" {
		t.Fatalf("messages[2].tool_call_id = %q, want call_a: %s", got, gotBody)
	}
	if got := messages[3].Get("tool_call_id").String(); got != "call_b" {
		t.Fatalf("messages[3].tool_call_id = %q, want call_b: %s", got, gotBody)
	}
}

func TestOpenAICompatExecutorCompactPassthrough(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"gpt-5.1-codex-max","input":[{"role":"user","content":"hi"}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.1-codex-max",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses/compact" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/responses/compact")
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("expected input in body")
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("unexpected messages in body")
	}
	if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorUsagePreservesRequestModelOverUpstreamEcho(t *testing.T) {
	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 4)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	// Upstream echoes a provider-internal model path (like Fireworks does for
	// glm-5.2 -> "accounts/fireworks/models/glm-5p2"). The usage record must keep
	// the clean request-time model, not the upstream echo, so display/cost/filter
	// keep working. This guards against re-introducing the setModel override.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"accounts/fireworks/models/glm-5p2","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: []byte(`{"model":"glm-5.2","input":"ping"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	timer := time.After(time.Second)
	for {
		select {
		case record := <-usagePlugin.records:
			if record.Model == "accounts/fireworks/models/glm-5p2" {
				t.Fatalf("usage record model = upstream echo %q; must stay the clean request model", record.Model)
			}
			if record.Model == "glm-5.2" {
				return
			}
		case <-timer:
			t.Fatal("timed out waiting for usage record with clean model glm-5.2")
		}
	}
}

func TestOpenAICompatExecutorStreamUsagePreservesRequestModelOverUpstreamEcho(t *testing.T) {
	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 4)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"chatcmpl-echo","object":"chat.completion.chunk","created":1,"model":"accounts/fireworks/models/glm-5p2","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-echo","object":"chat.completion.chunk","created":1,"model":"accounts/fireworks/models/glm-5p2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
			`data: [DONE]`,
			``,
		}, "\n\n")))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"glm-5.2","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	timer := time.After(time.Second)
	for {
		select {
		case record := <-usagePlugin.records:
			if record.Model == "accounts/fireworks/models/glm-5p2" {
				t.Fatalf("stream usage record model = upstream echo %q; must stay the clean request model", record.Model)
			}
			if record.Model == "glm-5.2" {
				return
			}
		case <-timer:
			t.Fatal("timed out waiting for stream usage record with clean model glm-5.2")
		}
	}
}

func TestOpenAICompatExecutorClaudeStreamSkipsEmptyToolNameEndToEnd(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotPath = r.URL.Path
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"chatcmpl-empty-tool","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_empty","type":"function","function":{"name":"","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-empty-tool","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: {"id":"chatcmpl-empty-tool","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
			`data: [DONE]`,
			``,
		}, "\n\n")))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"claude-3-opus",
		"max_tokens":1024,
		"stream":true,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_empty","name":"","input":{"path":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_empty","content":"orphan result"}]}
		]
	}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "m",
		Payload: payload,
	}, cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("claude"),
		Stream:          true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	messages := gjson.GetBytes(gotBody, "messages").Array()
	for _, msg := range messages {
		if msg.Get("tool_calls").Exists() {
			t.Fatalf("upstream OpenAI request must not include empty-name tool_calls: %s", gotBody)
		}
		if msg.Get("role").String() == "tool" {
			t.Fatalf("upstream OpenAI request must not include orphan tool result: %s", gotBody)
		}
	}

	var response strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		response.Write(chunk.Payload)
	}
	out := response.String()
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("Claude stream must not include tool_use for empty OpenAI function.name:\n%s", out)
	}
	if strings.Contains(out, `"type":"input_json_delta"`) {
		t.Fatalf("Claude stream must not include input_json_delta for empty OpenAI function.name:\n%s", out)
	}
	if strings.Contains(out, `"type":"content_block_stop"`) {
		t.Fatalf("Claude stream must not include orphan content_block_stop for empty OpenAI function.name:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("empty-only OpenAI tool_calls finish must map to Claude end_turn:\n%s", out)
	}
}

func TestOpenAICompatExecutorClaudeStreamSkipsMissingToolNameArgumentsEndToEnd(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"chatcmpl-provider-missing-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_missing","type":"function","function":{"arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-provider-missing-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"   ","arguments":"{\"offset\":10}"}}]},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-provider-missing-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: {"id":"chatcmpl-provider-missing-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}`,
			`data: [DONE]`,
			``,
		}, "\n\n")))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"claude-3-opus",
		"max_tokens":1024,
		"stream":true,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_missing","name":"","input":{"path":"a.go"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_missing","content":"orphan result"}]}
		]
	}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "m",
		Payload: payload,
	}, cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("claude"),
		Stream:          true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	messages := gjson.GetBytes(gotBody, "messages").Array()
	for _, msg := range messages {
		if msg.Get("tool_calls").Exists() {
			t.Fatalf("upstream OpenAI request must not include missing-name tool_calls: %s", gotBody)
		}
		if msg.Get("role").String() == "tool" {
			t.Fatalf("upstream OpenAI request must not include orphan tool result: %s", gotBody)
		}
	}

	var response strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		response.Write(chunk.Payload)
	}
	out := response.String()
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("Claude stream must not include tool_use when no valid OpenAI function.name arrives:\n%s", out)
	}
	if strings.Contains(out, `"type":"input_json_delta"`) {
		t.Fatalf("Claude stream must not include input_json_delta when no valid OpenAI function.name arrives:\n%s", out)
	}
	if strings.Contains(out, `"type":"content_block_stop"`) {
		t.Fatalf("Claude stream must not include orphan content_block_stop when no valid OpenAI function.name arrives:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("missing-name OpenAI tool_calls finish must map to Claude end_turn:\n%s", out)
	}
}

func TestOpenAICompatExecutorClaudeStreamPreservesValidParallelToolWhenEmptyIndexFirstEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"chatcmpl-provider-parallel","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_empty","type":"function","function":{"arguments":"{\"path\":\"x\"}"}},{"index":1,"id":"call_valid","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"a.go\""}}]},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-provider-parallel","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":""}},{"index":1,"function":{"arguments":",\"offset\":10}"}}]},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-provider-parallel","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
			``,
		}, "\n\n")))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"claude-3-opus",
		"max_tokens":1024,
		"stream":true,
		"messages":[{"role":"user","content":[{"type":"text","text":"read file"}]}]
	}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "m",
		Payload: payload,
	}, cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("claude"),
		Stream:          true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var response strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		response.Write(chunk.Payload)
	}
	out := response.String()
	if got := strings.Count(out, `"type":"tool_use"`); got != 1 {
		t.Fatalf("expected exactly one valid Claude tool_use, got %d:\n%s", got, out)
	}
	if strings.Contains(out, `call_empty`) {
		t.Fatalf("Claude stream must not include empty first-index tool call:\n%s", out)
	}
	if !strings.Contains(out, `"name":"Read"`) {
		t.Fatalf("Claude stream should preserve valid parallel tool name:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("valid parallel tool should keep stop_reason=tool_use:\n%s", out)
	}
}

func TestOpenAICompatExecutorClaudeNonStreamSkipsEmptyToolNameEndToEnd(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-empty-tool","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_empty","type":"function","function":{"name":"","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"claude-3-opus",
		"max_tokens":1024,
		"stream":false,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_empty","name":"   ","input":{"path":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_empty","content":"orphan result"}]}
		]
	}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "m",
		Payload: payload,
	}, cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("claude"),
		Stream:          false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	messages := gjson.GetBytes(gotBody, "messages").Array()
	for _, msg := range messages {
		if msg.Get("tool_calls").Exists() {
			t.Fatalf("upstream OpenAI request must not include whitespace-name tool_calls: %s", gotBody)
		}
		if msg.Get("role").String() == "tool" {
			t.Fatalf("upstream OpenAI request must not include orphan tool result: %s", gotBody)
		}
	}

	out := string(resp.Payload)
	if strings.Contains(out, `"type":"tool_use"`) || strings.Contains(out, `"name":""`) {
		t.Fatalf("Claude non-stream response must not include tool_use for empty OpenAI function.name:\n%s", out)
	}
	if got := gjson.Get(out, "stop_reason").String(); got != "end_turn" {
		t.Fatalf("Claude non-stream stop_reason = %q, want end_turn: %s", got, out)
	}
}
