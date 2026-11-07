package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// What a request's prompt was billed as: input outside the cache, cache reads
// and writes. Upstreams name these differently; each must end up the same.
type promptSplit struct {
	input    int64
	read     int64
	write    int64
	included bool
	reported bool
}

func promptSplitOf(detail usage.Detail) promptSplit {
	return promptSplit{
		input:    detail.InputTokens,
		read:     detail.CacheReadTokens,
		write:    detail.CacheWriteTokens,
		included: detail.CacheReadIncludedInInput,
		reported: detail.CacheReported,
	}
}

func TestOpenAIUsageReadsEveryUpstreamsCacheCounters(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want promptSplit
	}{
		{
			// prompt_tokens = hit + miss.
			name: "deepseek",
			body: `{"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"prompt_cache_hit_tokens":700,"prompt_cache_miss_tokens":300}}`,
			want: promptSplit{input: 1000, read: 700, included: true, reported: true},
		},
		{
			name: "kimi",
			body: `{"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"cached_tokens":600}}`,
			want: promptSplit{input: 1000, read: 600, included: true, reported: true},
		},
		{
			// prompt_tokens counts the writes too; they come out of the input.
			name: "openrouter with cache writes",
			body: `{"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"prompt_tokens_details":{"cached_tokens":300,"cache_write_tokens":500}}}`,
			want: promptSplit{input: 500, read: 300, write: 500, included: true, reported: true},
		},
		{
			// A relay passing on Anthropic's names with Anthropic's counts:
			// prompt_tokens is only what was neither read nor written.
			name: "anthropic names, anthropic counts",
			body: `{"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25,"cache_read_input_tokens":9000,"cache_creation_input_tokens":1000}}`,
			want: promptSplit{input: 20, read: 9000, write: 1000, reported: true},
		},
		{
			// The same names on a relay that counts the whole prompt.
			name: "anthropic names, whole prompt",
			body: `{"usage":{"prompt_tokens":10020,"completion_tokens":5,"total_tokens":10025,"cache_read_input_tokens":9000,"cache_creation_input_tokens":1000}}`,
			want: promptSplit{input: 9020, read: 9000, write: 1000, included: true, reported: true},
		},
		{
			// Anthropic's names inside OpenAI's details object, with OpenAI's
			// own counter there but zero.
			name: "anthropic names in the details",
			body: `{"usage":{"prompt_tokens":3000,"completion_tokens":5,"total_tokens":3005,"prompt_tokens_details":{"cached_tokens":0,"cache_creation_input_tokens":2000}}}`,
			want: promptSplit{input: 1000, write: 2000, included: true, reported: true},
		},
		{
			name: "openai's own counter wins",
			body: `{"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"prompt_tokens_details":{"cached_tokens":400},"prompt_cache_hit_tokens":700}}`,
			want: promptSplit{input: 1000, read: 400, included: true, reported: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := promptSplitOf(parseOpenAIUsage([]byte(tc.body))); got != tc.want {
				t.Fatalf("parsed %+v, want %+v", got, tc.want)
			}
			stream, ok := parseOpenAIStreamUsage([]byte("data: " + tc.body))
			if !ok {
				t.Fatal("stream parser rejected the usage chunk")
			}
			if got := promptSplitOf(stream); got != tc.want {
				t.Fatalf("stream parsed %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClaudeUsageSplitsCacheWritesByLifetime(t *testing.T) {
	detail := parseClaudeUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":400,"ephemeral_1h_input_tokens":600}}}`))
	if detail.CacheWriteTokens != 1000 || detail.CacheWrite1hTokens != 600 {
		t.Fatalf("writes = %d, one-hour = %d; want 1000 and 600", detail.CacheWriteTokens, detail.CacheWrite1hTokens)
	}

	onlySplit := parseClaudeUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":800}}}`))
	if onlySplit.CacheWriteTokens != 800 || onlySplit.CacheWrite1hTokens != 800 || !onlySplit.CacheReported {
		t.Fatalf("split alone = %+v, want 800 one-hour writes", onlySplit)
	}
}

func TestClaudeStreamUsageTakesThePromptFromMessageStart(t *testing.T) {
	start := `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":36,"output_tokens":1,"cache_read_input_tokens":5000,"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":1000}}}}`
	for _, tc := range []struct {
		name  string
		delta string
	}{
		// Bedrock and some relays: the final event has the output alone.
		{name: "output only", delta: `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":145}}`},
		// Anthropic repeats the counts, without the split.
		{name: "cumulative counts", delta: `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":36,"cache_read_input_tokens":5000,"cache_creation_input_tokens":1000,"output_tokens":145}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &claudeStreamUsage{}
			if _, ok := stream.observe([]byte(start)); ok {
				t.Fatal("message_start produced a usage record; the reporter would keep it and lose the output count")
			}
			detail, ok := stream.observe([]byte(tc.delta))
			if !ok {
				t.Fatal("message_delta produced no usage record")
			}
			want := usage.Detail{
				InputTokens: 36, OutputTokens: 145, TotalTokens: 181,
				CacheReadTokens: 5000, CacheWriteTokens: 1000, CacheWrite1hTokens: 1000, CachedTokens: 5000,
				CacheReported: true,
			}
			if detail != want {
				t.Fatalf("usage = %+v, want %+v", detail, want)
			}
		})
	}
}
