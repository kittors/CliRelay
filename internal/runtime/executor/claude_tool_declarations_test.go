package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestDeclareClaudeHistoryToolsWhenTheRequestHasNone(t *testing.T) {
	// A title request over a conversation that used tools, sent without them.
	body := []byte(`{"model":"claude-sonnet-5-5","tool_choice":{"type":"auto"},"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}},{"type":"tool_use","id":"toolu_2","name":"bash","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a"},{"type":"tool_result","tool_use_id":"toolu_2","content":"b"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_3","name":"read_file","input":{}},{"type":"tool_use","id":"toolu_4","name":"functions.bad:0","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","content":"c"},{"type":"tool_result","tool_use_id":"toolu_4","content":"d"}]},` +
		`{"role":"user","content":"Give this conversation a title."}]}`)
	out, declared := declareClaudeHistoryTools(body)
	if len(declared) != 2 || declared[0] != "read_file" || declared[1] != "bash" {
		t.Fatalf("declared %v, want [read_file bash] in the order the calls appear", declared)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 || tools[0].Get("name").String() != "read_file" || tools[1].Get("name").String() != "bash" {
		t.Fatalf("tools = %s", gjson.GetBytes(out, "tools").Raw)
	}
	if tools[0].Get("input_schema.type").String() != "object" || tools[0].Get("description").String() == "" {
		t.Fatalf("declaration = %s, want a description and an object schema", tools[0].Raw)
	}
	if got := gjson.GetBytes(out, "tool_choice").Raw; got != `{"type":"none"}` {
		t.Fatalf("tool_choice = %s, want none", got)
	}
	if again, _ := declareClaudeHistoryTools(body); string(again) != string(out) {
		t.Fatal("declaring the same history twice gave different requests")
	}
}

func TestDeclareClaudeHistoryToolsLeavesOtherRequestsAlone(t *testing.T) {
	for name, body := range map[string]string{
		"tools declared": `{"tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}]}]}`,
		"no tool calls":  `{"messages":[{"role":"user","content":"hi"}]}`,
	} {
		if out, declared := declareClaudeHistoryTools([]byte(body)); string(out) != body || declared != nil {
			t.Fatalf("%s: request changed to %s", name, out)
		}
	}
}
