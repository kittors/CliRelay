package chat_completions

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func claudeContentTypes(msg gjson.Result) string {
	var types []string
	msg.Get("content").ForEach(func(_, part gjson.Result) bool {
		types = append(types, part.Get("type").String())
		return true
	})
	return strings.Join(types, ",")
}

// Anthropic rejects an empty text block ("text content blocks must be
// non-empty"). Chat clients send one next to an image attached without typed
// text, and as the content of an assistant turn that only called tools. Empty
// parts are skipped like an empty string content already is; whitespace-only
// text is accepted upstream and kept (#1166).
func TestConvertOpenAIRequestToClaude_SkipsEmptyTextParts(t *testing.T) {
	input := []byte(`{
		"model": "claude-sonnet-5-5",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": ""},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo="}}
			]},
			{"role": "assistant", "content": [{"type": "text", "text": ""}], "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "shell", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "a.txt"},
			{"role": "user", "content": [{"type": "text", "text": "  "}, {"type": "text", "text": "thanks"}]}
		]
	}`)

	msgs := gjson.GetBytes(ConvertOpenAIRequestToClaude("claude-sonnet-5-5", input, false), "messages").Array()
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4", len(msgs))
	}
	for i, want := range []string{"image", "tool_use", "tool_result", "text,text"} {
		if got := claudeContentTypes(msgs[i]); got != want {
			t.Errorf("messages.%d content = %s, want %s: %s", i, got, want, msgs[i].Raw)
		}
	}
	if got := msgs[3].Get("content.0.text").String(); got != "  " {
		t.Errorf("whitespace-only text = %q, want it kept", got)
	}
}

// System text parts become text blocks of the leading user message; an empty
// one is skipped, as an empty string system content already was.
func TestConvertOpenAIRequestToClaude_SkipsEmptySystemTextParts(t *testing.T) {
	input := []byte(`{
		"model": "claude-sonnet-5-5",
		"messages": [
			{"role": "system", "content": [{"type": "text", "text": ""}, {"type": "text", "text": "Be brief."}]},
			{"role": "user", "content": "hi"}
		]
	}`)

	system := gjson.GetBytes(ConvertOpenAIRequestToClaude("claude-sonnet-5-5", input, false), "messages.0")
	if got := claudeContentTypes(system); got != "text" || system.Get("content.0.text").String() != "Be brief." {
		t.Fatalf("system message = %s, want only the \"Be brief.\" text block", system.Raw)
	}
}
