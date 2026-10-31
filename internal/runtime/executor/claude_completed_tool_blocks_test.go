package executor

import (
	"context"
	"reflect"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// newClaudeToOpenAIStreamFeed returns a function that feeds one Claude SSE
// event through the registered Claude -> OpenAI Chat stream translator, with
// or without completed tool-block negotiation, sharing translator state.
func newClaudeToOpenAIStreamFeed(enabled bool) func(event string) []string {
	var param any
	translate := func(raw []byte) []string {
		return sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "fixture", nil, nil, raw, &param)
	}
	return func(event string) []string {
		line := []byte("data: " + event)
		if enabled {
			return translateClaudeLineWithCompletedToolBlocks(line, translate)
		}
		return translate(line)
	}
}

func TestNegotiatedClaudeBlockCompletion(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		feed := newClaudeToOpenAIStreamFeed(enabled)
		feed(`{"type":"message_start","message":{"id":"msg_fixture"}}`)
		feed(`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_fixture","name":"shell","input":{}}}`)
		feed(`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"pwd\"}"}}`)
		chunks := feed(`{"type":"content_block_stop","index":2}`)
		want := 1
		if enabled {
			want = 2
		}
		if len(chunks) != want {
			t.Fatalf("enabled=%v: expected %d chunks, got %d", enabled, want, len(chunks))
		}
		if gjson.Get(chunks[0], "choices.0.delta.tool_calls.0.function.arguments").String() != `{"cmd":"pwd"}` {
			t.Fatal("arguments changed")
		}
		if enabled {
			call := gjson.Get(chunks[1], "choices.0.delta.tool_calls.0")
			if call.Get("index").Int() != 2 || call.Get("id").String() != "call_fixture" || call.Get("x_tool_completed").String() != sdktranslator.CompletedToolBlocksVersion || call.Get("function").Exists() {
				t.Fatal("marker did not bind exact completed call")
			}
			if gjson.Get(chunks[1], "id").String() != gjson.Get(chunks[0], "id").String() || gjson.Get(chunks[1], "created").Int() != gjson.Get(chunks[0], "created").Int() {
				t.Fatal("marker envelope differs from the completed tool-call chunk")
			}
		}
		if len(feed(`{"type":"content_block_stop","index":2}`)) != 0 {
			t.Fatal("duplicate completion output")
		}
	}
}

func TestCompletedToolBlocksIgnoreMalformedStopIndex(t *testing.T) {
	for _, index := range []string{"null", "-1", "0.5", `"0"`, "9007199254740992"} {
		feed := newClaudeToOpenAIStreamFeed(true)
		feed(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_fixture","name":"shell","input":{}}}`)
		if len(feed(`{"type":"content_block_stop","index":`+index+`}`)) != 0 {
			t.Fatalf("invalid index %s authorized completion", index)
		}
	}
}

func TestCompletedToolBlocksLeaveNonToolEventsUnchanged(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_fixture"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
	plain := newClaudeToOpenAIStreamFeed(false)
	negotiated := newClaudeToOpenAIStreamFeed(true)
	for _, event := range events {
		a, b := plain(event), negotiated(event)
		if len(a) != len(b) {
			t.Fatalf("event %s: chunk count %d vs %d", event, len(a), len(b))
		}
		for i := range a {
			// created timestamps may tick between the two feeds; compare the rest.
			if !reflect.DeepEqual(gjson.Get(a[i], "choices").Value(), gjson.Get(b[i], "choices").Value()) {
				t.Fatalf("event %s: negotiated output changed: %s vs %s", event, a[i], b[i])
			}
		}
	}
}
