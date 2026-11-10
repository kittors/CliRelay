package chat_completions

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/tidwall/gjson"
)

func claudeChatEvent(t *testing.T, state *any, event string) []string {
	t.Helper()
	return ConvertClaudeResponseToOpenAI(context.Background(), "claude-test", nil, nil, []byte("data: "+event), state)
}

func claudeToolUseStart(index int, id, name string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, id, name)
}

func claudeInputJSONDelta(index int, partial string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, strconv.Quote(partial))
}

func claudeBlockStop(index int) string {
	return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
}

// The call goes out at content_block_start, before Claude has generated any of
// its arguments. As in OpenAI's stream, the first chunk carries the index, id,
// type and name with empty arguments, and every input_json_delta fragment then
// follows as a chunk with only the index and those argument bytes. Before,
// nothing was sent until content_block_stop, so a client waiting on a long
// tool call saw no progress at all (#1177).
func TestClaudeChatToolStream_ArgumentsStreamAsGenerated(t *testing.T) {
	var state any
	start := claudeChatEvent(t, &state, claudeToolUseStart(2, "toolu_2", "shell"))
	if len(start) != 1 {
		t.Fatalf("content_block_start sent %d chunks, want 1", len(start))
	}
	call := gjson.Get(start[0], "choices.0.delta.tool_calls.0")
	if call.Get("index").Int() != 2 || call.Get("id").String() != "toolu_2" || call.Get("type").String() != "function" || call.Get("function.name").String() != "shell" {
		t.Fatalf("first chunk = %s, want index 2 with the id, type and name", call.Raw)
	}
	if args := call.Get("function.arguments"); args.Type != gjson.String || args.String() != "" {
		t.Fatalf("first chunk arguments = %s, want an empty string", args.Raw)
	}

	assembled := ""
	for _, partial := range []string{`{"command":`, `"printf ok"}`} {
		chunks := claudeChatEvent(t, &state, claudeInputJSONDelta(2, partial))
		if len(chunks) != 1 {
			t.Fatalf("input_json_delta %q sent %d chunks, want 1", partial, len(chunks))
		}
		delta := gjson.Get(chunks[0], "choices.0.delta.tool_calls.0")
		if delta.Get("index").Int() != 2 || delta.Get("id").Exists() || delta.Get("type").Exists() || delta.Get("function.name").Exists() {
			t.Fatalf("argument chunk = %s, want only the index and the arguments", delta.Raw)
		}
		assembled += delta.Get("function.arguments").String()
	}
	if assembled != `{"command":"printf ok"}` {
		t.Fatalf("assembled arguments = %q", assembled)
	}
	if stop := claudeChatEvent(t, &state, claudeBlockStop(2)); len(stop) != 0 {
		t.Fatalf("content_block_stop sent %v, want nothing: the call is already complete", stop)
	}
}

// Anthropic opens the arguments with an empty partial_json; it carries no
// bytes, so no chunk is sent for it.
func TestClaudeChatToolStream_EmptyFragmentSendsNothing(t *testing.T) {
	var state any
	claudeChatEvent(t, &state, claudeToolUseStart(0, "toolu_0", "shell"))
	if chunks := claudeChatEvent(t, &state, claudeInputJSONDelta(0, "")); len(chunks) != 0 {
		t.Fatalf("empty fragment sent %v, want nothing", chunks)
	}
}

// A call that got no arguments must still read as an empty object, so
// content_block_stop completes it with {} once, without repeating the id or
// name already sent.
func TestClaudeChatToolStream_NoArgumentsCompleteWithEmptyObject(t *testing.T) {
	var state any
	claudeChatEvent(t, &state, claudeToolUseStart(0, "toolu_empty", "clock"))
	stop := claudeChatEvent(t, &state, claudeBlockStop(0))
	if len(stop) != 1 {
		t.Fatalf("content_block_stop sent %d chunks, want 1", len(stop))
	}
	delta := gjson.Get(stop[0], "choices.0.delta.tool_calls.0")
	if delta.Get("index").Int() != 0 || delta.Get("function.arguments").String() != "{}" || delta.Get("id").Exists() || delta.Get("function.name").Exists() {
		t.Fatalf("completion chunk = %s, want index 0 with arguments {} only", delta.Raw)
	}
	if again := claudeChatEvent(t, &state, claudeBlockStop(0)); len(again) != 0 {
		t.Fatalf("a repeated content_block_stop sent %v, want nothing", again)
	}
}

