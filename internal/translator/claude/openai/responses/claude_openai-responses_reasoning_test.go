package responses

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Signatures shaped like the real formats. Anthropic's current signatures
// decode to a protobuf record starting 0x08 ("CAIS..."), the classic ones to
// 0x12 ("E..."); GPT reasoning blobs are Fernet tokens ("gAAAA...").
var (
	testClaudeSig        = base64.StdEncoding.EncodeToString(append([]byte{0x08, 0x02, 0x12}, []byte(strings.Repeat("claude-signature-body-", 6))...))
	testClaudeClassicSig = base64.StdEncoding.EncodeToString(append([]byte{0x12}, []byte(strings.Repeat("classic-signature-body-", 6))...))
	testGPTSig           = "gAAAAA" + base64.URLEncoding.EncodeToString([]byte(strings.Repeat("fernet-token-body-", 6)))
)

// claudeThinkingToolStream is the SSE a Claude model sends when it thinks and
// then calls a tool: thinking block, signature_delta, tool_use block.
func claudeThinkingToolStream(signature string) []string {
	return []string{
		`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I should list "}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"the files."}}`,
		fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":%q}}`, signature),
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"shell","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		`data: {"type":"message_stop"}`,
	}
}

func runClaudeStream(t *testing.T, lines []string) []responsesStreamEvent {
	t.Helper()
	var withBlanks []string
	for _, line := range lines {
		withBlanks = append(withBlanks, line, "")
	}
	return convertClaudeStream(t, withBlanks)
}

func reasoningDoneItems(events []responsesStreamEvent) []gjson.Result {
	var items []gjson.Result
	for _, ev := range events {
		if ev.name == "response.output_item.done" && ev.data.Get("item.type").String() == "reasoning" {
			items = append(items, ev.data.Get("item"))
		}
	}
	return items
}

// Codex CLI and pi persist a reasoning item only from response.output_item.done
// and replay its encrypted_content, so the signature must be on that event.
func TestClaudeResponsesStream_ReasoningDoneCarriesSignature(t *testing.T) {
	events := runClaudeStream(t, claudeThinkingToolStream(testClaudeSig))

	items := reasoningDoneItems(events)
	if len(items) != 1 {
		t.Fatalf("got %d reasoning output_item.done events, want 1", len(items))
	}
	item := items[0]
	if got := item.Get("encrypted_content").String(); got != testClaudeSig {
		t.Errorf("output_item.done encrypted_content = %q, want the signature_delta value", got)
	}
	if got := item.Get("summary.0.text").String(); got != "I should list the files." {
		t.Errorf("output_item.done summary text = %q", got)
	}
	if got := item.Get("id").String(); got != "rs_msg_01_0" {
		t.Errorf("output_item.done id = %q, want rs_msg_01_0", got)
	}

	// The signature itself must not leak as a text or summary delta.
	for _, ev := range events {
		if strings.Contains(ev.data.Get("delta").String(), testClaudeSig) {
			t.Fatalf("signature leaked through %s", ev.name)
		}
	}

	var completed gjson.Result
	for _, ev := range events {
		if ev.name == "response.completed" {
			completed = ev.data
		}
	}
	if got := completed.Get(`response.output.#(type=="reasoning").encrypted_content`).String(); got != testClaudeSig {
		t.Errorf("response.completed reasoning encrypted_content = %q, want the signature", got)
	}
}

// Anthropic interleaves several thinking blocks with tool calls; each one gets
// its own signature and its own reasoning item.
func TestClaudeResponsesStream_MultipleThinkingBlocksKeepOwnSignatures(t *testing.T) {
	lines := []string{
		`data: {"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first"}}`,
		fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":%q}}`, testClaudeSig),
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"second"}}`,
		fmt.Sprintf(`data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":%q}}`, testClaudeClassicSig),
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_stop"}`,
	}
	events := runClaudeStream(t, lines)

	items := reasoningDoneItems(events)
	if len(items) != 2 {
		t.Fatalf("got %d reasoning items, want 2", len(items))
	}
	if items[0].Get("encrypted_content").String() != testClaudeSig || items[1].Get("encrypted_content").String() != testClaudeClassicSig {
		t.Fatalf("signatures crossed between blocks: %s / %s", items[0].Raw, items[1].Raw)
	}
	var completed gjson.Result
	for _, ev := range events {
		if ev.name == "response.completed" {
			completed = ev.data
		}
	}
	if n := completed.Get(`response.output.#(type=="reasoning")#`).Array(); len(n) != 2 {
		t.Fatalf("response.completed lists %d reasoning items, want 2", len(n))
	}
}

