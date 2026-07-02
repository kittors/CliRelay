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

func TestConvertClaudeRequestToCodexConvertsSystemRoleMessagesToDeveloper(t *testing.T) {
	input := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [
			{"role": "system", "content": "metadata system"},
			{"role": "user", "content": "hi"}
		]
	}`)

	out := ConvertClaudeRequestToCodex("gpt-5.5", input, true)

	if got := gjson.GetBytes(out, "input.0.role").String(); got != "developer" {
		t.Fatalf("input.0.role = %q, want developer; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.text").String(); got != "metadata system" {
		t.Fatalf("input.0.content.0.text = %q, want metadata system; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "input.1.role").String(); got != "user" {
		t.Fatalf("input.1.role = %q, want user; body=%s", got, out)
	}
	gjson.GetBytes(out, "input").ForEach(func(index, item gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "system") {
			t.Fatalf("input.%d.role = system; body=%s", index.Int(), out)
		}
		return true
	})
}
