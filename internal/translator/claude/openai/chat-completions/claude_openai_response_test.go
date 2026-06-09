package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeResponseToOpenAI_StripsThinkingAndEmitsUsageChunk(t *testing.T) {
	ctx := context.Background()
	originalReq := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"只输出 OK"}],"stream":true,"stream_options":{"include_usage":true}}`)

	var param any

	start := ConvertClaudeResponseToOpenAI(ctx, "claude-sonnet-4-5", originalReq, nil, []byte(`data: {"type":"message_start","message":{"id":"msg_1"}}`), &param)
	if len(start) != 1 {
		t.Fatalf("start chunks = %d, want 1", len(start))
	}
	if got := gjson.Get(start[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("start role = %q, want assistant; chunk=%s", got, start[0])
	}

	thinking := ConvertClaudeResponseToOpenAI(ctx, "claude-sonnet-4-5", originalReq, nil, []byte(`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"secret"}}`), &param)
	if len(thinking) != 0 {
		t.Fatalf("thinking chunks = %d, want 0; chunks=%v", len(thinking), thinking)
	}

	text := ConvertClaudeResponseToOpenAI(ctx, "claude-sonnet-4-5", originalReq, nil, []byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`), &param)
	if len(text) != 1 {
		t.Fatalf("text chunks = %d, want 1", len(text))
	}
	if got := gjson.Get(text[0], "choices.0.delta.content").String(); got != "OK" {
		t.Fatalf("delta content = %q, want OK; chunk=%s", got, text[0])
	}
	if gjson.Get(text[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("unexpected reasoning_content in chunk: %s", text[0])
	}

	done := ConvertClaudeResponseToOpenAI(ctx, "claude-sonnet-4-5", originalReq, nil, []byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}`), &param)
	if len(done) != 2 {
		t.Fatalf("done chunks = %d, want 2; chunks=%v", len(done), done)
	}
	if got := gjson.Get(done[0], "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop; chunk=%s", got, done[0])
	}
	if got := len(gjson.Get(done[1], "choices").Array()); got != 0 {
		t.Fatalf("usage chunk choices len = %d, want 0; chunk=%s", got, done[1])
	}
	if got := gjson.Get(done[1], "usage.prompt_tokens").Int(); got != 11 {
		t.Fatalf("usage prompt_tokens = %d, want 11; chunk=%s", got, done[1])
	}
}

func TestConvertClaudeResponseToOpenAINonStream_StripsThinking(t *testing.T) {
	raw := []byte(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-5\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"secret\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}\n\n",
	)

	got := ConvertClaudeResponseToOpenAINonStream(context.Background(), "claude-sonnet-4-5", nil, nil, raw, nil)
	if content := gjson.Get(got, "choices.0.message.content").String(); content != "Hello" {
		t.Fatalf("message content = %q, want Hello; payload=%s", content, got)
	}
	if gjson.Get(got, "choices.0.message.reasoning").Exists() {
		t.Fatalf("unexpected reasoning field in payload: %s", got)
	}
}