func TestClaudeResponsesStream_RedactedThinkingCarriedVerbatim(t *testing.T) {
	lines := []string{
		`data: {"type":"message_start","message":{"id":"msg_03","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque-redacted-payload"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}
	items := reasoningDoneItems(runClaudeStream(t, lines))
	if len(items) != 1 {
		t.Fatalf("got %d reasoning items, want 1", len(items))
	}
	if got, want := items[0].Get("encrypted_content").String(), ClaudeResponsesRedactedThinkingPrefix+"opaque-redacted-payload"; got != want {
		t.Fatalf("encrypted_content = %q, want %q", got, want)
	}
}

func TestClaudeResponsesNonStream_ReasoningCarriesSignature(t *testing.T) {
	raw := strings.Join(claudeThinkingToolStream(testClaudeSig), "\n")
	req := []byte(`{"model":"claude-opus-5-5","input":"hi"}`)
	out := gjson.Parse(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-opus-5-5", req, req, []byte(raw), nil))

	reasoning := out.Get(`output.#(type=="reasoning")`)
	if got := reasoning.Get("encrypted_content").String(); got != testClaudeSig {
		t.Fatalf("non-stream reasoning encrypted_content = %q, want the signature", got)
	}
	if got := reasoning.Get("summary.0.text").String(); got != "I should list the files." {
		t.Fatalf("non-stream reasoning text = %q", got)
	}
}

// replayRequest builds a second-turn Responses request the way Codex and pi
// send it: user prompt, the reasoning item, the tool call, the tool output.
func replayRequest(t *testing.T, reasoningItem string) []byte {
	t.Helper()
	req := `{"model":"claude-opus-5-5","reasoning":{"effort":"high"},"input":[]}`
	req, _ = sjson.SetRaw(req, "input.-1", `{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}`)
	if reasoningItem != "" {
		req, _ = sjson.SetRaw(req, "input.-1", reasoningItem)
	}
	req, _ = sjson.SetRaw(req, "input.-1", `{"type":"function_call","call_id":"toolu_01","name":"shell","arguments":"{\"command\":\"ls\"}"}`)
	req, _ = sjson.SetRaw(req, "input.-1", `{"type":"function_call_output","call_id":"toolu_01","output":"a.txt"}`)
	return []byte(req)
}

func reasoningItemJSON(encrypted string) string {
	item := `{"type":"reasoning","id":"rs_msg_01_0","summary":[{"type":"summary_text","text":"I should list the files."}]}`
	if encrypted != "" {
		item, _ = sjson.Set(item, "encrypted_content", encrypted)
	}
	return item
}

func contentTypes(msg gjson.Result) []string {
	var types []string
	msg.Get("content").ForEach(func(_, part gjson.Result) bool {
		types = append(types, part.Get("type").String())
		return true
	})
	return types
}

func TestClaudeResponsesRequest_ReplaysSignedReasoningWithToolUse(t *testing.T) {
	for _, sig := range []struct{ name, value, want string }{
		{"current CAIS signature", testClaudeSig, testClaudeSig},
		{"classic signature", testClaudeClassicSig, testClaudeClassicSig},
		{"claude group prefix", "claude#" + testClaudeSig, testClaudeSig},
	} {
		t.Run(sig.name, func(t *testing.T) {
			out := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", replayRequest(t, reasoningItemJSON(sig.value)), true))
			msgs := out.Get("messages").Array()
			if len(msgs) != 3 {
				t.Fatalf("got %d messages, want 3 (user, assistant, user): %s", len(msgs), out.Get("messages").Raw)
			}
			asst := msgs[1]
			if asst.Get("role").String() != "assistant" {
				t.Fatalf("message 1 role = %q, want assistant", asst.Get("role").String())
			}
			// Anthropic requires thinking and the tool_use it led to in one
			// assistant message, thinking first.
			if got := strings.Join(contentTypes(asst), ","); got != "thinking,tool_use" {
				t.Fatalf("assistant content = %s, want thinking,tool_use", got)
			}
			if got := asst.Get("content.0.signature").String(); got != sig.want {
				t.Errorf("thinking signature = %q, want %q", got, sig.want)
			}
			if got := asst.Get("content.0.thinking").String(); got != "I should list the files." {
				t.Errorf("thinking text = %q", got)
			}
		})
	}
}

func TestClaudeResponsesRequest_DropsReasoningWithoutClaudeSignature(t *testing.T) {
	for _, tc := range []struct{ name, encrypted string }{
		{"no encrypted_content", ""},
		{"short value", "abc"},
		{"gemini skip sentinel", "skip_thought_signature_validator"},
		{"gpt reasoning blob", testGPTSig},
		{"foreign group prefix", "gemini#" + testClaudeSig},
		{"not base64", strings.Repeat("!not-base64?", 8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", replayRequest(t, reasoningItemJSON(tc.encrypted)), true))
			msgs := out.Get("messages").Array()
			if len(msgs) != 3 {
				t.Fatalf("got %d messages, want 3: %s", len(msgs), out.Get("messages").Raw)
			}
			if got := strings.Join(contentTypes(msgs[1]), ","); got != "tool_use" {
				t.Fatalf("assistant content = %s, want only tool_use", got)
			}
		})
	}
}

func TestClaudeResponsesRequest_ReasoningThenAssistantTextSharesMessage(t *testing.T) {
	req := `{"model":"claude-opus-5-5","input":[]}`
	req, _ = sjson.SetRaw(req, "input.-1", `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`)
	req, _ = sjson.SetRaw(req, "input.-1", reasoningItemJSON(testClaudeSig))
	req, _ = sjson.SetRaw(req, "input.-1", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello."}]}`)
	req, _ = sjson.SetRaw(req, "input.-1", `{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}`)

	msgs := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", []byte(req), true)).Get("messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if got := strings.Join(contentTypes(msgs[1]), ","); got != "thinking,text" {
		t.Fatalf("assistant content = %s, want thinking,text", got)
	}
}

func TestClaudeResponsesRequest_RedactedThinkingRestored(t *testing.T) {
	item := reasoningItemJSON(ClaudeResponsesRedactedThinkingPrefix + "opaque-redacted-payload")
	msgs := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", replayRequest(t, item), true)).Get("messages").Array()
	block := msgs[1].Get("content.0")
	if block.Get("type").String() != "redacted_thinking" || block.Get("data").String() != "opaque-redacted-payload" {
		t.Fatalf("redacted block = %s", block.Raw)
	}
	if block.Get("signature").Exists() {
		t.Fatalf("redacted_thinking must not carry a signature: %s", block.Raw)
	}
}

// A turn cut off right after thinking leaves a trailing thinking block, which
// Anthropic rejects as the last assistant content.
func TestClaudeResponsesRequest_StripsTrailingThinking(t *testing.T) {
	req := `{"model":"claude-opus-5-5","input":[]}`
	req, _ = sjson.SetRaw(req, "input.-1", `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`)
	req, _ = sjson.SetRaw(req, "input.-1", reasoningItemJSON(testClaudeSig))

	msgs := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", []byte(req), true)).Get("messages").Array()
	if len(msgs) != 1 || msgs[0].Get("role").String() != "user" {
		t.Fatalf("trailing thinking-only assistant message was not removed: %v", msgs)
	}
}

// Round trip: what the response converter emits for turn 1 is what the request
// converter must turn back into a signed thinking block for turn 2.
func TestClaudeResponses_ReasoningRoundTrip(t *testing.T) {
	events := runClaudeStream(t, claudeThinkingToolStream(testClaudeSig))
	items := reasoningDoneItems(events)
	if len(items) != 1 {
		t.Fatalf("got %d reasoning items, want 1", len(items))
	}

	msgs := gjson.ParseBytes(ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", replayRequest(t, items[0].Raw), true)).Get("messages").Array()
	asst := msgs[1]
	if got := strings.Join(contentTypes(asst), ","); got != "thinking,tool_use" {
		t.Fatalf("assistant content = %s, want thinking,tool_use", got)
	}
	if asst.Get("content.0.signature").String() != testClaudeSig || asst.Get("content.0.thinking").String() != "I should list the files." {
		t.Fatalf("round-tripped thinking block = %s", asst.Get("content.0").Raw)
	}
}

func TestClaudeResponsesStream_InterleavedReasoningAndToolsKeepOutputOrder(t *testing.T) {
	lines := []string{
		`data: {"type":"message_start","message":{"id":"msg_order","type":"message","role":"assistant","content":[]}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first"}}`,
		fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":%q}}`, testClaudeSig),
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_order","name":"shell","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"thinking_delta","thinking":"second"}}`,
		fmt.Sprintf(`data: {"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":%q}}`, testClaudeSig),
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"message_stop"}`,
	}
	events := runClaudeStream(t, lines)
	var output gjson.Result
	for _, ev := range events {
		if ev.name == "response.completed" {
			output = ev.data.Get("response.output")
		}
	}
	if got := output.Get("1.type").String(); got != "function_call" {
		t.Fatalf("response.output order = %s, want function_call at index 1", output.Raw)
	}
	if got := output.Get("2.type").String(); got != "reasoning" {
		t.Fatalf("response.output order = %s, want second reasoning at index 2", output.Raw)
	}
}

