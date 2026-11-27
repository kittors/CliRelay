package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodex_StripsDeferLoading(t *testing.T) {
	input := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"name": "WebSearch", "description": "search", "input_schema": {"type": "object"}, "defer_loading": true},
			{"name": "Read", "description": "read", "input_schema": {"type": "object"}}
		]
	}`)

	out := ConvertClaudeRequestToCodex("gpt-5.2", input, true)

	// Ensure defer_loading is removed from all tools in the output
	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() {
		t.Fatal("expected tools to be an array")
	}
	tools.ForEach(func(i, tool gjson.Result) bool {
		if tool.Get("defer_loading").Exists() {
			t.Fatalf("tool %d still has defer_loading: %s", i.Int(), tool.Raw)
		}
		return true
	})
}

func TestConvertClaudeRequestToCodex_EmptyToolsOmitsToolChoice(t *testing.T) {
	input := []byte(`{
		"model":"claude-haiku-4-5",
		"messages":[{"role":"user","content":"Create a title"}],
		"tools":[],
		"tool_choice":{"type":"auto"}
	}`)

	out := ConvertClaudeRequestToCodex("grok-4", input, false)
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("empty tools must not emit tool_choice: %s", out)
	}
	if tools := gjson.GetBytes(out, "tools"); tools.Exists() && len(tools.Array()) != 0 {
		t.Fatalf("unexpected translated tools: %s", out)
	}
}

func TestConvertClaudeRequestToCodex_InlineSystemBecomesDeveloper(t *testing.T) {
	input := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "system", "content": [{"type": "text", "text": "inline reminder"}]},
			{"role": "assistant", "content": "working on it"},
			{"role": "system", "content": "follow up note"}
		]
	}`)

	out := ConvertClaudeRequestToCodex("gpt-test", input, false)
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 4 {
		t.Fatalf("expected 4 input items, got %d: %s", len(items), out)
	}
	wantRoles := []string{"user", "developer", "assistant", "developer"}
	for i, wantRole := range wantRoles {
		if got := items[i].Get("role").String(); got != wantRole {
			t.Fatalf("input[%d].role = %q, want %q: %s", i, got, wantRole, out)
		}
	}
	if got := items[1].Get("content.0.type").String(); got != "input_text" {
		t.Fatalf("input[1].content[0].type = %q, want input_text: %s", got, out)
	}
	if got := items[1].Get("content.0.text").String(); got != "inline reminder" {
		t.Fatalf("input[1].content[0].text = %q, want inline reminder: %s", got, out)
	}
	if got := items[3].Get("content.0.text").String(); got != "follow up note" {
		t.Fatalf("input[3].content[0].text = %q, want follow up note: %s", got, out)
	}
}
