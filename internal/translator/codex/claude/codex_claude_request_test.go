package claude

import (
	"strings"
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

func TestConvertClaudeRequestToCodex_MapsToolResultImageToVisionInput(t *testing.T) {
	input := []byte(`{
		"messages": [
			{
				"role": "assistant",
				"content": [{"type": "tool_use", "id": "call_read", "name": "Read", "input": {"file_path": "screenshot.png"}}]
			},
			{
				"role": "user",
				"content": [{
					"type": "tool_result",
					"tool_use_id": "call_read",
					"content": [
						{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}
					]
				}]
			}
		]
	}`)

	out := ConvertClaudeRequestToCodex("gpt-5.6-luna", input, true)
	items := gjson.GetBytes(out, "input")
	if !items.IsArray() || len(items.Array()) != 3 {
		t.Fatalf("expected function call, output, and image message: %s", out)
	}

	functionOutput := items.Array()[1]
	if got := functionOutput.Get("type").String(); got != "function_call_output" {
		t.Fatalf("second input type = %q, want function_call_output", got)
	}
	if got := functionOutput.Get("output").String(); got != "[Image attached]" {
		t.Fatalf("function output = %q, want image completion marker", got)
	}
	if strings.Contains(functionOutput.Get("output").String(), "aGVsbG8=") {
		t.Fatalf("function output must not contain image Base64: %s", functionOutput.Raw)
	}

	imageMessage := items.Array()[2]
	if got := imageMessage.Get("role").String(); got != "user" {
		t.Fatalf("image message role = %q, want user", got)
	}
	image := imageMessage.Get("content.0")
	if got := image.Get("type").String(); got != "input_image" {
		t.Fatalf("image content type = %q, want input_image", got)
	}
	if got := image.Get("image_url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image URL = %q", got)
	}
}
