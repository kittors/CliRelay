package test

import (
	"context"
	"strings"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func collectOpenAIToClaudeStreamViaSDK(t *testing.T, chunks ...string) string {
	t.Helper()

	originalRequest := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)
	translatedRequest := []byte(`{"model":"m","stream":true}`)
	var param any
	var out []string
	for _, chunk := range chunks {
		out = append(out, sdktranslator.TranslateStream(
			context.Background(),
			sdktranslator.FormatOpenAI,
			sdktranslator.FormatClaude,
			"m",
			originalRequest,
			translatedRequest,
			[]byte(chunk),
			&param,
		)...)
	}
	return strings.Join(out, "")
}

func assertNoOrphanAnthropicContentEvents(t *testing.T, stream string) {
	t.Helper()

	openBlocks := make(map[int]bool)
	for _, segment := range strings.Split(stream, "\n\n") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}

		var eventType, data string
		for _, line := range strings.Split(segment, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if data == "" {
			continue
		}

		payload := gjson.Parse(data)
		switch eventType {
		case "content_block_start":
			openBlocks[int(payload.Get("index").Int())] = true
		case "content_block_delta":
			idx := int(payload.Get("index").Int())
			if !openBlocks[idx] {
				t.Fatalf("content_block_delta without content_block_start at index %d:\n%s", idx, stream)
			}
		case "content_block_stop":
			idx := int(payload.Get("index").Int())
			if !openBlocks[idx] {
				t.Fatalf("content_block_stop without content_block_start at index %d:\n%s", idx, stream)
			}
			delete(openBlocks, idx)
		}
	}
}

func TestOpenAIClaudeEmptyToolNameStreamingChainSkipsInvalidHistory(t *testing.T) {
	stream := collectOpenAIToClaudeStreamViaSDK(t,
		`data: {"id":"chatcmpl-empty-tool","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_empty","type":"function","function":{"name":"","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-empty-tool","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl-empty-tool","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
		`data: [DONE]`,
	)

	if strings.Contains(stream, `"type":"tool_use"`) {
		t.Fatalf("empty OpenAI function.name must not become Claude tool_use:\n%s", stream)
	}
	if strings.Contains(stream, `"type":"input_json_delta"`) {
		t.Fatalf("empty OpenAI function.name must not emit Claude input_json_delta:\n%s", stream)
	}
	if !strings.Contains(stream, `"stop_reason":"end_turn"`) {
		t.Fatalf("empty-only OpenAI tool_calls finish must become Claude end_turn:\n%s", stream)
	}
	if strings.Count(stream, `"type":"message_stop"`) != 1 {
		t.Fatalf("stream should emit exactly one message_stop:\n%s", stream)
	}
	assertNoOrphanAnthropicContentEvents(t, stream)

	dirtyReplay := []byte(`{
		"model":"claude-3-opus",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_empty","name":"","input":{"path":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_empty","content":"orphan result"}]}
		]
	}`)
	nextOpenAIRequest := sdktranslator.TranslateRequest(sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "m", dirtyReplay, false)
	messages := gjson.GetBytes(nextOpenAIRequest, "messages").Array()
	for _, msg := range messages {
		if msg.Get("tool_calls").Exists() {
			t.Fatalf("dirty empty-name Claude history must not replay as OpenAI tool_calls: %s", nextOpenAIRequest)
		}
		if msg.Get("role").String() == "tool" {
			t.Fatalf("tool_result for skipped tool_use must not replay as orphan OpenAI tool message: %s", nextOpenAIRequest)
		}
	}
}

func TestOpenAIClaudeProviderStyleMissingToolNameStreamingChain(t *testing.T) {
	stream := collectOpenAIToClaudeStreamViaSDK(t,
		`data: {"id":"chatcmpl-provider-missing-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_missing","type":"function","function":{"arguments":"{\"path\":\"a.go\""}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-provider-missing-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"offset\":10}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-provider-missing-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl-provider-missing-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}`,
		`data: [DONE]`,
	)

	if strings.Contains(stream, `"type":"tool_use"`) {
		t.Fatalf("provider-style arguments without function.name must not become Claude tool_use:\n%s", stream)
	}
	if strings.Contains(stream, `"type":"input_json_delta"`) || strings.Contains(stream, `"type":"content_block_stop"`) {
		t.Fatalf("provider-style arguments without function.name must not emit orphan tool events:\n%s", stream)
	}
	if !strings.Contains(stream, `"stop_reason":"end_turn"`) {
		t.Fatalf("provider-style missing tool name must finish as end_turn:\n%s", stream)
	}
	assertNoOrphanAnthropicContentEvents(t, stream)
}

