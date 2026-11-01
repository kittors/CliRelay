package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/openaicompat"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func parseCodexUsage(data []byte) (usage.Detail, bool) {
	usageNode := gjson.ParseBytes(data).Get("response.usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	detail := usage.Detail{
		InputTokens:  usageNode.Get("input_tokens").Int(),
		OutputTokens: usageNode.Get("output_tokens").Int(),
		TotalTokens:  usageNode.Get("total_tokens").Int(),
	}
	if cached := usageNode.Get("input_tokens_details.cached_tokens"); cached.Exists() {
		detail.CacheReadTokens = cached.Int()
		detail.CachedTokens = detail.CacheReadTokens
		detail.CacheReadIncludedInInput = true
		detail.CacheReported = true
	}
	if reasoning := usageNode.Get("output_tokens_details.reasoning_tokens"); reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	return detail, true
}

func parseOpenAIUsage(data []byte) usage.Detail {
	usageNode := openaicompat.ParseResponseRoot(data).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	inputNode := usageNode.Get("prompt_tokens")
	if !inputNode.Exists() {
		inputNode = usageNode.Get("input_tokens")
	}
	outputNode := usageNode.Get("completion_tokens")
	if !outputNode.Exists() {
		outputNode = usageNode.Get("output_tokens")
	}
	detail := usage.Detail{
		InputTokens:  inputNode.Int(),
		OutputTokens: outputNode.Int(),
		TotalTokens:  usageNode.Get("total_tokens").Int(),
	}
	applyOpenAICacheUsage(&detail, usageNode)
	reasoning := usageNode.Get("completion_tokens_details.reasoning_tokens")
	if !reasoning.Exists() {
		reasoning = usageNode.Get("output_tokens_details.reasoning_tokens")
	}
	if reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	return detail
}

func parseOpenAIResponseModel(data []byte) string {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return ""
	}
	return strings.TrimSpace(openaicompat.ParseResponseRoot(data).Get("model").String())
}

func parseOpenAIStreamModel(line []byte) string {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	return strings.TrimSpace(openaicompat.ParseResponseRoot(payload).Get("model").String())
}

func parseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := openaicompat.ParseResponseRoot(payload).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	detail := usage.Detail{
		InputTokens:  usageNode.Get("prompt_tokens").Int(),
		OutputTokens: usageNode.Get("completion_tokens").Int(),
		TotalTokens:  usageNode.Get("total_tokens").Int(),
	}
	applyOpenAICacheUsage(&detail, usageNode)
	if reasoning := usageNode.Get("completion_tokens_details.reasoning_tokens"); reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	return detail, true
}

// applyOpenAICacheUsage reads the cache counters of an OpenAI-style usage
// object into detail, whose InputTokens already holds the prompt count.
//
// OpenAI's prompt_tokens is the whole prompt: cache reads are a part of it
// (CacheReadIncludedInInput), and so are cache writes where an upstream counts
// them (prompt_tokens_details.cache_write_tokens, as OpenRouter does). The
// writes are taken out of InputTokens, because no input figure downstream is
// expected to include them (see promptTokenTotal in internal/usage).
//
// Not every upstream uses OpenAI's names. DeepSeek reports reads as
// prompt_cache_hit_tokens, Kimi as a top-level cached_tokens, and relays in
// front of Claude pass on Anthropic's cache_read_input_tokens and
// cache_creation_input_tokens, at the top level or in prompt_tokens_details.
// Anthropic's names may come with Anthropic's prompt count, which leaves out
// both, so with them the prompt count is taken as the whole prompt only when
// it is at least the two together.
func applyOpenAICacheUsage(detail *usage.Detail, usageNode gjson.Result) {
	details := usageNode.Get("prompt_tokens_details")
	if !details.Exists() {
		details = usageNode.Get("input_tokens_details")
	}
	read := firstExistingResult(details.Get("cached_tokens"), usageNode.Get("prompt_cache_hit_tokens"), usageNode.Get("cached_tokens"))
	write := firstExistingResult(details.Get("cache_write_tokens"), usageNode.Get("cache_write_tokens"))
	whole := true
	if read.Int() <= 0 && write.Int() <= 0 {
		anthropicRead := firstExistingResult(details.Get("cache_read_input_tokens"), usageNode.Get("cache_read_input_tokens"))
		anthropicWrite := firstExistingResult(details.Get("cache_creation_input_tokens"), usageNode.Get("cache_creation_input_tokens"))
		if anthropicRead.Exists() || anthropicWrite.Exists() {
			read, write = anthropicRead, anthropicWrite
			whole = detail.InputTokens >= max(read.Int(), 0)+max(write.Int(), 0)
		}
	}
	if !read.Exists() && !write.Exists() {
		return
	}
	detail.CacheReported = true
	detail.CacheReadTokens = max(read.Int(), 0)
	detail.CacheWriteTokens = max(write.Int(), 0)
	detail.CacheReadIncludedInInput = whole
	if whole {
		detail.InputTokens = max(detail.InputTokens-detail.CacheWriteTokens, 0)
	}
	detail.CachedTokens = detail.CacheReadTokens
	if detail.CachedTokens == 0 {
		detail.CachedTokens = detail.CacheWriteTokens
	}
}

func firstExistingResult(results ...gjson.Result) gjson.Result {
	for _, result := range results {
		if result.Exists() {
			return result
		}
	}
	return gjson.Result{}
}

