package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

// Chat Completions functions are non-strict unless the client asks for strict,
// while the Responses API tries strict mode when strict is missing. The Codex
// tool has to carry the Chat meaning explicitly.
func TestConvertOpenAIRequestToCodexWritesChatStrictDefault(t *testing.T) {
	cases := []struct {
		name   string
		strict string // raw function.strict; empty leaves it out
		want   string // raw strict on the Codex tool
	}{
		{name: "missing", want: "false"},
		{name: "null", strict: "null", want: "false"},
		{name: "false", strict: "false", want: "false"},
		{name: "true", strict: "true", want: "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := `{"name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`
			if tc.strict != "" {
				fn += `,"strict":` + tc.strict
			}
			fn += `}`
			in := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":` + fn + `}]}`

			out := ConvertOpenAIRequestToCodex("gpt-6-astra", []byte(in), true)

			if got := gjson.GetBytes(out, "tools.0.strict").Raw; got != tc.want {
				t.Fatalf("tools.0.strict = %q, want %s; body: %s", got, tc.want, out)
			}
		})
	}
}

// automationUpdateParameters has the shape of Codex App's
// mcp__codex_app__automation_update schema: an empty root "properties" beside
// a oneOf of $ref branches that each require id. gpt-6-astra rejected it with
// "Unsatisfiable schema: required property 'id' is unsatisfiable" when the
// translated tool had no strict and the request carried Codex's Version
// header; the same request with strict false was answered.
const automationUpdateParameters = `{"type":"object","properties":{},"oneOf":[{"$ref":"#/$defs/view"},{"$ref":"#/$defs/delete"}],"$defs":{"view":{"type":"object","properties":{"id":{"$ref":"#/$defs/id"},"mode":{"type":"string","enum":["view"]}},"required":["mode","id"],"additionalProperties":false},"delete":{"type":"object","properties":{"id":{"$ref":"#/$defs/id"},"mode":{"type":"string","enum":["delete"]}},"required":["mode","id"],"additionalProperties":false},"id":{"type":"string"}}}`

func TestConvertOpenAIRequestToCodexKeepsUnionSchemaNonStrict(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"mcp__codex_app__automation_update","description":"Manage automations.","parameters":` + automationUpdateParameters + `}}]}`

	out := ConvertOpenAIRequestToCodex("gpt-6-astra", []byte(in), true)

	tool := gjson.GetBytes(out, "tools.0")
	if got := tool.Get("strict").Raw; got != "false" {
		t.Fatalf("strict = %q, want false; tool: %s", got, tool.Raw)
	}
	if got := tool.Get("parameters").Raw; got != automationUpdateParameters {
		t.Fatalf("parameters changed\n got: %s\nwant: %s", got, automationUpdateParameters)
	}
	if got := tool.Get("name").String(); got != "mcp__codex_app__automation_update" {
		t.Fatalf("name = %q", got)
	}
}
