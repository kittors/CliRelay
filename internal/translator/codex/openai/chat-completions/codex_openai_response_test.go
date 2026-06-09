package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAI_EmitsStandardChatChunks(t *testing.T) {
	ctx := context.Background()
	originalReq := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"只输出 OK"}],"stream":true,"stream_options":{"include_usage":true}}`)
	req := []byte(`{"model":"gpt-5.4","stream":true}`)

	var param any

	created := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", originalReq, req, []byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","created_at":1710000001}}`), &param)
	if len(created) != 1 {
		t.Fatalf("created chunks = %d, want 1", len(created))
	}
	if got := gjson.Get(created[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("created role = %q, want assistant; chunk=%s", got, created[0])
	}

	reasoning := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", originalReq, req, []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}`), &param)
	if len(reasoning) != 0 {
		t.Fatalf("reasoning chunks = %d, want 0; chunks=%v", len(reasoning), reasoning)
	}

	text := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", originalReq, req, []byte(`data: {"type":"response.output_text.delta","delta":"OK"}`), &param)
	if len(text) != 1 {
		t.Fatalf("text chunks = %d, want 1", len(text))
	}
	if got := gjson.Get(text[0], "model").String(); got != "gpt-5.4" {
		t.Fatalf("chunk model = %q, want gpt-5.4; chunk=%s", got, text[0])
	}
	if got := gjson.Get(text[0], "choices.0.delta.content").String(); got != "OK" {
		t.Fatalf("delta content = %q, want OK; chunk=%s", got, text[0])
	}
	if gjson.Get(text[0], "choices.0.delta.reasoning_content").Exists() {
		t.Fatalf("unexpected reasoning_content in chat chunk: %s", text[0])
	}

	done := ConvertCodexResponseToOpenAI(ctx, "gpt-5.4", originalReq, req, []byte(`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","created_at":1710000001,"usage":{"input_tokens":9,"output_tokens":24,"total_tokens":33,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":17}}}}`), &param)
	if len(done) != 2 {
		t.Fatalf("done chunks = %d, want 2; chunks=%v", len(done), done)
	}
	if got := gjson.Get(done[0], "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop; chunk=%s", got, done[0])
	}
	if got := len(gjson.Get(done[1], "choices").Array()); got != 0 {
		t.Fatalf("usage chunk choices len = %d, want 0; chunk=%s", got, done[1])
	}
	if got := gjson.Get(done[1], "usage.prompt_tokens").Int(); got != 9 {
		t.Fatalf("usage prompt_tokens = %d, want 9; chunk=%s", got, done[1])
	}
}

func TestConvertCodexResponseToOpenAINonStream_StripsReasoningSummary(t *testing.T) {
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","created_at":1710000001,"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"secret thinking"}]},{"type":"message","content":[{"type":"output_text","text":"Hello!"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)

	got := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.4", []byte(`{"tools":[]}`), nil, raw, nil)
	if content := gjson.Get(got, "choices.0.message.content").String(); content != "Hello!" {
		t.Fatalf("message content = %q, want Hello!; payload=%s", content, got)
	}
	if gjson.Get(got, "choices.0.message.reasoning_content").Exists() {
		t.Fatalf("unexpected reasoning_content in non-stream payload: %s", got)
	}
}

func TestConvertOpenAIRequestToCodex_StripsUnsupportedTokenLimitFields(t *testing.T) {
	input := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1024,"max_tokens":2048}`)
	got := ConvertOpenAIRequestToCodex("gpt-5.4", input, true)
	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("%s should be stripped for codex upstream; payload=%s", field, got)
		}
	}
}
