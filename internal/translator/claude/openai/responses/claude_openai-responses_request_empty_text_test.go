package responses

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const emptyTextTestImage = `{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,iVBORw0KGgo="}`

func responsesRequest(items ...string) []byte {
	req := `{"model":"claude-sonnet-5-5","input":[]}`
	for _, item := range items {
		req, _ = sjson.SetRaw(req, "input.-1", item)
	}
	return []byte(req)
}

// An image sent without any typed text arrives as an empty input_text next to
// the input_image (pi does this). Anthropic rejects the empty text block with
// "messages: text content blocks must be non-empty", so only the image may go
// (#1166).
func TestConvertOpenAIResponsesRequestToClaude_ImageWithEmptyTextDropsTextBlock(t *testing.T) {
	in := responsesRequest(`{"type":"message","role":"user","content":[{"type":"input_text","text":""},` + emptyTextTestImage + `]}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-5-5", in, false)
	content := gjson.GetBytes(out, "messages.0.content")
	if !content.IsArray() || len(content.Array()) != 1 || content.Get("0.type").String() != "image" {
		t.Fatalf("content = %s, want only the image block", content.Raw)
	}
	if role := gjson.GetBytes(out, "messages.0.role").String(); role != "user" {
		t.Fatalf("role = %q, want user", role)
	}
}

// Anthropic accepts a whitespace-only text block, so it is passed through.
func TestConvertOpenAIResponsesRequestToClaude_ImageWithWhitespaceTextKept(t *testing.T) {
	in := responsesRequest(`{"type":"message","role":"user","content":[{"type":"input_text","text":"  "},` + emptyTextTestImage + `]}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-5-5", in, false)
	content := gjson.GetBytes(out, "messages.0.content")
	if len(content.Array()) != 2 || content.Get("0.text").String() != "  " {
		t.Fatalf("content = %s, want the whitespace text block kept before the image", content.Raw)
	}
}

// Text that carries content is kept next to the image, as before.
func TestConvertOpenAIResponsesRequestToClaude_ImageWithTextKeepsBoth(t *testing.T) {
	in := responsesRequest(`{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this?"},` + emptyTextTestImage + `]}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-5-5", in, false)
	content := gjson.GetBytes(out, "messages.0.content")
	if len(content.Array()) != 2 || content.Get("0.text").String() != "what is this?" || content.Get("1.type").String() != "image" {
		t.Fatalf("content = %s, want the text block then the image block", content.Raw)
	}
}

// A plain text message keeps the legacy string content.
func TestConvertOpenAIResponsesRequestToClaude_PlainTextUnchanged(t *testing.T) {
	in := responsesRequest(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`)
	out := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-5-5", in, false)
	if got := gjson.GetBytes(out, "messages.0.content"); got.Type != gjson.String || got.String() != "hi" {
		t.Fatalf("content = %s, want the string \"hi\"", got.Raw)
	}
}

// A message whose only part is an empty text has no block left once that part
// is skipped. It still goes out with the empty string content it always had:
// dropping it would remove a turn and could put two assistant turns side by
// side.
func TestConvertOpenAIResponsesRequestToClaude_OnlyEmptyTextKeepsMessage(t *testing.T) {
	in := responsesRequest(
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`,
	)
	msgs := gjson.GetBytes(ConvertOpenAIResponsesRequestToClaude("claude-sonnet-5-5", in, false), "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (user, assistant, user)", len(msgs))
	}
	for i, wantRole := range []string{"user", "assistant", "user"} {
		if got := msgs[i].Get("role").String(); got != wantRole {
			t.Errorf("messages.%d.role = %q, want %q", i, got, wantRole)
		}
	}
	for _, i := range []int{1, 2} {
		if content := msgs[i].Get("content"); content.Type != gjson.String || content.String() != "" {
			t.Errorf("messages.%d.content = %s, want the empty string it had before", i, content.Raw)
		}
	}
}

// After a replayed reasoning item, the assistant items that follow join its
// thinking turn. A turn that only thought and called a tool can come back with
// an empty output_text between the two: it adds no block, and must not split
// the thinking from its tool_use either.
func TestConvertOpenAIResponsesRequestToClaude_EmptyAssistantTextInThinkingTurn(t *testing.T) {
	in := responsesRequest(
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"list the files"}]}`,
		reasoningItemJSON(testClaudeSig),
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}`,
		`{"type":"function_call","call_id":"toolu_01","name":"shell","arguments":"{\"command\":\"ls\"}"}`,
		`{"type":"function_call_output","call_id":"toolu_01","output":"a.txt"}`,
	)
	msgs := gjson.GetBytes(ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", in, true), "messages")
	if n := len(msgs.Array()); n != 3 {
		t.Fatalf("got %d messages, want 3 (user, assistant, user): %s", n, msgs.Raw)
	}
	if got := strings.Join(contentTypes(msgs.Get("1")), ","); got != "thinking,tool_use" {
		t.Fatalf("assistant content = %s, want thinking,tool_use: %s", got, msgs.Get("1").Raw)
	}
}
