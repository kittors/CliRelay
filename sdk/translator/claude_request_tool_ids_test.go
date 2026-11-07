package translator

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEncodeClaudeRequestToolIDsForAClaudeUpstream(t *testing.T) {
	// A conversation held on Kimi through an OpenAI-format client, continued
	// on Claude: the request translation keeps Kimi's ids.
	body := []byte(`{"model":"claude-sonnet-5-5","messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"functions.get_weather:0","name":"get_weather","input":{}},{"type":"tool_use","id":"toolu_01AbC","name":"get_time","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"functions.get_weather:0","content":"sunny, functions.get_weather:0"},{"type":"tool_result","tool_use_id":"toolu_01AbC","content":"noon"}]}]}`)
	out := EncodeClaudeRequestToolIDs(body)
	if !gjson.ValidBytes(out) {
		t.Fatalf("encoded body is not JSON: %s", out)
	}
	call := gjson.GetBytes(out, "messages.1.content.0.id").String()
	result := gjson.GetBytes(out, "messages.2.content.0.tool_use_id").String()
	if call != encodeClaudeToolID("functions.get_weather:0") || result != call {
		t.Fatalf("call id %q and result id %q, want both %q", call, result, encodeClaudeToolID("functions.get_weather:0"))
	}
	if !isClaudeToolID(call) {
		t.Fatalf("encoded id %q is still outside the alphabet", call)
	}
	if got := gjson.GetBytes(out, "messages.1.content.1.id").String(); got != "toolu_01AbC" {
		t.Fatalf("a valid id became %q", got)
	}
	if got := gjson.GetBytes(out, "messages.2.content.0.content").String(); !strings.Contains(got, "functions.get_weather:0") {
		t.Fatalf("the id in the result text was rewritten: %q", got)
	}
	if again := EncodeClaudeRequestToolIDs(out); string(again) != string(out) {
		t.Fatal("encoding an encoded request changed it again")
	}
}

func TestEncodeClaudeRequestToolIDsKeepsValidIDs(t *testing.T) {
	// What a Claude client sends back after #1213 encoded an id for it.
	encoded := encodeClaudeToolID("functions.x:0")
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"` + encoded + `","name":"x","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + encoded + `","content":"ok"}]}]}`)
	if out := EncodeClaudeRequestToolIDs(body); string(out) != string(body) {
		t.Fatalf("a request with valid ids changed:\n%s", out)
	}
}
