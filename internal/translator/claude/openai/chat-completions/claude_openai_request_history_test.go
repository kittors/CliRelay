package chat_completions

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// longToolHistoryRequest is an agent's Chat history: a system message, then
// calls assistant tool calls each answered by a tool result of textBytes bytes.
func longToolHistoryRequest(calls, textBytes int) []byte {
	messages := []any{map[string]any{"role": "system", "content": "Synthetic system instruction."}}
	for i := 0; i < calls; i++ {
		id := fmt.Sprint("call_", i)
		messages = append(messages,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "inspect", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": strings.Repeat("x", textBytes)},
		)
	}
	body, _ := json.Marshal(map[string]any{"messages": messages, "max_completion_tokens": 16384, "reasoning_effort": "medium"})
	return body
}

// allocatedBytes reports what one conversion allocates. TotalAlloc only grows,
// so a collection during the call does not hide anything.
func allocatedBytes(t *testing.T, body []byte) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out := ConvertOpenAIRequestToClaude("claude-opus-5-5", body, true)
	runtime.ReadMemStats(&after)
	if got, want := gjson.GetBytes(out, "messages.#").Int(), gjson.GetBytes(body, "messages.#").Int(); got != want {
		t.Fatalf("converted %d messages, want %d", got, want)
	}
	return after.TotalAlloc - before.TotalAlloc
}

// Appending each message with sjson copied the whole request built so far, so
// allocations grew with the square of the history: 512 tool calls and results
// cost about 2.7 GB (#1179). With messages serialized on their own and written
// into the request once, four times the history allocates about four times as
// much; the old copies made it about fifteen times.
func TestConvertOpenAIRequestToClaude_LongHistoryAllocationsGrowLinearly(t *testing.T) {
	allocatedBytes(t, longToolHistoryRequest(1, 16)) // first call sets up the process-wide user ID
	small := allocatedBytes(t, longToolHistoryRequest(64, 1024))
	large := allocatedBytes(t, longToolHistoryRequest(256, 1024))
	if ratio := float64(large) / float64(small); ratio > 8 {
		t.Fatalf("4x the history allocated %.1fx as much (%d -> %d bytes), want linear growth (about 4x)", ratio, small, large)
	}
}

// Every system message is folded, in order, into the slot of the first one,
// while the other messages keep their own order.
func TestConvertOpenAIRequestToClaude_SystemMessagesShareFirstSlot(t *testing.T) {
	input := []byte(`{"model":"claude-opus-5-5","messages":[
		{"role":"user","content":"before"},
		{"role":"system","content":"first"},
		{"role":"assistant","content":"between"},
		{"role":"system","content":[{"type":"text","text":"second"}]},
		{"role":"tool","tool_call_id":"call_1","content":"result"},
		{"role":"system","content":"third"}
	]}`)

	msgs := gjson.GetBytes(ConvertOpenAIRequestToClaude("claude-opus-5-5", input, false), "messages").Array()
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4", len(msgs))
	}
	if got := msgs[0].Get("content.0.text").String(); got != "before" {
		t.Errorf("messages.0 text = %q, want before", got)
	}
	var system []string
	msgs[1].Get("content").ForEach(func(_, part gjson.Result) bool {
		system = append(system, part.Get("text").String())
		return true
	})
	if got := strings.Join(system, ","); msgs[1].Get("role").String() != "user" || got != "first,second,third" {
		t.Errorf("messages.1 = %s, want the user slot holding first,second,third", msgs[1].Raw)
	}
	if got := msgs[2].Get("content.0.text").String(); got != "between" {
		t.Errorf("messages.2 text = %q, want between", got)
	}
	if got := msgs[3].Get("content.0.type").String(); got != "tool_result" {
		t.Errorf("messages.3 type = %q, want tool_result", got)
	}
}

func BenchmarkConvertOpenAIRequestToClaude_LongToolHistory(b *testing.B) {
	for _, calls := range []int{32, 128, 512} {
		body := longToolHistoryRequest(calls, 4096)
		b.Run(fmt.Sprint(calls), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				_ = ConvertOpenAIRequestToClaude("claude-opus-5-5", body, true)
			}
		})
	}
}
