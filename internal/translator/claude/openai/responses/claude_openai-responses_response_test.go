package responses

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claudeStreamBlock is one Claude content block fed to the stream converter.
type claudeStreamBlock struct {
	kind   string   // text | thinking | redacted_thinking | tool_use
	chunks []string // delta payloads in arrival order: text, thinking or partial_json
}

// claudeStreamLines renders blocks as the raw SSE lines ClaudeExecutor hands the
// converter one at a time, including the event: and blank lines it must skip.
func claudeStreamLines(t *testing.T, blocks []claudeStreamBlock, stopReason string) []string {
	t.Helper()
	var lines []string
	emit := func(event, data string) {
		lines = append(lines, "event: "+event, "data: "+data, "")
	}
	emit("message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":1}}}`)
	emit("ping", `{"type":"ping"}`)
	for i, b := range blocks {
		var start, deltaType, deltaField string
		switch b.kind {
		case "text":
			start, deltaType, deltaField = `{"type":"text","text":""}`, "text_delta", "text"
		case "thinking":
			start, deltaType, deltaField = `{"type":"thinking","thinking":"","signature":""}`, "thinking_delta", "thinking"
		case "redacted_thinking":
			start = `{"type":"redacted_thinking","data":"EmwKAhgBEgyRedactedPayload"}`
		case "tool_use":
			start = fmt.Sprintf(`{"type":"tool_use","id":"toolu_%02d","name":"shell","input":{}}`, i)
			deltaType, deltaField = "input_json_delta", "partial_json"
		default:
			t.Fatalf("unknown block kind %q", b.kind)
		}
		emit("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, start))
		for _, chunk := range b.chunks {
			delta := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":%q}}`, i, deltaType)
			delta, _ = sjson.Set(delta, "delta."+deltaField, chunk)
			emit("content_block_delta", delta)
		}
		if b.kind == "thinking" {
			emit("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":"EqQBCgIYAhIM"}}`, i))
		}
		emit("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
	}
	emit("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":42}}`, stopReason))
	emit("message_stop", `{"type":"message_stop"}`)
	return lines
}

type responsesStreamEvent struct {
	name string
	data gjson.Result
}

func convertClaudeStream(t *testing.T, lines []string) []responsesStreamEvent {
	t.Helper()
	req := []byte(`{"model":"claude-opus-4-6","stream":true,"input":"hi"}`)
	var param any
	var events []responsesStreamEvent
	for _, line := range lines {
		for _, chunk := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-opus-4-6", req, req, []byte(line), &param) {
			head, payload, ok := strings.Cut(chunk, "\n")
			payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
			if !ok || !gjson.Valid(payload) {
				t.Fatalf("malformed SSE chunk: %q", chunk)
			}
			name := strings.TrimSpace(strings.TrimPrefix(head, "event:"))
			events = append(events, responsesStreamEvent{name: name, data: gjson.Parse(payload)})
		}
	}
	return events
}

// messageDoneTexts returns, one entry per closed text block, the text carried by
// each of the three message-level done events.
func messageDoneTexts(events []responsesStreamEvent) (textDone, partDone, itemDone []string) {
	for _, ev := range events {
		switch ev.name {
		case "response.output_text.done":
			textDone = append(textDone, ev.data.Get("text").String())
		case "response.content_part.done":
			partDone = append(partDone, ev.data.Get("part.text").String())
		case "response.output_item.done":
			if ev.data.Get("item.type").String() == "message" {
				itemDone = append(itemDone, ev.data.Get("item.content.0.text").String())
			}
		}
	}
	return textDone, partDone, itemDone
}

// completedOutput summarizes response.completed.output, one entry per item.
func completedOutput(t *testing.T, events []responsesStreamEvent) []string {
	t.Helper()
	var completed []gjson.Result
	for _, ev := range events {
		if ev.name == "response.completed" {
			completed = append(completed, ev.data)
		}
	}
	if len(completed) != 1 {
		t.Fatalf("got %d response.completed events, want 1", len(completed))
	}
	var items []string
	completed[0].Get("response.output").ForEach(func(_, item gjson.Result) bool {
		switch typ := item.Get("type").String(); typ {
		case "message":
			items = append(items, fmt.Sprintf("message %s %q", item.Get("id").String(), item.Get("content.0.text").String()))
		case "reasoning":
			items = append(items, fmt.Sprintf("reasoning %s %q", item.Get("id").String(), item.Get("summary.0.text").String()))
		case "function_call":
			items = append(items, fmt.Sprintf("function_call %s %s", item.Get("call_id").String(), item.Get("arguments").String()))
		default:
			items = append(items, typ)
		}
		return true
	})
	return items
}

