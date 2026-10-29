package translator

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
	"github.com/tidwall/gjson"
)

// This matrix ends one text reply for every reason each upstream protocol can
// give, sends it through every registered response translator to a protocol
// with other names for its reasons, streaming and not, and checks that the
// client gets its own name for the same reason: a reply cut short or withheld
// must not arrive as a plain end of turn, and no client may get a name its
// protocol does not define. Gemini, Gemini CLI and Antigravity share Gemini's
// names, and the translators between them pass reasons on as they are.

// stopUpstream is one upstream protocol: the reasons it can end a reply with,
// and the response it sends for each.
type stopUpstream struct {
	format    string
	names     string // whose names for stop reasons the protocol uses
	reasons   map[string]common.StopReason
	stream    func(reason string) []string // chunks, in the form the executor hands them over
	nonStream func(reason string) string
}

func claudeUpstreamEvents(reason string) []string {
	return []string{
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"` + reason + `","stop_sequence":null},"usage":{"output_tokens":3}}`,
		`data: {"type":"message_stop"}`,
	}
}

func chatUpstreamChunks(reason string) []string {
	return []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}`,
		`data: [DONE]`,
	}
}

func chatUpstreamResponse(reason string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"` + reason + `"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
}

// codexFinalEvent is the event that ends a Responses stream: response.completed,
// or response.incomplete with reason as incomplete_details.reason.
func codexFinalEvent(reason string) string {
	output := `"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}`
	if reason == "completed" {
		return `{"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m",` + output + `}}`
	}
	return `{"type":"response.incomplete","response":{"id":"resp_1","object":"response","created_at":1,"status":"incomplete","incomplete_details":{"reason":"` + reason + `"},"model":"m",` + output + `}}`
}

func codexUpstreamEvents(reason string) []string {
	return []string{
		`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`,
		`data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}`,
		`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello"}`,
		`data: {"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hello","annotations":[]}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello","annotations":[]}]}}`,
		`data: ` + codexFinalEvent(reason),
	}
}

func geminiFinalChunk(text, reason string) string {
	return `{"candidates":[{"content":{"role":"model","parts":[{"text":"` + text + `"}]},"finishReason":"` + reason + `","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8},"modelVersion":"m","responseId":"r1"}`
}

func geminiUpstreamChunks(wrap func(string) string) func(string) []string {
	return func(reason string) []string {
		return []string{
			wrap(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]},"index":0}],"modelVersion":"m","responseId":"r1"}`),
			wrap(geminiFinalChunk("lo", reason)),
			"[DONE]",
		}
	}
}

func asIs(chunk string) string { return chunk }

// cliEnvelope wraps a Gemini response the way Gemini CLI and Antigravity send it.
func cliEnvelope(chunk string) string { return `{"response":` + chunk + `}` }

var (
	geminiStopReasons = map[string]common.StopReason{
		"STOP":       common.StopEndTurn,
		"MAX_TOKENS": common.StopMaxTokens,
		"SAFETY":     common.StopRefusal,
		"RECITATION": common.StopRefusal,
	}
	stopUpstreams = []stopUpstream{
		{
			format: "claude",
			names:  "claude",
			reasons: map[string]common.StopReason{
				"end_turn":      common.StopEndTurn,
				"stop_sequence": common.StopEndTurn,
				"pause_turn":    common.StopEndTurn,
				"max_tokens":    common.StopMaxTokens,
				// Claude 4.5 and later hitting the context window before max_tokens.
				"model_context_window_exceeded": common.StopMaxTokens,
				"refusal":                       common.StopRefusal,
			},
			stream:    claudeUpstreamEvents,
			nonStream: func(reason string) string { return strings.Join(claudeUpstreamEvents(reason), "\n") },
		},
		{
			format: "openai",
			names:  "openai",
			reasons: map[string]common.StopReason{
				"stop":           common.StopEndTurn,
				"length":         common.StopMaxTokens,
				"content_filter": common.StopRefusal,
			},
			stream:    chatUpstreamChunks,
			nonStream: chatUpstreamResponse,
		},
		{
			format: "codex",
			names:  "responses",
			reasons: map[string]common.StopReason{
				"completed":         common.StopEndTurn,
				"max_output_tokens": common.StopMaxTokens,
				"content_filter":    common.StopRefusal,
			},
			stream:    codexUpstreamEvents,
			nonStream: codexFinalEvent,
		},
		{
			format:    "gemini",
			names:     "gemini",
			reasons:   geminiStopReasons,
			stream:    geminiUpstreamChunks(asIs),
			nonStream: func(reason string) string { return geminiFinalChunk("Hello", reason) },
		},
		{
			format:    "gemini-cli",
			names:     "gemini",
			reasons:   geminiStopReasons,
			stream:    geminiUpstreamChunks(cliEnvelope),
			nonStream: func(reason string) string { return cliEnvelope(geminiFinalChunk("Hello", reason)) },
		},
		{
			format:    "antigravity",
			names:     "gemini",
			reasons:   geminiStopReasons,
			stream:    geminiUpstreamChunks(cliEnvelope),
			nonStream: func(reason string) string { return cliEnvelope(geminiFinalChunk("Hello", reason)) },
		},
	}
)

// stopClient is one client protocol: how to read the reason it was given.
type stopClient struct {
	format    string
	names     string
	want      func(common.StopReason) string
	stream    func(t *testing.T, outputs []string) string
	nonStream func(out string) string
}

// sseData returns the JSON payloads of the data: lines in outputs, or each
// output itself when it is a bare JSON chunk.
func sseData(outputs []string) []gjson.Result {
	var payloads []gjson.Result
	for _, output := range outputs {
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
			if gjson.Valid(line) && strings.HasPrefix(line, "{") {
				payloads = append(payloads, gjson.Parse(line))
			}
		}
	}
	return payloads
}

// lastValue returns the last non-empty value at path among payloads.
func lastValue(payloads []gjson.Result, paths ...string) string {
	value := ""
	for _, payload := range payloads {
		for _, path := range paths {
			if v := payload.Get(path).String(); v != "" {
				value = v
			}
		}
	}
	return value
}

var stopClients = []stopClient{
	{
		format: "claude",
		names:  "claude",
		want:   common.StopReason.Claude,
		stream: func(t *testing.T, outputs []string) string {
			payloads := sseData(outputs)
			if lastValue(payloads, "type") != "message_stop" {
				t.Errorf("stream does not end with message_stop: %q", outputs)
			}
			return lastValue(payloads, "delta.stop_reason")
		},
		nonStream: func(out string) string { return gjson.Get(out, "stop_reason").String() },
	},
	{
		format: "openai",
		names:  "openai",
		want:   common.StopReason.OpenAI,
		stream: func(_ *testing.T, outputs []string) string {
			return lastValue(sseData(outputs), "choices.0.finish_reason")
		},
		nonStream: func(out string) string { return gjson.Get(out, "choices.0.finish_reason").String() },
	},
	{
		format: "gemini",
		names:  "gemini",
		want:   common.StopReason.Gemini,
		stream: func(_ *testing.T, outputs []string) string {
			return lastValue(sseData(outputs), "candidates.0.finishReason")
		},
		nonStream: func(out string) string { return gjson.Get(out, "candidates.0.finishReason").String() },
	},
	{
		format: "gemini-cli",
		names:  "gemini",
		want:   common.StopReason.Gemini,
		stream: func(_ *testing.T, outputs []string) string {
			return lastValue(sseData(outputs), "response.candidates.0.finishReason")
		},
		nonStream: func(out string) string { return gjson.Get(out, "response.candidates.0.finishReason").String() },
	},
}

func TestStopReasonsSurviveEveryTranslator(t *testing.T) {
	ctx := context.Background()
	request := []byte(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	// Some translators only write events for a request that asked for a stream.
	streamRequest := []byte(`{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	paths := 0
	for _, upstream := range stopUpstreams {
		for _, client := range stopClients {
			if upstream.names == client.names || !translator.NeedConvert(client.format, upstream.format) {
				continue
			}
			paths++
			for reason, kind := range upstream.reasons {
				name := upstream.format + "->" + client.format + "/" + reason
				want := client.want(kind)
				t.Run(name+"/stream", func(t *testing.T) {
					var param any
					var outputs []string
					for _, chunk := range upstream.stream(reason) {
						outputs = append(outputs, translator.Response(upstream.format, client.format, ctx, "m", streamRequest, streamRequest, []byte(chunk), &param)...)
					}
					if got := client.stream(t, outputs); got != want {
						t.Fatalf("stop reason = %q, want %q; outputs: %q", got, want, outputs)
					}
				})
				t.Run(name+"/non-stream", func(t *testing.T) {
					var param any
					out := translator.ResponseNonStream(upstream.format, client.format, ctx, "m", request, request, []byte(upstream.nonStream(reason)), &param)
					if got := client.nonStream(out); got != want {
						t.Fatalf("stop reason = %q, want %q; response: %s", got, want, out)
					}
				})
			}
		}
	}
	// Every response translator between two sets of names here is covered.
	if paths < 16 {
		t.Fatalf("only %d translator paths exercised", paths)
	}
}