// applyClaudeCacheUsage reads Anthropic's two cache counters. Its input_tokens
// counts neither of them, so reads are not a subset of InputTokens.
//
// CachedTokens keeps its legacy meaning, reads or else writes, for the
// request_logs.cached_tokens column and the clients that still show it. Hit
// rates are computed from CacheReadTokens alone: a turn that only wrote the
// cache is a miss, not a hit.
//
// cache_creation splits the writes by how long the entry lives; an entry kept
// for an hour costs more to write than one kept for five minutes.
func applyClaudeCacheUsage(detail *usage.Detail, usageNode gjson.Result) {
	cacheRead := usageNode.Get("cache_read_input_tokens")
	cacheCreate := usageNode.Get("cache_creation_input_tokens")
	split := usageNode.Get("cache_creation")
	detail.CacheReported = cacheRead.Exists() || cacheCreate.Exists() || split.Exists()
	if cacheRead.Int() > 0 {
		detail.CacheReadTokens = cacheRead.Int()
		detail.CachedTokens = detail.CacheReadTokens
	}
	writes := cacheCreate.Int()
	written1h := max(split.Get("ephemeral_1h_input_tokens").Int(), 0)
	if writes <= 0 {
		writes = max(split.Get("ephemeral_5m_input_tokens").Int(), 0) + written1h
	}
	if writes > 0 {
		detail.CacheWriteTokens = writes
		detail.CacheWrite1hTokens = min(written1h, writes)
		if detail.CachedTokens == 0 {
			detail.CachedTokens = detail.CacheWriteTokens
		}
	}
}

func parseClaudeUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	return claudeUsageDetail(usageNode)
}

func claudeUsageDetail(usageNode gjson.Result) usage.Detail {
	detail := usage.Detail{
		InputTokens:  usageNode.Get("input_tokens").Int(),
		OutputTokens: usageNode.Get("output_tokens").Int(),
	}
	applyClaudeCacheUsage(&detail, usageNode)
	detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	return detail
}

// parseClaudeStreamUsage reads the usage of a single stream line; see
// claudeStreamUsage for a whole stream.
func parseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	return (&claudeStreamUsage{}).observe(line)
}

// claudeStreamUsage reads the usage of one Claude Messages stream.
//
// A stream reports its counts twice. message_start carries the prompt's: the
// input, the cache reads and writes, and the split of the writes by cache
// lifetime. message_delta carries the final ones, but how many of them depends
// on the upstream: Anthropic repeats every count except the split, while
// Bedrock and some relays send output_tokens alone. The counts are cumulative,
// so each is the larger of the two events' values. The detail is only
// returned for message_delta: the reporter keeps the first detail it gets.
type claudeStreamUsage struct {
	start usage.Detail
}

func (u *claudeStreamUsage) observe(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	if gjson.GetBytes(payload, "type").String() == "message_start" {
		if node := gjson.GetBytes(payload, "message.usage"); node.Exists() {
			u.start = claudeUsageDetail(node)
		}
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	detail, start := claudeUsageDetail(usageNode), u.start
	detail.InputTokens = max(detail.InputTokens, start.InputTokens)
	detail.OutputTokens = max(detail.OutputTokens, start.OutputTokens)
	detail.CacheReadTokens = max(detail.CacheReadTokens, start.CacheReadTokens)
	detail.CacheWriteTokens = max(detail.CacheWriteTokens, start.CacheWriteTokens)
	detail.CacheWrite1hTokens = min(max(detail.CacheWrite1hTokens, start.CacheWrite1hTokens), detail.CacheWriteTokens)
	detail.CacheReported = detail.CacheReported || start.CacheReported
	detail.CachedTokens = detail.CacheReadTokens
	if detail.CachedTokens == 0 {
		detail.CachedTokens = detail.CacheWriteTokens
	}
	detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	return detail, true
}

func parseGeminiFamilyUsageDetail(node gjson.Result) usage.Detail {
	detail := usage.Detail{
		InputTokens:     node.Get("promptTokenCount").Int(),
		OutputTokens:    node.Get("candidatesTokenCount").Int(),
		ReasoningTokens: node.Get("thoughtsTokenCount").Int(),
		TotalTokens:     node.Get("totalTokenCount").Int(),
		CachedTokens:    node.Get("cachedContentTokenCount").Int(),
	}
	// promptTokenCount is the whole prompt, the cached part included ("when
	// cachedContent is set, this is still the total effective prompt size"),
	// so a cache read is a subset of InputTokens and must not be billed twice.
	// Gemini drops zero-valued fields, so an absent cachedContentTokenCount
	// means nothing was read from the cache: the accounting is still reported.
	detail.CacheReported = true
	detail.CacheReadIncludedInInput = true
	if cached := node.Get("cachedContentTokenCount"); cached.Int() > 0 {
		detail.CacheReadTokens = cached.Int()
		detail.CachedTokens = detail.CacheReadTokens
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	return detail
}

func parseGeminiCLIUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("response.usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("response.usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseGeminiUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseGeminiStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func parseGeminiCLIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "response.usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func parseAntigravityUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("response.usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("usageMetadata")
	}
	if !node.Exists() {
		node = usageNode.Get("usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseAntigravityStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "response.usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usageMetadata")
	}
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}
