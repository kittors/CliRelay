package claude

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// These tests read the translated stream event by event, the way an
// Anthropic client assembles it, and hold it to the order Anthropic's
// streaming documentation gives: message_start; content blocks one after
// another, each content_block_start, its deltas, content_block_stop, with
// indexes counting up from 0; then message_delta and message_stop.

type claudeEvent struct {
	name string
	data gjson.Result
}

// streamedBlock is a content block as a client assembles it.
type streamedBlock struct {
	kind, id, name string
	content        string // text, thinking, or the joined partial_json
	deltas         int
}

// translateChunks feeds chunks to the translator one at a time and returns
// the events each produced.
func translateChunks(chunks ...string) [][]string {
	var param any
	perChunk := make([][]string, 0, len(chunks))
	for _, chunk := range chunks {
		perChunk = append(perChunk, ConvertOpenAIResponseToClaude(context.Background(), "m", []byte(`{"stream":true}`), nil, []byte(chunk), &param))
	}
	return perChunk
}

func parseClaudeEvents(t *testing.T, outputs []string) []claudeEvent {
	t.Helper()
	var events []claudeEvent
	for _, output := range outputs {
		for _, segment := range strings.Split(output, "\n\n") {
			if segment = strings.TrimSpace(segment); segment == "" {
				continue
			}
			var name, data string
			for _, line := range strings.Split(segment, "\n") {
				if strings.HasPrefix(line, "event: ") {
					name = strings.TrimPrefix(line, "event: ")
				} else if strings.HasPrefix(line, "data: ") {
					data = strings.TrimPrefix(line, "data: ")
				}
			}
			payload := gjson.Parse(data)
			if !gjson.Valid(data) || payload.Get("type").String() != name {
				t.Fatalf("event %q carries %q", name, data)
			}
			events = append(events, claudeEvent{name: name, data: payload})
		}
	}
	return events
}

// assembleClaudeStream checks the event order and returns the blocks.
func assembleClaudeStream(t *testing.T, perChunk [][]string) []streamedBlock {
	t.Helper()
	var all []string
	for _, outputs := range perChunk {
		all = append(all, outputs...)
	}
	events := parseClaudeEvents(t, all)
	stream := strings.Join(all, "")
	open := -1
	var blocks []streamedBlock
	messageDelta, messageStop := false, false
	for i, event := range events {
		index := int(event.data.Get("index").Int())
		switch event.name {
		case "message_start":
			if i != 0 {
				t.Fatalf("message_start is event %d:\n%s", i, stream)
			}
		case "content_block_start":
			if open != -1 {
				t.Fatalf("block %d opened while block %d is open:\n%s", index, open, stream)
			}
			if messageDelta {
				t.Fatalf("block %d opened after message_delta:\n%s", index, stream)
			}
			if index != len(blocks) {
				t.Fatalf("block index %d, want %d:\n%s", index, len(blocks), stream)
			}
			block := event.data.Get("content_block")
			blocks = append(blocks, streamedBlock{kind: block.Get("type").String(), id: block.Get("id").String(), name: block.Get("name").String()})
			open = index
		case "content_block_delta":
			if index != open {
				t.Fatalf("delta for block %d while block %d is open:\n%s", index, open, stream)
			}
			want := map[string]string{"text_delta": "text", "thinking_delta": "thinking", "input_json_delta": "tool_use"}[event.data.Get("delta.type").String()]
			if want != blocks[index].kind {
				t.Fatalf("%s delta in a %s block:\n%s", event.data.Get("delta.type").String(), blocks[index].kind, stream)
			}
			delta := event.data.Get("delta")
			blocks[index].content += delta.Get("text").String() + delta.Get("thinking").String() + delta.Get("partial_json").String()
			blocks[index].deltas++
		case "content_block_stop":
			if index != open {
				t.Fatalf("stop for block %d while block %d is open:\n%s", index, open, stream)
			}
			open = -1
		case "message_delta":
			if open != -1 {
				t.Fatalf("message_delta while block %d is open:\n%s", open, stream)
			}
			messageDelta = true
		case "message_stop":
			if !messageDelta || i != len(events)-1 {
				t.Fatalf("message_stop must follow message_delta and end the stream:\n%s", stream)
			}
			messageStop = true
		}
	}
	if !messageStop {
		t.Fatalf("stream does not end with message_stop:\n%s", stream)
	}
	return blocks
}

func openAITextChunk(field, text string) string {
	return fmt.Sprintf(`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{%s:%s},"finish_reason":null}]}`, strconv.Quote(field), strconv.Quote(text))
}

func toolChunk(deltas ...streamToolDeltaSpec) string {
	return openAIStreamChunk("c", deltas, "")
}

func startTool(index int, id, name, args string) streamToolDeltaSpec {
	return streamToolDeltaSpec{Index: index, ID: id, HasName: true, Name: name, HasArgs: args != "", Args: args}
}

func toolArgs(index int, args string) streamToolDeltaSpec {
	return streamToolDeltaSpec{Index: index, HasArgs: true, Args: args}
}