func TestStopReasonVocabulary(t *testing.T) {
	// Each protocol's own names read back as themselves.
	for _, kind := range []common.StopReason{common.StopEndTurn, common.StopMaxTokens, common.StopToolUse, common.StopRefusal} {
		if got := common.StopReasonFromClaude(kind.Claude()); got != kind || !common.IsClaudeStopReason(kind.Claude()) {
			t.Errorf("Claude round trip of %v = %v", kind, got)
		}
		if got := common.StopReasonFromOpenAI(kind.OpenAI()); got != kind {
			t.Errorf("OpenAI round trip of %v = %v", kind, got)
		}
		if kind != common.StopToolUse {
			if got := common.StopReasonFromGemini(kind.Gemini()); got != kind {
				t.Errorf("Gemini round trip of %v = %v", kind, got)
			}
		}
	}
	for _, tc := range []struct {
		name string
		got  common.StopReason
		want common.StopReason
	}{
		// Anthropic-shaped relays pass OpenAI's names on.
		{"claude relay length", common.StopReasonFromClaude("length"), common.StopMaxTokens},
		{"claude relay tool_calls", common.StopReasonFromClaude("tool_calls"), common.StopToolUse},
		{"claude relay content_filter", common.StopReasonFromClaude("content_filter"), common.StopRefusal},
		{"claude unknown", common.StopReasonFromClaude("something_new"), common.StopEndTurn},
		{"claude stop_sequence", common.StopReasonFromClaude("stop_sequence"), common.StopEndTurn},
		{"claude pause_turn", common.StopReasonFromClaude("pause_turn"), common.StopEndTurn},
		{"openai compatible max_tokens", common.StopReasonFromOpenAI("max_tokens"), common.StopMaxTokens},
		{"openai legacy function_call", common.StopReasonFromOpenAI("function_call"), common.StopToolUse},
		{"gemini lower case", common.StopReasonFromGemini("max_tokens"), common.StopMaxTokens},
		{"gemini malformed function call", common.StopReasonFromGemini("MALFORMED_FUNCTION_CALL"), common.StopEndTurn},
		{"responses completed", common.StopReasonFromResponses("completed", ""), common.StopEndTurn},
		{"responses unknown incomplete", common.StopReasonFromResponses("incomplete", "something_new"), common.StopEndTurn},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
