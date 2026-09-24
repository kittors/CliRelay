package chat_completions

import (
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

// Two tools, so a test can tell "tools kept" apart from "tools dropped".
const toolChoiceTestTools = `[
	{"type":"function","function":{"name":"tool_a","description":"a","parameters":{"type":"object","properties":{}}}},
	{"type":"function","function":{"name":"tool_b","description":"b","parameters":{"type":"object","properties":{}}}}
]`

func TestConvertOpenAIRequestToClaude_ToolChoiceMapping(t *testing.T) {
	cases := []struct {
		name       string
		toolChoice string
		want       string // expected Claude tool_choice as raw JSON
	}{
		// Issue #1042: "none" left tool_choice unset, which Anthropic treats as auto
		// while tools are attached.
		{name: "none", toolChoice: `"none"`, want: `{"type":"none"}`},
		{name: "auto", toolChoice: `"auto"`, want: `{"type":"auto"}`},
		{name: "required", toolChoice: `"required"`, want: `{"type":"any"}`},
		{name: "named", toolChoice: `{"type":"function","function":{"name":"tool_b"}}`, want: `{"type":"tool","name":"tool_b"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],` +
				`"tools":` + toolChoiceTestTools + `,"tool_choice":` + tc.toolChoice + `}`
			result := ConvertOpenAIRequestToClaude("test-model", []byte(input), false)

			assertToolChoice(t, result, tc.want)
			if got := gjson.GetBytes(result, "tools.#").Int(); got != 2 {
				t.Fatalf("tools count = %d, want 2: %s", got, result)
			}
		})
	}
}

func TestConvertOpenAIRequestToClaude_ToolChoiceNoneWithoutToolsOmitted(t *testing.T) {
	cases := []struct {
		name  string
		field string
	}{
		{name: "no tools field", field: ``},
		{name: "empty tools", field: `,"tools":[]`},
		{name: "only non-function tools", field: `,"tools":[{"type":"custom","custom":{"name":"x"}}]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]` +
				tc.field + `,"tool_choice":"none"}`
			result := ConvertOpenAIRequestToClaude("test-model", []byte(input), false)

			if gjson.GetBytes(result, "tools").Exists() {
				t.Fatalf("expected no tools in the translated request: %s", result)
			}
			// Anthropic rejects a tool_choice sent without tools; "none" is its default then.
			assertToolChoice(t, result, ``)
		})
	}
}

func assertToolChoice(t *testing.T, body []byte, want string) {
	t.Helper()
	got := gjson.GetBytes(body, "tool_choice")
	if want == "" {
		if got.Exists() {
			t.Fatalf("tool_choice = %s, want it absent: %s", got.Raw, body)
		}
		return
	}
	if !got.Exists() || !reflect.DeepEqual(got.Value(), gjson.Parse(want).Value()) {
		t.Fatalf("tool_choice = %s, want %s: %s", got.Raw, want, body)
	}
}