func inputJSONDeltas(t *testing.T, outputs []string) []string {
	t.Helper()
	var partials []string
	for _, event := range parseClaudeEvents(t, outputs) {
		if event.data.Get("delta.type").String() == "input_json_delta" {
			partials = append(partials, event.data.Get("delta.partial_json").String())
		}
	}
	return partials
}

func TestOpenAIStreamingToolArgumentsStreamAsTheyArrive(t *testing.T) {
	// A large Write call, its arguments split over many chunks.
	content := strings.Repeat("line of a large file\\n", 400)
	args := `{"file_path":"big.txt","content":"` + content + `"}`
	var pieces []string
	for rest := args; rest != ""; {
		n := min(97, len(rest))
		pieces = append(pieces, rest[:n])
		rest = rest[n:]
	}
	chunks := []string{toolChunk(startTool(0, "call_write", "Write", ""))}
	for _, piece := range pieces {
		chunks = append(chunks, toolChunk(toolArgs(0, piece)))
	}
	chunks = append(chunks, openAIStreamChunk("c", nil, "tool_calls"), openAIStreamUsageChunk("c"), "data: [DONE]")

	perChunk := translateChunks(chunks...)
	// Each piece goes out with the chunk that brought it, not at the end.
	for i, piece := range pieces {
		got := inputJSONDeltas(t, perChunk[i+1])
		if len(got) != 1 || got[0] != piece {
			t.Fatalf("chunk %d produced partial_json %q, want %q", i+1, got, piece)
		}
	}
	blocks := assembleClaudeStream(t, perChunk)
	if len(blocks) != 1 || blocks[0].kind != "tool_use" || blocks[0].name != "Write" || blocks[0].id != "call_write" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[0].content != args || !gjson.Valid(blocks[0].content) {
		t.Fatalf("assembled input differs from the upstream arguments")
	}
	if blocks[0].deltas != len(pieces) {
		t.Fatalf("deltas = %d, want one per upstream chunk (%d)", blocks[0].deltas, len(pieces))
	}
}

func TestOpenAIStreamingInterleavedToolCallsTakeTurns(t *testing.T) {
	perChunk := translateChunks(
		toolChunk(startTool(0, "call_a", "Read", `{"path":`)),
		toolChunk(startTool(1, "call_b", "Grep", `{"pattern":`)),
		toolChunk(toolArgs(0, `"a.go"}`), toolArgs(1, `"func`)),
		toolChunk(toolArgs(1, ` main"}`)),
		openAIStreamChunk("c", nil, "tool_calls"),
		"data: [DONE]",
	)
	// The second call waits while the first is unfinished...
	if got := inputJSONDeltas(t, perChunk[1]); len(got) != 0 {
		t.Fatalf("second call streamed while the first was open: %q", got)
	}
	// ...and opens, with what it was holding, the moment the first closes.
	if got := inputJSONDeltas(t, perChunk[2]); strings.Join(got, "|") != `"a.go"}|{"pattern":|"func` {
		t.Fatalf("chunk 2 partial_json = %q", got)
	}
	blocks := assembleClaudeStream(t, perChunk)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v", blocks)
	}
	for i, want := range []streamedBlock{
		{kind: "tool_use", id: "call_a", name: "Read", content: `{"path":"a.go"}`},
		{kind: "tool_use", id: "call_b", name: "Grep", content: `{"pattern":"func main"}`},
	} {
		got := blocks[i]
		if got.kind != want.kind || got.id != want.id || got.name != want.name || got.content != want.content {
			t.Fatalf("block %d = %+v, want %+v", i, got, want)
		}
	}
	if stream := strings.Join(append(perChunk[4], perChunk[5]...), ""); !strings.Contains(stream, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason is not tool_use:\n%s", stream)
	}
}

