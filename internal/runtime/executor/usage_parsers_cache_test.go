package executor

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// cacheFields is the cache side of a usage.Detail.
type cacheFields struct {
	reported bool
	read     int64
	write    int64
	legacy   int64
	included bool
}

func cacheFieldsOf(detail usage.Detail) cacheFields {
	return cacheFields{
		reported: detail.CacheReported,
		read:     detail.CacheReadTokens,
		write:    detail.CacheWriteTokens,
		legacy:   detail.CachedTokens,
		included: detail.CacheReadIncludedInInput,
	}
}

func TestUsageParsersReportCacheReadsAndWrites(t *testing.T) {
	streamDetail := func(parse func([]byte) (usage.Detail, bool)) func([]byte) usage.Detail {
		return func(line []byte) usage.Detail {
			detail, ok := parse(line)
			if !ok {
				t.Fatalf("stream parser rejected %s", line)
			}
			return detail
		}
	}
	codex := streamDetail(parseCodexUsage)

	for _, tc := range []struct {
		name  string
		parse func([]byte) usage.Detail
		body  string
		want  cacheFields
	}{
		{
			// The first turn of a conversation: legacy cached_tokens holds the
			// write, the read stays zero.
			name:  "claude turn that only wrote the cache",
			parse: parseClaudeUsage,
			body:  `{"usage":{"input_tokens":21,"cache_creation_input_tokens":50000,"cache_read_input_tokens":0,"output_tokens":5}}`,
			want:  cacheFields{reported: true, write: 50000, legacy: 50000},
		},
		{
			name:  "claude turn that read and wrote",
			parse: parseClaudeUsage,
			body:  `{"usage":{"input_tokens":100,"cache_creation_input_tokens":1000,"cache_read_input_tokens":900,"output_tokens":5}}`,
			want:  cacheFields{reported: true, read: 900, write: 1000, legacy: 900},
		},
		{
			name:  "claude without cache counters",
			parse: parseClaudeUsage,
			body:  `{"usage":{"input_tokens":100,"output_tokens":5}}`,
			want:  cacheFields{},
		},
		{
			name:  "claude stream delta with zero cache counters",
			parse: streamDetail(parseClaudeStreamUsage),
			body:  `data: {"type":"message_delta","usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":15}}`,
			want:  cacheFields{reported: true},
		},
		{
			// Some Anthropic-compatible upstreams put only output_tokens in the
			// final delta. Nothing was said about the cache: unknown, not a miss.
			name:  "claude stream delta without cache counters",
			parse: streamDetail(parseClaudeStreamUsage),
			body:  `data: {"type":"message_delta","usage":{"output_tokens":15}}`,
			want:  cacheFields{},
		},
		{
			name:  "openai chat with cached tokens",
			parse: parseOpenAIUsage,
			body:  `{"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"prompt_tokens_details":{"cached_tokens":400}}}`,
			want:  cacheFields{reported: true, read: 400, legacy: 400, included: true},
		},
		{
			name:  "openai chat that read nothing",
			parse: parseOpenAIUsage,
			body:  `{"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"prompt_tokens_details":{"cached_tokens":0}}}`,
			want:  cacheFields{reported: true, included: true},
		},
		{
			name:  "openai-compatible upstream without cache details",
			parse: parseOpenAIUsage,
			body:  `{"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005}}`,
			want:  cacheFields{},
		},
		{
			name:  "openai stream with cached tokens",
			parse: streamDetail(parseOpenAIStreamUsage),
			body:  `data: {"usage":{"prompt_tokens":1000,"completion_tokens":5,"total_tokens":1005,"prompt_tokens_details":{"cached_tokens":300}}}`,
			want:  cacheFields{reported: true, read: 300, legacy: 300, included: true},
		},
		{
			name:  "codex with cached tokens",
			parse: codex,
			body:  `{"response":{"usage":{"input_tokens":1000,"output_tokens":5,"total_tokens":1005,"input_tokens_details":{"cached_tokens":600}}}}`,
			want:  cacheFields{reported: true, read: 600, legacy: 600, included: true},
		},
		{
			name:  "codex without cache details",
			parse: codex,
			body:  `{"response":{"usage":{"input_tokens":1000,"output_tokens":5,"total_tokens":1005}}}`,
			want:  cacheFields{},
		},
		{
			// promptTokenCount includes the cached content.
			name:  "gemini with cached content",
			parse: parseGeminiUsage,
			body:  `{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":5,"totalTokenCount":1005,"cachedContentTokenCount":600}}`,
			want:  cacheFields{reported: true, read: 600, legacy: 600, included: true},
		},
		{
			// Gemini omits zero-valued fields: no cachedContentTokenCount is a
			// reported miss.
			name:  "gemini without cached content",
			parse: parseGeminiUsage,
			body:  `{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":5,"totalTokenCount":1005}}`,
			want:  cacheFields{reported: true, included: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheFieldsOf(tc.parse([]byte(tc.body))); got != tc.want {
				t.Fatalf("cache fields = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestOllamaPromptCacheEstimateReportsCacheUse(t *testing.T) {
	plain := applyOllamaPromptCacheEstimate(context.Background(), usage.Detail{InputTokens: 1000, OutputTokens: 5})
	if plain.CacheReported {
		t.Fatal("a request without an estimator reported its cache use")
	}

	// No reusable prefix: a known miss.
	estimator := &ollamaPromptCacheEstimator{promptLen: 10000}
	ctx := contextWithOllamaPromptCacheEstimator(context.Background(), estimator)
	miss := applyOllamaPromptCacheEstimate(ctx, usage.Detail{InputTokens: 1000, OutputTokens: 5})
	if got := cacheFieldsOf(miss); got != (cacheFields{reported: true}) {
		t.Fatalf("estimated miss = %+v, want a reported miss", got)
	}

	estimator.commonLen = 6000
	hit := applyOllamaPromptCacheEstimate(ctx, usage.Detail{InputTokens: 1000, OutputTokens: 5})
	if got := cacheFieldsOf(hit); got != (cacheFields{reported: true, read: 600, legacy: 600, included: true}) {
		t.Fatalf("estimated hit = %+v, want 600 read inside the input", got)
	}
}
