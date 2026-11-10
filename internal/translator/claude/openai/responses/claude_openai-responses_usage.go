package responses

import "github.com/tidwall/gjson"

// claudeResponsesUsage accumulates Anthropic usage for one response and maps
// it to OpenAI Responses usage.
//
// Anthropic reports input_tokens without the cached prefix: tokens read from
// the prompt cache and tokens written to it are reported separately, as
// cache_read_input_tokens and cache_creation_input_tokens. OpenAI Responses
// counts every prompt token in input_tokens and reports the cache hit as
// input_tokens_details.cached_tokens. Passing Anthropic's input_tokens through
// unchanged makes a cached 50k-token prompt look like a 10-token one, which
// breaks clients that size their context window from usage (pi, Codex CLI).
//
// The relay's own usage records are parsed from the upstream body in the
// executor and do not go through this mapping.
type claudeResponsesUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	Seen                bool
}

// merge records the fields present in an Anthropic usage object. Both
// message_start and message_delta carry usage, message_delta's counts are
// cumulative, so later values win; a field it leaves out keeps its value.
func (u *claudeResponsesUsage) merge(usage gjson.Result) {
	if !usage.Exists() {
		return
	}
	if v := usage.Get("input_tokens"); v.Exists() {
		u.InputTokens = v.Int()
		u.Seen = true
	}
	if v := usage.Get("output_tokens"); v.Exists() {
		u.OutputTokens = v.Int()
		u.Seen = true
	}
	if v := usage.Get("cache_read_input_tokens"); v.Exists() {
		u.CacheReadTokens = v.Int()
		u.Seen = true
	}
	if v := usage.Get("cache_creation_input_tokens"); v.Exists() {
		u.CacheCreationTokens = v.Int()
		u.Seen = true
	}
}

// responsesInputTokens is the full prompt size: uncached, cache-read and
// cache-written tokens.
func (u claudeResponsesUsage) responsesInputTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

func (u claudeResponsesUsage) responsesTotalTokens() int64 {
	return u.responsesInputTokens() + u.OutputTokens
}
