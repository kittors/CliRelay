package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// claudeCachedTurn is a cached Claude turn as Anthropic streams it:
// message_start carries the prompt split into uncached, cache-written and
// cache-read tokens, and message_delta closes the turn with deltaUsage.
func claudeCachedTurn(deltaUsage string) string {
	return `event: message_start
data: {"type":"message_start","message":{"id":"msg_cache","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":5292,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":` + deltaUsage + `}

event: message_stop
data: {"type":"message_stop"}
`
}

var claudeCachedTurns = []struct{ name, sse string }{
	// Anthropic repeats the cumulative split on message_delta.
	{"split repeated on message_delta", claudeCachedTurn(`{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":5292,"output_tokens":5}`)},
	// Older streams carry only output_tokens there; the split from
	// message_start must survive it.
	{"only output_tokens on message_delta", claudeCachedTurn(`{"output_tokens":5}`)},
}

// OpenAI Responses counts the whole prompt in input_tokens and the cache hit
// in input_tokens_details.cached_tokens.
const (
	wantCachedInputTokens = 10 + 200 + 5292
	wantCachedReadTokens  = 5292
	wantCachedTotalTokens = wantCachedInputTokens + 5
)

func assertCachedUsage(t *testing.T, usage gjson.Result) {
	t.Helper()
	if got := usage.Get("input_tokens").Int(); got != wantCachedInputTokens {
		t.Errorf("input_tokens = %d, want %d (uncached + cache write + cache read)", got, wantCachedInputTokens)
	}
	if got := usage.Get("input_tokens_details.cached_tokens").Int(); got != wantCachedReadTokens {
		t.Errorf("input_tokens_details.cached_tokens = %d, want %d", got, wantCachedReadTokens)
	}
	if got := usage.Get("output_tokens").Int(); got != 5 {
		t.Errorf("output_tokens = %d, want 5", got)
	}
	if got := usage.Get("total_tokens").Int(); got != wantCachedTotalTokens {
		t.Errorf("total_tokens = %d, want %d", got, wantCachedTotalTokens)
	}
}

// Anthropic's input_tokens leaves out the cached prefix, so passing it through
// made a cached 5k-token prompt reach the client as 10 tokens, and clients that
// size the context window from usage (pi) never compacted (#1163).
func TestConvertClaudeResponseToOpenAIResponses_CountsCachedPrompt(t *testing.T) {
	for _, tc := range claudeCachedTurns {
		t.Run(tc.name, func(t *testing.T) {
			for _, ev := range convertClaudeStream(t, strings.Split(tc.sse, "\n")) {
				if ev.name == "response.completed" {
					assertCachedUsage(t, ev.data.Get("response.usage"))
					return
				}
			}
			t.Fatal("no response.completed event")
		})
	}
}

func TestConvertClaudeResponseToOpenAIResponsesNonStream_CountsCachedPrompt(t *testing.T) {
	req := []byte(`{"model":"claude-sonnet-5-5","input":"hi"}`)
	for _, tc := range claudeCachedTurns {
		t.Run(tc.name, func(t *testing.T) {
			out := gjson.Parse(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-sonnet-5-5", req, req, []byte(tc.sse), nil))
			assertCachedUsage(t, out.Get("usage"))
		})
	}
}

// Without cache fields the numbers are Anthropic's own, as before.
func TestConvertClaudeResponseToOpenAIResponses_UncachedUsageUnchanged(t *testing.T) {
	lines := claudeStreamLines(t, []claudeStreamBlock{{kind: "text", chunks: []string{"hi"}}}, "end_turn")
	assertUncached := func(t *testing.T, usage gjson.Result) {
		t.Helper()
		if usage.Get("input_tokens").Int() != 25 || usage.Get("input_tokens_details.cached_tokens").Int() != 0 ||
			usage.Get("output_tokens").Int() != 42 || usage.Get("total_tokens").Int() != 67 {
			t.Fatalf("usage = %s, want input 25, cached 0, output 42, total 67", usage.Raw)
		}
	}
	t.Run("stream", func(t *testing.T) {
		for _, ev := range convertClaudeStream(t, lines) {
			if ev.name == "response.completed" {
				assertUncached(t, ev.data.Get("response.usage"))
				return
			}
		}
		t.Fatal("no response.completed event")
	})
	t.Run("non-stream", func(t *testing.T) {
		req := []byte(`{"model":"claude-opus-4-6","input":"hi"}`)
		out := gjson.Parse(ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-opus-4-6", req, req, []byte(strings.Join(lines, "\n")), nil))
		assertUncached(t, out.Get("usage"))
	})
}