func TestOpenAIStreamingTextInterleavedWithToolCall(t *testing.T) {
	perChunk := translateChunks(
		openAITextChunk("reasoning_content", "Need the file."),
		openAITextChunk("content", "Let me look."),
		toolChunk(startTool(0, "call_a", "Read", `{"path":`)),
		openAITextChunk("content", " Also this."),
		toolChunk(toolArgs(0, `"a.go"}`)),
		openAITextChunk("content", " Done."),
		openAIStreamChunk("c", nil, "tool_calls"),
		openAIStreamUsageChunk("c"),
		"data: [DONE]",
	)
	// The call's block opens, and its arguments stream, as soon as it starts.
	if got := inputJSONDeltas(t, perChunk[2]); strings.Join(got, "") != `{"path":` {
		t.Fatalf("tool arguments after text did not stream: %q", got)
	}
	// Text that arrives inside an unfinished call waits for it.
	if len(perChunk[3]) != 0 {
		t.Fatalf("text went out inside an open tool_use block: %q", perChunk[3])
	}
	blocks := assembleClaudeStream(t, perChunk)
	want := []streamedBlock{
		{kind: "thinking", content: "Need the file."},
		{kind: "text", content: "Let me look."},
		{kind: "tool_use", id: "call_a", name: "Read", content: `{"path":"a.go"}`},
		{kind: "text", content: " Also this. Done."},
	}
	if len(blocks) != len(want) {
		t.Fatalf("blocks = %+v", blocks)
	}
	for i := range want {
		got := blocks[i]
		if got.kind != want[i].kind || got.content != want[i].content || got.name != want[i].name || got.id != want[i].id {
			t.Fatalf("block %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestOpenAIStreamingHeldBlocksOpenInTurn(t *testing.T) {
	// Text and a second call both arrive while the first call is unfinished.
	// When it finishes, the text goes out as its own block and the second
	// call opens behind it, with the arguments it was holding, in the same
	// chunk rather than at the end of the stream.
	perChunk := translateChunks(
		toolChunk(startTool(0, "call_a", "Read", `{"path":`)),
		openAITextChunk("content", "Checking."),
		toolChunk(startTool(1, "call_b", "Grep", `{"pattern":`)),
		toolChunk(toolArgs(0, `"a.go"}`)),
		toolChunk(toolArgs(1, `"main"}`)),
		openAIStreamChunk("c", nil, "tool_calls"),
		"data: [DONE]",
	)
	if len(perChunk[1]) != 0 || len(perChunk[2]) != 0 {
		t.Fatalf("blocks went out while the first call was open: %q %q", perChunk[1], perChunk[2])
	}
	var names []string
	for _, event := range parseClaudeEvents(t, perChunk[3]) {
		names = append(names, fmt.Sprintf("%s:%d", event.name, event.data.Get("index").Int()))
	}
	want := "content_block_delta:0 content_block_stop:0 content_block_start:1 content_block_delta:1 content_block_stop:1 content_block_start:2 content_block_delta:2"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("chunk 3 events:\n got %s\nwant %s", got, want)
	}
	if got := inputJSONDeltas(t, perChunk[4]); len(got) != 1 || got[0] != `"main"}` {
		t.Fatalf("chunk 4 partial_json = %q", got)
	}
	blocks := assembleClaudeStream(t, perChunk)
	if len(blocks) != 3 || blocks[0].content != `{"path":"a.go"}` || blocks[1].content != "Checking." || blocks[2].content != `{"pattern":"main"}` || blocks[2].id != "call_b" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestOpenAIStreamingUnfinishedCallHoldsOthersUntilTheEnd(t *testing.T) {
	// The first call never completes its JSON (cut off by the token limit):
	// the second still arrives, whole, after it.
	perChunk := translateChunks(
		toolChunk(startTool(0, "call_a", "Write", `{"content":"partial`)),
		toolChunk(startTool(1, "call_b", "Read", `{"path":"b.go"}`)),
		openAIStreamChunk("c", nil, "length"),
		"data: [DONE]",
	)
	blocks := assembleClaudeStream(t, perChunk)
	if len(blocks) != 2 || blocks[0].content != `{"content":"partial` || blocks[1].content != `{"path":"b.go"}` {
		t.Fatalf("blocks = %+v", blocks)
	}
	if stream := strings.Join(perChunk[3], ""); !strings.Contains(stream, `"stop_reason":"max_tokens"`) {
		t.Fatalf("stop_reason is not max_tokens:\n%s", stream)
	}
}

func TestOpenAIStreamingSingleQuotedArgumentsAreFixed(t *testing.T) {
	// Some upstreams write Python-style arguments. Standard JSON streams as
	// it arrives; from the first single-quoted string on, the rest waits
	// for util.FixJSON at the end of the block.
	perChunk := translateChunks(
		toolChunk(startTool(0, "call_a", "Read", `{"path": `)),
		toolChunk(toolArgs(0, `'a.go', "line": 1}`)),
		openAIStreamChunk("c", nil, "tool_calls"),
		"data: [DONE]",
	)
	if got := inputJSONDeltas(t, perChunk[0]); strings.Join(got, "") != `{"path": ` {
		t.Fatalf("standard prefix not streamed: %q", got)
	}
	blocks := assembleClaudeStream(t, perChunk)
	if len(blocks) != 1 || blocks[0].content != `{"path": "a.go", "line": 1}` {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestToolArgsScannerCompletion(t *testing.T) {
	for _, tc := range []struct {
		chunks   []string
		complete bool
		needsFix bool
	}{
		{[]string{`{"a":1}`}, true, false},
		{[]string{`{"a":`, `[1,2]`, `}`}, true, false},
		{[]string{`{"a":"}"`}, false, false},
		{[]string{`{"a":"\"}"`, `}`}, true, false},
		{[]string{`{"a":"x\\`, `"}`}, true, false},
		{[]string{`{'a': '}'`}, false, true},
		{[]string{`{'a': '}'}`}, true, true},
		{[]string{`"just a string"`}, false, false},
		{[]string{``}, false, false},
	} {
		var scanner toolArgsScanner
		for _, chunk := range tc.chunks {
			scanner.feed(chunk)
		}
		if scanner.complete() != tc.complete || scanner.needsFix != tc.needsFix {
			t.Errorf("%q: complete=%v needsFix=%v, want %v %v", tc.chunks, scanner.complete(), scanner.needsFix, tc.complete, tc.needsFix)
		}
	}
}
