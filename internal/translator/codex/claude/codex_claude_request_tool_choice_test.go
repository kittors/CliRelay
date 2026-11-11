package claude

import (
	"github.com/tidwall/gjson"
	"testing"
)

// Regression for Android-Login-Analysis's #1042: a caller prohibiting tools
// must not become auto, and disabling parallel tool use must survive translation.
func TestClaudeToCodexNoneAndParallel(t *testing.T) {
	choices := []struct {
		name, raw, wantChoice string
		parallel              bool
	}{
		{"absent", "", "auto", true},
		{"auto", `{"type":"auto"}`, "auto", true},
		{"none", `{"type":"none"}`, "none", true},
		{"auto sequential", `{"type":"auto","disable_parallel_tool_use":true}`, "auto", false},
		{"auto parallel", `{"type":"auto","disable_parallel_tool_use":false}`, "auto", true},
		{"none sequential", `{"type":"none","disable_parallel_tool_use":true}`, "none", false},
		{"null flag", `{"type":"auto","disable_parallel_tool_use":null}`, "auto", true},
		{"string flag is not boolean", `{"type":"auto","disable_parallel_tool_use":"true"}`, "auto", true},
	}
	toolsets := []struct {
		name, raw string
		count     int
	}{
		{"absent", "", 0}, {"empty", `[]`, 0},
		{"functions", `[{"name":"a","strict":true,"input_schema":{"type":"object","properties":{}}},{"name":"b","input_schema":{"type":"object","properties":{}}}]`, 2},
		{"builtin", `[{"type":"web_search_20250305","name":"web_search"}]`, 1},
	}
	for _, tools := range toolsets {
		for _, tc := range choices {
			for _, stream := range []bool{false, true} {
				t.Run(tools.name+"/"+tc.name+"/stream="+map[bool]string{false: "false", true: "true"}[stream], func(t *testing.T) {
					in := `{"messages":[{"role":"user","content":"hi"}]`
					if tools.raw != "" {
						in += `,"tools":` + tools.raw
					}
					if tc.raw != "" {
						in += `,"tool_choice":` + tc.raw
					}
					in += `}`
					raw := []byte(in)
					out := ConvertClaudeRequestToCodex("gpt-6-astra", raw, stream)
					if string(raw) != in {
						t.Fatal("input mutated")
					}
					if !gjson.ValidBytes(out) {
						t.Fatalf("invalid JSON: %s", out)
					}
					choice := gjson.GetBytes(out, "tool_choice")
					if tools.count == 0 {
						if choice.Exists() {
							t.Fatalf("choice without tools: %s", out)
						}
					} else if choice.String() != tc.wantChoice {
						t.Fatalf("choice = %s, want %s", choice.Raw, tc.wantChoice)
					}
					if got := gjson.GetBytes(out, "tools.#").Int(); got != int64(tools.count) {
						t.Fatalf("tools count %d, want %d", got, tools.count)
					}
					if got := gjson.GetBytes(out, "parallel_tool_calls"); got.Bool() != tc.parallel || (got.Type != gjson.True && got.Type != gjson.False) {
						t.Fatalf("parallel = %s, want %v", got.Raw, tc.parallel)
					}
					if tools.name == "functions" {
						if gjson.GetBytes(out, "tools.0.strict").Raw != "false" {
							t.Fatal("unrelated Claude strict policy changed")
						}
						if gjson.GetBytes(out, "tools.1.name").String() != "b" {
							t.Fatal("declarations changed")
						}
					}
					if tools.name == "builtin" && gjson.GetBytes(out, "tools.0.type").String() != "web_search" {
						t.Fatal("builtin mapping changed")
					}
				})
			}
		}
	}
}

func TestClaudeToCodexDisableParallelForForcedChoices(t *testing.T) {
	// This first fix preserves the single-call restriction even for choice modes
	// whose force/named mapping still needs a separate compatibility fix.
	for _, choice := range []string{`{"type":"any","disable_parallel_tool_use":true}`, `{"type":"tool","name":"a","disable_parallel_tool_use":true}`} {
		out := ConvertClaudeRequestToCodex("m", []byte(`{"tools":[{"name":"a","input_schema":{"type":"object","properties":{}}}],"tool_choice":`+choice+`}`), false)
		if gjson.GetBytes(out, "parallel_tool_calls").Raw != "false" {
			t.Fatalf("parallel restriction lost: %s", out)
		}
	}
}