// Parallel calls keep their own indexes, so their fragments never mix.
func TestClaudeChatToolStream_ParallelCallsKeepTheirIndexes(t *testing.T) {
	var state any
	for index, tool := range []struct{ id, name string }{{"toolu_a", "first"}, {"toolu_b", "second"}} {
		chunks := claudeChatEvent(t, &state, claudeToolUseStart(index, tool.id, tool.name))
		if len(chunks) != 1 || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.index").Int() != int64(index) || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.id").String() != tool.id {
			t.Fatalf("tool %d start = %v", index, chunks)
		}
	}
	for index, partial := range []string{`{"a":1}`, `{"b":2}`} {
		chunks := claudeChatEvent(t, &state, claudeInputJSONDelta(index, partial))
		if len(chunks) != 1 || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.index").Int() != int64(index) || gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.function.arguments").String() != partial {
			t.Fatalf("tool %d fragment = %v", index, chunks)
		}
	}
	for index := range 2 {
		if stop := claudeChatEvent(t, &state, claudeBlockStop(index)); len(stop) != 0 {
			t.Fatalf("tool %d stop sent %v, want nothing", index, stop)
		}
	}
}

// Without an id or a name the call cannot be announced, so it is still sent
// whole at content_block_stop, as before.
func TestClaudeChatToolStream_IncompleteMetadataSentWholeAtStop(t *testing.T) {
	var state any
	if start := claudeChatEvent(t, &state, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_no_name","input":{}}}`); len(start) != 0 {
		t.Fatalf("a call without a name was announced early: %v", start)
	}
	if delta := claudeChatEvent(t, &state, claudeInputJSONDelta(0, `{"x":1}`)); len(delta) != 0 {
		t.Fatalf("arguments were sent before the call was announced: %v", delta)
	}
	stop := claudeChatEvent(t, &state, claudeBlockStop(0))
	if len(stop) != 1 {
		t.Fatalf("content_block_stop sent %d chunks, want 1", len(stop))
	}
	call := gjson.Get(stop[0], "choices.0.delta.tool_calls.0")
	if call.Get("id").String() != "toolu_no_name" || call.Get("type").String() != "function" || call.Get("function.arguments").String() != `{"x":1}` {
		t.Fatalf("whole call = %s, want the buffered call as before", call.Raw)
	}
}

// A client that puts the chunks together the OpenAI way (strings appended per
// tool call index) ends up with the same calls the buffered stream sent in one
// piece, and finish_reason tool_calls.
func TestClaudeChatToolStream_ClientAssemblesTheSameCalls(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me check."}}`,
		claudeBlockStop(0),
		claudeToolUseStart(1, "toolu_a", "shell"),
		claudeInputJSONDelta(1, ""),
		claudeInputJSONDelta(1, `{"command":`),
		claudeInputJSONDelta(1, `"ls -la"}`),
		claudeBlockStop(1),
		claudeToolUseStart(2, "toolu_b", "clock"),
		claudeBlockStop(2),
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`,
		`{"type":"message_stop"}`,
	}
	type assembledCall struct{ id, typ, name, arguments string }
	calls := map[int64]*assembledCall{}
	content, finishReason := "", ""
	var state any
	for _, event := range events {
		for _, chunk := range claudeChatEvent(t, &state, event) {
			choice := gjson.Get(chunk, "choices.0")
			content += choice.Get("delta.content").String()
			if reason := choice.Get("finish_reason"); reason.Type == gjson.String {
				finishReason = reason.String()
			}
			choice.Get("delta.tool_calls").ForEach(func(_, tc gjson.Result) bool {
				call := calls[tc.Get("index").Int()]
				if call == nil {
					call = &assembledCall{}
					calls[tc.Get("index").Int()] = call
				}
				call.id += tc.Get("id").String()
				call.typ += tc.Get("type").String()
				call.name += tc.Get("function.name").String()
				call.arguments += tc.Get("function.arguments").String()
				return true
			})
		}
	}
	if content != "Let me check." || finishReason != "tool_calls" {
		t.Fatalf("content = %q, finish_reason = %q", content, finishReason)
	}
	want := map[int64]assembledCall{
		1: {"toolu_a", "function", "shell", `{"command":"ls -la"}`},
		2: {"toolu_b", "function", "clock", "{}"},
	}
	if len(calls) != len(want) {
		t.Fatalf("assembled %d calls, want %d", len(calls), len(want))
	}
	for index, w := range want {
		if got := calls[index]; got == nil || *got != w {
			t.Errorf("call %d = %+v, want %+v", index, got, w)
		}
	}
}
