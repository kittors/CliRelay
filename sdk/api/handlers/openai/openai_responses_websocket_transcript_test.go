package openai

import (
	"testing"

	"github.com/tidwall/gjson"
)

// pi with `transport: websocket-cached` reuses one socket and sends an
// incremental input while the request body is otherwise unchanged. As soon as a
// field like `tools` changes (tools are added mid-session, which is the normal
// case when a tool group is activated), it resends the whole transcript and
// drops `previous_response_id`. Appending that to the previous turn submits the
// same function_call twice without its function_call_output in between, and
// Anthropic rejects the request with
// `messages.N: tool_use ids were found without tool_result blocks immediately after`.
func TestNormalizeResponsesWebsocketRequestTreatsFullTranscriptAsReplacement(t *testing.T) {
	create := []byte(`{"type":"response.create","model":"claude-opus-5-5","instructions":"sys","stream":true,"tools":[{"type":"function","name":"bash"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}`)
	_, lastRequest, errMsg := normalizeResponsesWebsocketRequest(create, nil, nil)
	if errMsg != nil {
		t.Fatalf("unexpected error on create: %v", errMsg.Error)
	}
	// The turn's own output, as the handler records it after every turn.
	lastResponseOutput := []byte(`[{"type":"function_call","call_id":"toolu_A","name":"bash","arguments":"{}"}]`)

	// Next turn: the tool set changed, so pi resends the entire conversation.
	next := []byte(`{"type":"response.create","model":"claude-opus-5-5","stream":true,"tools":[{"type":"function","name":"bash"},{"type":"function","name":"computer_use_list_windows"}],"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
		{"type":"function_call","call_id":"toolu_A","name":"bash","arguments":"{}"},
		{"type":"function_call_output","call_id":"toolu_A","output":"ok"}
	]}`)

	normalized, _, errMsg := normalizeResponsesWebsocketRequest(next, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}

	var calls, outputs int
	gjson.GetBytes(normalized, "input").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "function_call":
			calls++
		case "function_call_output":
			outputs++
		}
		return true
	})
	if calls != 1 || outputs != 1 {
		t.Fatalf("input items: function_call=%d function_call_output=%d, want 1 and 1; the previous turn's output must not be merged into a full transcript", calls, outputs)
	}
	if n := gjson.GetBytes(normalized, "input.#").Int(); n != 3 {
		t.Fatalf("input items = %d, want the 3 sent by the client", n)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must not be set on a replacement")
	}
	if model := gjson.GetBytes(normalized, "model").String(); model != "claude-opus-5-5" {
		t.Fatalf("model = %q, want it inherited from the previous request", model)
	}
}

// Every function_call the request carries must be answered by the next item,
// which is exactly what Anthropic validates.
func TestNormalizeResponsesWebsocketRequestKeepsToolCallsAnswered(t *testing.T) {
	create := []byte(`{"type":"response.create","model":"claude-opus-5-5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}`)
	_, lastRequest, _ := normalizeResponsesWebsocketRequest(create, nil, nil)
	lastResponseOutput := []byte(`[
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}]},
		{"type":"function_call","call_id":"toolu_A","name":"bash","arguments":"{}"}
	]`)
	next := []byte(`{"type":"response.create","model":"claude-opus-5-5","stream":true,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
		{"type":"function_call","call_id":"toolu_A","name":"bash","arguments":"{}"},
		{"type":"function_call_output","call_id":"toolu_A","output":"ok"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"and now?"}]}
	]}`)

	normalized, _, errMsg := normalizeResponsesWebsocketRequest(next, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}

	items := gjson.GetBytes(normalized, "input").Array()
	for i, item := range items {
		if item.Get("type").String() != "function_call" {
			continue
		}
		if i+1 >= len(items) || items[i+1].Get("type").String() != "function_call_output" {
			t.Fatalf("input.%d is a function_call not immediately followed by its function_call_output: %s", i, gjson.GetBytes(normalized, "input").Raw)
		}
	}
}

// A genuine delta stays a delta: without model output in the input there is
// nothing that could have been a replacement.
func TestNormalizeResponsesWebsocketRequestKeepsIncrementalAppend(t *testing.T) {
	create := []byte(`{"type":"response.create","model":"claude-opus-5-5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}`)
	_, lastRequest, _ := normalizeResponsesWebsocketRequest(create, nil, nil)
	lastResponseOutput := []byte(`[{"type":"function_call","call_id":"toolu_A","name":"bash","arguments":"{}"}]`)
	// The delta the client sends after running the tool itself.
	next := []byte(`{"type":"response.create","model":"claude-opus-5-5","stream":true,"input":[{"type":"function_call_output","call_id":"toolu_A","output":"ok"}]}`)

	normalized, _, errMsg := normalizeResponsesWebsocketRequest(next, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	// The previous turn's call and this turn's output both have to be present,
	// in that order.
	items := gjson.GetBytes(normalized, "input").Array()
	var seq []string
	for _, item := range items {
		itemType := item.Get("type").String()
		if itemType == "" {
			itemType = "message"
		}
		seq = append(seq, itemType)
	}
	want := []string{"message", "function_call", "function_call_output"}
	if len(seq) != len(want) {
		t.Fatalf("input = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("input = %v, want %v", seq, want)
		}
	}
}

func TestShouldReplaceWebsocketTranscript(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		input string
		want  bool
	}{
		{"function call", `{"type":"response.create"}`, `[{"type":"function_call","call_id":"c"}]`, true},
		{"assistant message", `{"type":"response.create"}`, `[{"type":"message","role":"assistant"}]`, true},
		{"custom tool call", `{"type":"response.create"}`, `[{"type":"custom_tool_call","call_id":"c"}]`, true},
		{"append with function call", `{"type":"response.append"}`, `[{"type":"function_call","call_id":"c"}]`, true},
		{"developer message in a delta", `{"type":"response.create"}`, `[{"type":"message","role":"developer"},{"type":"message","role":"user"}]`, false},
		{"delta only", `{"type":"response.create"}`, `[{"type":"function_call_output","call_id":"c"}]`, false},
		{"user message only", `{"type":"response.create"}`, `[{"type":"message","role":"user"}]`, false},
		{"with parent reference", `{"type":"response.create","previous_response_id":"resp_1"}`, `[{"type":"function_call","call_id":"c"}]`, false},
		{"unsupported request type", `{"type":"response.cancel"}`, `[{"type":"function_call","call_id":"c"}]`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := shouldReplaceWebsocketTranscript([]byte(testCase.raw), gjson.Parse(testCase.input))
			if got != testCase.want {
				t.Fatalf("shouldReplaceWebsocketTranscript() = %v, want %v", got, testCase.want)
			}
		})
	}
}
