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

func TestConvertClaudeRequestToCodex_SystemBecomesInstructions(t *testing.T) {
	input := []byte(`{
		"model": "claude-opus-5",
		"system": [
			{"type": "text", "text": "You are Claude Code."},
			{"type": "text", "text": "Follow repository instructions."}
		],
		"messages": [
			{"role": "user", "content": "hello"}
		]
	}`)

	out := ConvertClaudeRequestToCodex("gpt-5.6-sol", input, true)

	gotInstructions := gjson.GetBytes(out, "instructions").String()
	wantInstructions := "You are Claude Code.\n\nFollow repository instructions."

	if gotInstructions != wantInstructions {
		t.Fatalf("unexpected instructions:\nwant: %q\ngot:  %q\noutput: %s",
			wantInstructions, gotInstructions, out)
	}

	inputMessages := gjson.GetBytes(out, "input").Array()
	for _, message := range inputMessages {
		role := message.Get("role").String()
		if role == "system" || role == "developer" {
			t.Fatalf("system/developer message must not appear in input: %s", out)
		}
	}
}

func TestConvertClaudeRequestToCodex_InConversationSystemMessageFolded(t *testing.T) {
	// Claude Code Agent Teams injects a role:"system" message inside messages.
	// It must be folded into instructions, never emitted into input.
	input := []byte(`{
		"model": "gpt-5.6-sol",
		"system": [{"type": "text", "text": "You are Claude Code."}],
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "system", "content": [{"type": "text", "text": "Available agent types for the Agent tool: BigBrother."}]},
			{"role": "user", "content": "go"}
		]
	}`)

	out := ConvertClaudeRequestToCodex("gpt-5.6-sol", input, true)

	gotInstructions := gjson.GetBytes(out, "instructions").String()
	wantInstructions := "You are Claude Code.\n\nAvailable agent types for the Agent tool: BigBrother."
	if gotInstructions != wantInstructions {
		t.Fatalf("unexpected instructions:\nwant: %q\ngot:  %q\noutput: %s",
			wantInstructions, gotInstructions, out)
	}

	for _, message := range gjson.GetBytes(out, "input").Array() {
		if role := message.Get("role").String(); role == "system" || role == "developer" {
			t.Fatalf("system/developer message must not appear in input: %s", out)
		}
	}
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