func TestOpenAIClaudeLateValidToolNameStreamingChainPreservesToolResultReplay(t *testing.T) {
	stream := collectOpenAIToClaudeStreamViaSDK(t,
		`data: {"id":"chatcmpl-late-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_late","type":"function","function":{"arguments":"{\"path\":\"x\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-late-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"Read"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-late-name","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)

	start := strings.Index(stream, `"type":"tool_use"`)
	delta := strings.Index(stream, `"type":"input_json_delta"`)
	stop := strings.Index(stream, `"type":"content_block_stop"`)
	if start == -1 || delta == -1 || stop == -1 || !(start < delta && delta < stop) {
		t.Fatalf("late valid tool name should emit ordered Claude tool block events:\n%s", stream)
	}
	if !strings.Contains(stream, `"name":"Read"`) || !strings.Contains(stream, `"partial_json":"{\"path\":\"x\"}"`) {
		t.Fatalf("late valid tool name should preserve name and buffered arguments:\n%s", stream)
	}
	if !strings.Contains(stream, `"stop_reason":"tool_use"`) {
		t.Fatalf("valid OpenAI tool call must remain Claude tool_use stop_reason:\n%s", stream)
	}
	assertNoOrphanAnthropicContentEvents(t, stream)

	replay := []byte(`{
		"model":"claude-3-opus",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_late","name":"Read","input":{"path":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_late","content":"file contents"}]}
		]
	}`)
	nextOpenAIRequest := sdktranslator.TranslateRequest(sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "m", replay, false)
	messages := gjson.GetBytes(nextOpenAIRequest, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("valid Claude tool replay should produce assistant tool_calls and tool result, got %d: %s", len(messages), nextOpenAIRequest)
	}
	if got := messages[0].Get("tool_calls.0.function.name").String(); got != "Read" {
		t.Fatalf("tool_calls.0.function.name = %q, want Read: %s", got, nextOpenAIRequest)
	}
	if got := messages[1].Get("role").String(); got != "tool" {
		t.Fatalf("second replayed message role = %q, want tool: %s", got, nextOpenAIRequest)
	}
	if got := messages[1].Get("tool_call_id").String(); got != "call_late" {
		t.Fatalf("tool_call_id = %q, want call_late: %s", got, nextOpenAIRequest)
	}
}

func TestOpenAIClaudeMixedValidAndEmptyToolNameStreamingChain(t *testing.T) {
	stream := collectOpenAIToClaudeStreamViaSDK(t,
		`data: {"id":"chatcmpl-mixed-tools","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_valid","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"a.go\"}"}},{"index":1,"id":"call_empty","type":"function","function":{"name":"   ","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-mixed-tools","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)

	if got := strings.Count(stream, `"type":"tool_use"`); got != 1 {
		t.Fatalf("expected exactly one valid Claude tool_use, got %d:\n%s", got, stream)
	}
	if strings.Contains(stream, "call_empty") {
		t.Fatalf("empty-name OpenAI tool call must not appear in Claude stream:\n%s", stream)
	}
	if !strings.Contains(stream, `"name":"Read"`) || !strings.Contains(stream, `"stop_reason":"tool_use"`) {
		t.Fatalf("valid tool call should be preserved while empty one is skipped:\n%s", stream)
	}
	assertNoOrphanAnthropicContentEvents(t, stream)
}

func TestOpenAIClaudeProviderStyleParallelEmptyFirstIndexStreamingChain(t *testing.T) {
	stream := collectOpenAIToClaudeStreamViaSDK(t,
		`data: {"id":"chatcmpl-provider-parallel","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_empty","type":"function","function":{"arguments":"{\"path\":\"x\"}"}},{"index":1,"id":"call_valid","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"a.go\""}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-provider-parallel","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"   "}},{"index":1,"function":{"arguments":",\"offset\":10}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-provider-parallel","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)

	if got := strings.Count(stream, `"type":"tool_use"`); got != 1 {
		t.Fatalf("expected exactly one valid Claude tool_use, got %d:\n%s", got, stream)
	}
	if strings.Contains(stream, "call_empty") {
		t.Fatalf("empty first-index tool call must not appear in Claude stream:\n%s", stream)
	}
	if !strings.Contains(stream, `"name":"Read"`) || !strings.Contains(stream, `"stop_reason":"tool_use"`) {
		t.Fatalf("valid parallel tool should be preserved while empty one is skipped:\n%s", stream)
	}
	assertNoOrphanAnthropicContentEvents(t, stream)
}

func TestOpenAIClaudeEmptyToolNameNonStreamingChain(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl-empty-tool","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_empty","type":"function","function":{"name":"","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)

	out := sdktranslator.TranslateNonStream(
		context.Background(),
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatClaude,
		"m",
		[]byte(`{"model":"m","stream":false}`),
		[]byte(`{"model":"m","stream":false}`),
		raw,
		nil,
	)
	if strings.Contains(out, `"type":"tool_use"`) || strings.Contains(out, `"name":""`) {
		t.Fatalf("empty OpenAI function.name must not become Claude non-streaming tool_use:\n%s", out)
	}
	if got := gjson.Get(out, "stop_reason").String(); got != "end_turn" {
		t.Fatalf("Claude non-streaming stop_reason = %q, want end_turn: %s", got, out)
	}
}