// Codex CLI takes the final assistant message, and the history item it replays on
// the next turn, from response.output_item.done instead of the deltas, so every
// message-level done event must carry the text of the block it closes (#1053).
func TestConvertClaudeResponseToOpenAIResponses_DoneEventsCarryBlockText(t *testing.T) {
	const codeReply = "```go\nfmt.Println(\"a\\tb\")\n```\n<ok> & done"
	cases := []struct {
		name   string
		blocks []claudeStreamBlock
		stop   string
		// wantDone is the text of each text block, in order.
		wantDone []string
		// wantCompleted is response.completed.output as it was before #1053; the
		// fix must leave it alone. Folding every text block into one message
		// ahead of the tool calls is a separate known issue: when that is fixed,
		// update these expectations with it.
		wantCompleted []string
	}{
		{
			name:          "single text block",
			blocks:        []claudeStreamBlock{{kind: "text", chunks: []string{"Hello", ", ", "world!"}}},
			stop:          "end_turn",
			wantDone:      []string{"Hello, world!"},
			wantCompleted: []string{`message msg_msg_01_0 "Hello, world!"`},
		},
		{
			name: "multiple text blocks each carry only their own text",
			blocks: []claudeStreamBlock{
				{kind: "text", chunks: []string{"First ", "block."}},
				{kind: "text", chunks: []string{"Second ", "block."}},
				{kind: "text", chunks: []string{"Third."}},
			},
			stop:          "end_turn",
			wantDone:      []string{"First block.", "Second block.", "Third."},
			wantCompleted: []string{`message msg_msg_01_0 "First block.Second block.Third."`},
		},
		{
			name: "utf-8 multi-byte text across blocks",
			blocks: []claudeStreamBlock{
				{kind: "text", chunks: []string{"Héllo, ", "世界", " 🌍!"}},
				{kind: "text", chunks: []string{"第二段：", "emoji 🚀 ok"}},
			},
			stop:          "end_turn",
			wantDone:      []string{"Héllo, 世界 🌍!", "第二段：emoji 🚀 ok"},
			wantCompleted: []string{`message msg_msg_01_0 "Héllo, 世界 🌍!第二段：emoji 🚀 ok"`},
		},
		{
			name:          "code with newlines, quotes and backslashes",
			blocks:        []claudeStreamBlock{{kind: "text", chunks: []string{"```go\n", "fmt.Println(\"a\\tb\")\n", "```\n<ok> & done"}}},
			stop:          "end_turn",
			wantDone:      []string{codeReply},
			wantCompleted: []string{fmt.Sprintf("message msg_msg_01_0 %q", codeReply)},
		},
		{
			name: "empty text block between text blocks",
			blocks: []claudeStreamBlock{
				{kind: "text", chunks: []string{"AAA"}},
				{kind: "text"},
				{kind: "text", chunks: []string{"BBB"}},
			},
			stop:          "end_turn",
			wantDone:      []string{"AAA", "", "BBB"},
			wantCompleted: []string{`message msg_msg_01_0 "AAABBB"`},
		},
		{
			name: "thinking then text",
			blocks: []claudeStreamBlock{
				{kind: "thinking", chunks: []string{"Let me ", "think."}},
				{kind: "text", chunks: []string{"The answer ", "is 42."}},
			},
			stop:     "end_turn",
			wantDone: []string{"The answer is 42."},
			wantCompleted: []string{
				`reasoning rs_msg_01_0 "Let me think."`,
				`message msg_msg_01_0 "The answer is 42."`,
			},
		},
		{
			name: "redacted_thinking then text",
			blocks: []claudeStreamBlock{
				{kind: "redacted_thinking"},
				{kind: "text", chunks: []string{"Still ", "here."}},
			},
			stop:          "end_turn",
			wantDone:      []string{"Still here."},
			wantCompleted: []string{`message msg_msg_01_0 "Still here."`},
		},
		{
			name: "text interleaved with tool_use",
			blocks: []claudeStreamBlock{
				{kind: "text", chunks: []string{"I'll run ", "ls."}},
				{kind: "tool_use", chunks: []string{`{"command":`, `["ls"]}`}},
				{kind: "text", chunks: []string{"Then git ", "status."}},
				{kind: "tool_use", chunks: []string{`{"command":["git","status"]}`}},
			},
			stop:     "tool_use",
			wantDone: []string{"I'll run ls.", "Then git status."},
			wantCompleted: []string{
				`message msg_msg_01_0 "I'll run ls.Then git status."`,
				`function_call toolu_01 {"command":["ls"]}`,
				`function_call toolu_03 {"command":["git","status"]}`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := convertClaudeStream(t, claudeStreamLines(t, tc.blocks, tc.stop))

			textDone, partDone, itemDone := messageDoneTexts(events)
			for _, done := range []struct {
				field string
				got   []string
			}{
				{"response.output_text.done text", textDone},
				{"response.content_part.done part.text", partDone},
				{"response.output_item.done item.content[0].text", itemDone},
			} {
				if !slices.Equal(done.got, tc.wantDone) {
					t.Errorf("%s = %q, want %q", done.field, done.got, tc.wantDone)
				}
			}

			if got := completedOutput(t, events); !slices.Equal(got, tc.wantCompleted) {
				t.Errorf("response.completed output = %q, want %q", got, tc.wantCompleted)
			}
		})
	}
}
