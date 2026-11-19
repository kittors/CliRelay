package executor

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// reEncodeChatCalls hands the calls of a Chat turn back the way magpie does
// for a custom tool: the arguments decoded and written again by Go's
// json.Marshal, which escapes < > and &.
func reEncodeChatCalls(t *testing.T, turn chatTurn) (chatTurn, bool) {
	t.Helper()
	changed := false
	out := chatTurn{content: turn.content}
	for _, call := range turn.calls {
		args := gjson.Get(call, "function.arguments").String()
		var value map[string]any
		if err := json.Unmarshal([]byte(args), &value); err != nil {
			t.Fatalf("arguments %s: %v", args, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		changed = changed || string(encoded) != args
		call, _ = sjson.Set(call, "function.arguments", string(encoded))
		out.calls = append(out.calls, call)
	}
	return out, changed
}

// A Chat client that hands a call back as other bytes holding the same
// arguments still continues its chain: the second turn goes upstream as only
// the tool output on top of the first reply.
func TestCodexIncrementalChatReEncodedArgumentsGoAsIncrements(t *testing.T) {
	call, _ := sjson.Set(`{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_1","name":"exec_command"}`, "arguments", `{"cmd":"echo '<b>' && ls > out.txt"}`)
	upstream := newIncUpstream(t, []string{incReasoning, incReplyText1, call}, []string{incReplyText2})
	exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
	auth := upstream.auth()

	messages := []string{`{"role":"system","content":"be brief"}`, `{"role":"user","content":"look at the screen"}`}
	first := runChatTurn(t, exec, auth, messages)
	sentBack, changed := reEncodeChatCalls(t, first)
	if !changed || len(sentBack.calls) != 1 {
		t.Fatalf("fixture: the client did not re-encode the arguments: %+v", sentBack)
	}
	messages = append(messages, sentBack.message(), `{"role":"tool","tool_call_id":"call_1","content":"a.txt"}`)
	if second := runChatTurn(t, exec, auth, messages); second.content != "There is a.txt." {
		t.Fatalf("second turn = %+v", second)
	}

	msgs := upstream.websocketMessages()
	if len(msgs) != 2 {
		t.Fatalf("websocket messages = %d, want 2", len(msgs))
	}
	second := gjson.Parse(msgs[1].raw)
	if second.Get("previous_response_id").String() != "resp_1" || second.Get("input").Raw != `[{"type":"function_call_output","call_id":"call_1","output":"a.txt"}]` {
		t.Fatalf("second turn = %s, want only the tool output on top of resp_1", msgs[1].raw)
	}
	if got := pool.stats.byMode["on"]; got == nil || got.hits != 1 || got.reasons["echoed_output_mismatch"] != 0 {
		t.Fatalf("stats = %+v", got)
	}
}
