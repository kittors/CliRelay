package responses

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesRequestPreservesReadableReasoning(t *testing.T) {
	const reason = `{"type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"思考\n\"quoted\""}],"encrypted_content":"opaque-secret"}`
	const call = `{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"q\":1}"}`
	const result = `{"type":"function_call_output","call_id":"c1","output":"found"}`
	const assistant = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`
	const user = `{"type":"message","role":"user","content":"next"}`
	for _, tc := range []struct {
		name        string
		input       string
		count       int64
		reasonIndex int
		check       func(*testing.T, gjson.Result)
	}{
		{"tool history", reason + "," + call + "," + result, 2, 0, func(t *testing.T, out gjson.Result) {
			if out.Get("messages.0.tool_calls.0.id").String() != "c1" || out.Get("messages.1.tool_call_id").String() != "c1" || out.Get("messages.1.content").String() != "found" {
				t.Fatalf("tool history changed: %s", out.Raw)
			}
		}},
		{"assistant text", reason + "," + assistant, 1, 0, func(t *testing.T, out gjson.Result) {
			if out.Get("messages.0.content.0.text").String() != "answer" {
				t.Fatalf("assistant text changed: %s", out.Raw)
			}
		}},
		{"user boundary", reason + "," + user + "," + assistant, 3, 0, nil},
		{"trailing reasoning", user + "," + reason, 2, 1, nil},
		{"tool output boundary", call + "," + result + "," + reason + "," + user, 4, 2, nil},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				input := []byte(`{"model":"m","input":[` + tc.input + `]}`)
				before := string(input)
				out := gjson.ParseBytes(ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", input, stream))
				if string(input) != before {
					t.Fatal("input mutated")
				}
				if out.Get("messages.#").Int() != tc.count || out.Get("stream").Bool() != stream {
					t.Fatalf("message count/stream changed: %s", out.Raw)
				}
				path := fmt.Sprintf("messages.%d", tc.reasonIndex)
				if out.Get(path+".role").String() != "assistant" || out.Get(path+".reasoning_content").String() != "first\n思考\n\"quoted\"" {
					t.Fatalf("reasoning missing from assistant: %s", out.Raw)
				}
				out.Get("messages").ForEach(func(_, message gjson.Result) bool {
					if message.Get("role").String() != "assistant" && message.Get("reasoning_content").Exists() {
						t.Errorf("reasoning attached to a non-assistant: %s", message.Raw)
					}
					return true
				})
				if strings.Contains(out.Raw, "opaque-secret") {
					t.Fatal("opaque reasoning leaked into plaintext")
				}
				if tc.check != nil {
					tc.check(t, out)
				}
			})
		}
	}
}

func TestResponsesRequestIgnoresOpaqueAndMalformedReasoning(t *testing.T) {
	for _, item := range []string{
		`{"type":"reasoning","encrypted_content":"opaque-secret"}`,
		`{"type":"reasoning","summary":[]}`,
		`{"type":"reasoning","summary":[{"type":"summary_text","text":""}]}`,
		`{"type":"reasoning","summary":[{"type":"summary_text","text":123}]}`,
		`{"type":"reasoning","summary":[{"type":"other","text":"unrelated"}]}`,
	} {
		input := []byte(`{"input":[` + item + `,{"role":"user","content":"hello"}]}`)
		out := gjson.ParseBytes(ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", input, false))
		if out.Get("messages.#").Int() != 1 || strings.Contains(out.Raw, "reasoning_content") || strings.Contains(out.Raw, "opaque-secret") {
			t.Fatalf("unexpected reasoning message: %s", out.Raw)
		}
	}
}

func TestResponsesRequestCombinesAdjacentReasoning(t *testing.T) {
	input := []byte(`{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"one"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"two"}]},{"type":"function_call","call_id":"a","name":"first","arguments":"{}"},{"type":"function_call","call_id":"b","name":"second","arguments":"{}"}]}`)
	out := gjson.ParseBytes(ConvertOpenAIResponsesRequestToOpenAIChatCompletions("m", input, false))
	if out.Get("messages.#").Int() != 2 || out.Get("messages.0.reasoning_content").String() != "one\ntwo" || out.Get("messages.1.reasoning_content").Exists() {
		t.Fatalf("reasoning duplicated or discarded: %s", out.Raw)
	}
}

func TestResponsesStreamUsesDistinctStableOutputIndices(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice int
		deltas []string
		types  string
	}{
		{"reason then text", 0, []string{`{"reasoning_content":"one "}`, `{"reasoning_content":"思考"}`, `{"content":"answer"}`}, "reasoning,message"},
		{"reason only", 0, []string{`{"reasoning_content":"one"}`}, "reasoning"},
		{"text only nonzero choice", 3, []string{`{"content":"a"}`, `{"content":"b"}`}, "message"},
		{"parallel tools", 0, []string{`{"tool_calls":[{"index":3,"id":"c3","function":{"name":"three","arguments":"{"}},{"index":1,"id":"c1","function":{"name":"one","arguments":"{"}}]}`, `{"tool_calls":[{"index":1,"function":{"arguments":"}"}},{"index":3,"function":{"arguments":"}"}}]}`}, "function_call,function_call"},
		{"reason then parallel tools", 0, []string{`{"reasoning_content":"tools"}`, `{"tool_calls":[{"index":0,"id":"c0","function":{"name":"zero","arguments":"{}"}},{"index":1,"id":"c1","function":{"name":"one","arguments":"{}"}}]}`}, "reasoning,function_call,function_call"},
		{"mixed chunk", 0, []string{`{"reasoning_content":"think","content":"answer","tool_calls":[{"index":0,"id":"c0","function":{"name":"zero","arguments":"{}"}}]}`}, "reasoning,message,function_call"},
		{"reasoning resumes", 0, []string{`{"reasoning_content":"one"}`, `{"content":"answer"}`, `{"reasoning_content":"two"}`}, "reasoning,message,reasoning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var param any
			var events []gjson.Result
			for i, delta := range append(tc.deltas, `{}`) {
				finish := "null"
				if i == len(tc.deltas) {
					finish = `"stop"`
				}
				chunk := fmt.Sprintf(`{"id":"test","object":"chat.completion.chunk","choices":[{"index":%d,"delta":%s,"finish_reason":%s}]}`, tc.choice, delta, finish)
				for _, event := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "m", nil, nil, []byte(chunk), &param) {
					_, data, ok := strings.Cut(event, "\ndata: ")
					if !ok || !gjson.Valid(data) {
						t.Fatalf("invalid event: %s", event)
					}
					events = append(events, gjson.Parse(data))
				}
			}
			added := map[int64]gjson.Result{}
			ids := map[string]int64{}
			done := map[string]int{}
			reasoning := map[string]string{}
			var completed gjson.Result
			for i, event := range events {
				if event.Get("sequence_number").Int() != int64(i+1) {
					t.Fatalf("sequence changed: %s", event.Raw)
				}
				kind := event.Get("type").String()
				id := event.Get("item_id").String()
				if kind == "response.output_item.added" || kind == "response.output_item.done" {
					id = event.Get("item.id").String()
				}
				index := event.Get("output_index").Int()
				if kind == "response.output_item.added" {
					if _, exists := added[index]; exists {
						t.Fatalf("reused output index %d: %s", index, event.Raw)
					}
					if _, exists := ids[id]; exists {
						t.Fatalf("reused item id %s", id)
					}
					added[index] = event.Get("item")
					ids[id] = index
				}
				if event.Get("output_index").Exists() {
					want, exists := ids[id]
					if !exists || want != index {
						t.Fatalf("item index unstable/unannounced: %s", event.Raw)
					}
				}
				if kind == "response.reasoning_summary_text.delta" {
					reasoning[id] += event.Get("delta").String()
				}
				if kind == "response.output_item.done" {
					done[id]++
					if event.Get("item.type").String() == "reasoning" && event.Get("item.summary.0.text").String() != reasoning[id] {
						t.Fatalf("reasoning summary lost: %s", event.Raw)
					}
				}
				if kind == "response.completed" {
					if completed.Exists() {
						t.Fatal("duplicate completion")
					}
					completed = event.Get("response")
				}
			}
			output := completed.Get("output").Array()
			if len(output) != len(added) {
				t.Fatalf("completed output size differs: %s", completed.Raw)
			}
			var types []string
			for i, item := range output {
				id := item.Get("id").String()
				if added[int64(i)].Get("id").String() != id || done[id] != 1 {
					t.Fatalf("completed item order/lifecycle differs at %d: %s", i, item.Raw)
				}
				if item.Get("type").String() == "reasoning" && item.Get("summary.0.text").String() != reasoning[id] {
					t.Fatalf("completed reasoning lost: %s", item.Raw)
				}
				types = append(types, item.Get("type").String())
			}
			if strings.Join(types, ",") != tc.types {
				t.Fatalf("item order = %v, want %s", types, tc.types)
			}
		})
	}
}
