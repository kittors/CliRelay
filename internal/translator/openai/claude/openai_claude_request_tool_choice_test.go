package claude

import (
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

// Two tools, so a test can tell "tools kept" apart from "tools dropped".
const toolChoiceTestTools = `[
	{"name":"tool_a","description":"a","input_schema":{"type":"object","properties":{}}},
	{"name":"tool_b","description":"b","input_schema":{"type":"object","properties":{}}}
]`

func TestConvertClaudeRequestToOpenAI_ToolChoiceMapping(t *testing.T) {
	cases := []struct {
		name       string
		toolChoice string
		want       string // expected OpenAI tool_choice as raw JSON; empty means absent
	}{
		// Issue #1042: "none" fell into the default branch and was sent as "auto".
		{name: "none", toolChoice: `{"type":"none"}`, want: `"none"`},
		{name: "auto", toolChoice: `{"type":"auto"}`, want: `"auto"`},
		{name: "any", toolChoice: `{"type":"any"}`, want: `"required"`},
		{name: "named", toolChoice: `{"type":"tool","name":"tool_b"}`, want: `{"type":"function","function":{"name":"tool_b"}}`},
		// Unrecognized choices are left to the upstream default rather than forced to "auto".
		{name: "unknown type", toolChoice: `{"type":"not_a_choice"}`, want: ``},
		{name: "missing type", toolChoice: `{"disable_parallel_tool_use":true}`, want: ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"model":"claude-3-opus","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` +
				`"tools":` + toolChoiceTestTools + `,"tool_choice":` + tc.toolChoice + `}`
			result := ConvertClaudeRequestToOpenAI("test-model", []byte(input), false)

			assertToolChoice(t, result, tc.want)
			if got := gjson.GetBytes(result, "tools.#").Int(); got != 2 {
				t.Fatalf("tools count = %d, want 2: %s", got, result)
			}
		})
	}
}

func TestConvertClaudeRequestToOpenAI_ToolChoiceWithoutToolsOmitted(t *testing.T) {
	toolsVariants := []struct {
		name  string
		field string
	}{
		{name: "no tools field", field: ``},
		{name: "empty tools", field: `,"tools":[]`},
		{name: "only nameless tools", field: `,"tools":[{"name":" ","input_schema":{"type":"object"}}]`},
	}
	choices := []struct {
		name  string
		value string
	}{
		{name: "none", value: `{"type":"none"}`},
		{name: "unknown type", value: `{"type":"not_a_choice"}`},
	}

	for _, tools := range toolsVariants {
		for _, choice := range choices {
			t.Run(tools.name+"/"+choice.name, func(t *testing.T) {
				input := `{"model":"claude-3-opus","max_tokens":64,"messages":[{"role":"user","content":"hi"}]` +
					tools.field + `,"tool_choice":` + choice.value + `}`
				result := ConvertClaudeRequestToOpenAI("test-model", []byte(input), false)

				if gjson.GetBytes(result, "tools").Exists() {
					t.Fatalf("expected no tools in the translated request: %s", result)
				}
				// Chat Completions rejects any tool_choice, "none" included, sent without tools.
				assertToolChoice(t, result, ``)
			})
		}
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