func TestClaudeResponsesNonStream_InterleavedOutputKeepsSourceOrder(t *testing.T) {
	blocks := []claudeStreamBlock{
		{kind: "text", chunks: []string{"before tool"}},
		{kind: "thinking", chunks: []string{"first thought"}},
		{kind: "tool_use", chunks: []string{`{"command":"ls"}`}},
		{kind: "thinking", chunks: []string{"second thought"}},
	}
	raw := strings.Join(claudeStreamLines(t, blocks, "tool_use"), "\n")
	req := []byte(`{"model":"claude-opus-5-5","input":"hi"}`)
	out := gjson.Parse(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-opus-5-5", req, req, []byte(raw), nil))

	var got []string
	out.Get("output").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "message":
			got = append(got, "message:"+item.Get("content.0.text").String())
		case "reasoning":
			got = append(got, "reasoning:"+item.Get("summary.0.text").String())
		case "function_call":
			got = append(got, "function_call:"+item.Get("call_id").String())
		}
		return true
	})
	want := []string{
		"message:before tool",
		"reasoning:first thought",
		"function_call:toolu_02",
		"reasoning:second thought",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("non-stream output order = %q, want %q; output=%s", got, want, out.Get("output").Raw)
	}

	// The stream and non-stream paths must expose the same ordered item kinds.
	stream := convertClaudeStream(t, claudeStreamLines(t, blocks, "tool_use"))
	var streamOutput gjson.Result
	for _, ev := range stream {
		if ev.name == "response.completed" {
			streamOutput = ev.data.Get("response.output")
		}
	}
	var streamKinds []string
	streamOutput.ForEach(func(_, item gjson.Result) bool {
		streamKinds = append(streamKinds, item.Get("type").String())
		return true
	})
	var nonStreamKinds []string
	out.Get("output").ForEach(func(_, item gjson.Result) bool {
		nonStreamKinds = append(nonStreamKinds, item.Get("type").String())
		return true
	})
	if !slices.Equal(streamKinds, nonStreamKinds) {
		t.Fatalf("stream output kinds = %q, non-stream = %q", streamKinds, nonStreamKinds)
	}
}

func TestClaudeResponsesStream_TextOutputIndicesMatchAddedAndDone(t *testing.T) {
	blocks := []claudeStreamBlock{
		{kind: "text", chunks: []string{"answer"}},
		{kind: "thinking", chunks: []string{"after"}},
	}
	events := convertClaudeStream(t, claudeStreamLines(t, blocks, "end_turn"))
	added := map[string]int{}
	done := map[string]int{}
	for _, ev := range events {
		if ev.name == "response.output_item.added" {
			added[ev.data.Get("item.id").String()] = int(ev.data.Get("output_index").Int())
		}
		if ev.name == "response.output_item.done" {
			done[ev.data.Get("item.id").String()] = int(ev.data.Get("output_index").Int())
		}
	}
	if len(added) != len(done) {
		t.Fatalf("added item IDs = %v, done IDs = %v", added, done)
	}
	for id, got := range added {
		if done[id] != got {
			t.Errorf("item %s output_index added=%d done=%d", id, got, done[id])
		}
	}
}
