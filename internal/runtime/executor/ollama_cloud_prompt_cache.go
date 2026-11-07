package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// Ollama never reports prompt-cache counters. The native /api/chat response only
// carries prompt_eval_count, and both the OpenAI- and Anthropic-compatible
// endpoints mirror that shape, so every Ollama Cloud request used to log zero
// cached tokens even though the runner keeps a prefix cache alive between turns
// (see ollamaCloudNativeKeepAlive). We derive a cache-read estimate from the
// stable session prefix instead, and only when the upstream reported nothing of
// its own — if Ollama ever starts publishing real counters they win.

const (
	// A prefix shorter than this is noise (shared system preamble at most), not a
	// reused conversation, so it must not be reported as a cache hit.
	ollamaPromptCacheMinPrefixChars = 4096
	// Prefix window used to derive a session key when the client sends no session
	// identifier: the opening of a conversation (system prompt plus first turn) is
	// stable across turns while differing between conversations.
	ollamaPromptCacheFingerprintChars = 2048
	ollamaPromptCacheTTL              = 30 * time.Minute
)

type ollamaPromptCacheEntry struct {
	prompt string
	// commonLen of the request that stored this prompt, replayed when the exact
	// same prompt comes back (see newOllamaPromptCacheEstimator).
	commonLen int
	expiresAt time.Time
}

var ollamaPromptCache sync.Map

// ollamaPromptCacheEstimator holds one request's cache-read estimate. It is
// created once per request, before the upstream call: construction compares the
// prompt against the previous turn of the same session and immediately stores
// the new prompt, so later usage events (streaming emits several) can be scored
// without re-reading a cache the current request has already overwritten.
type ollamaPromptCacheEstimator struct {
	promptLen int
	commonLen int
}

type ollamaPromptCacheContextKey struct{}

func contextWithOllamaPromptCacheEstimator(ctx context.Context, estimator *ollamaPromptCacheEstimator) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if estimator == nil {
		return ctx
	}
	return context.WithValue(ctx, ollamaPromptCacheContextKey{}, estimator)
}

func ollamaPromptCacheEstimatorFromContext(ctx context.Context) *ollamaPromptCacheEstimator {
	if ctx == nil {
		return nil
	}
	estimator, _ := ctx.Value(ollamaPromptCacheContextKey{}).(*ollamaPromptCacheEstimator)
	return estimator
}

func newOllamaPromptCacheEstimator(cacheKey, prompt string) *ollamaPromptCacheEstimator {
	cacheKey = strings.TrimSpace(cacheKey)
	if cacheKey == "" || strings.TrimSpace(prompt) == "" {
		return nil
	}
	now := time.Now()
	commonLen := 0
	if raw, ok := ollamaPromptCache.Load(cacheKey); ok {
		if entry, _ := raw.(ollamaPromptCacheEntry); now.Before(entry.expiresAt) {
			if entry.prompt == prompt {
				// Identical prompt on the same key means this request is being retried
				// (same account, new attempt), not a new turn. Scoring it against itself
				// would claim a 100% cache hit, so replay the original estimate.
				commonLen = entry.commonLen
			} else {
				commonLen = commonPrefixLen(entry.prompt, prompt)
			}
		}
	}
	ollamaPromptCache.Store(cacheKey, ollamaPromptCacheEntry{
		prompt:    prompt,
		commonLen: commonLen,
		expiresAt: now.Add(ollamaPromptCacheTTL),
	})
	return &ollamaPromptCacheEstimator{promptLen: len(prompt), commonLen: commonLen}
}

// estimate converts the shared prefix into a token count, proportionally to the
// prompt tokens the upstream did report for this request.
func (e *ollamaPromptCacheEstimator) estimate(inputTokens int64) int64 {
	if e == nil || inputTokens <= 0 || e.promptLen <= 0 {
		return 0
	}
	if e.commonLen < ollamaPromptCacheMinPrefixChars {
		return 0
	}
	cached := inputTokens * int64(e.commonLen) / int64(e.promptLen)
	if cached > inputTokens {
		cached = inputTokens
	}
	return cached
}

// applyOllamaPromptCacheEstimate fills the cache-read counters of a usage detail
// that the provider left empty. Called from the usage reporter so it covers every
// Ollama Cloud path: native /api/chat, the Anthropic-compatible /messages
// executor, and the OpenAI-compatible executor it delegates to.
func applyOllamaPromptCacheEstimate(ctx context.Context, detail coreusage.Detail) coreusage.Detail {
	if detail.CachedTokens > 0 || detail.CacheReadTokens > 0 || detail.CacheWriteTokens > 0 {
		return detail
	}
	estimator := ollamaPromptCacheEstimatorFromContext(ctx)
	if estimator == nil {
		return detail
	}
	// The estimate stands in for the counters Ollama never sends, so a turn
	// with no reusable prefix is a known miss rather than an unknown.
	detail.CacheReported = true
	cached := estimator.estimate(detail.InputTokens)
	if cached <= 0 {
		return detail
	}
	detail.CacheReadTokens = cached
	detail.CachedTokens = cached
	// Ollama counts the whole prompt in prompt_eval_count / input_tokens, so the
	// cached part is included rather than additive.
	detail.CacheReadIncludedInInput = true
	return detail
}

// withOllamaPromptCacheEstimator attaches one estimate to the request context.
// It is installed at the executor entry points so the native, Anthropic- and
// OpenAI-compatible branches all score the same prompt exactly once.
func withOllamaPromptCacheEstimator(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) context.Context {
	prompt := ollamaPromptCacheText(req.Payload)
	if prompt == "" {
		return ctx
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	key := ollamaPromptCacheKey(auth, model, req.Payload, opts, prompt)
	return contextWithOllamaPromptCacheEstimator(ctx, newOllamaPromptCacheEstimator(key, prompt))
}

// ollamaPromptCacheKey scopes the estimate to one conversation on one account.
// Clients that send a session identifier get the shared session key; the rest
// fall back to a fingerprint of the prompt opening, which is stable across the
// turns of a conversation and distinct between conversations.
func ollamaPromptCacheKey(auth *cliproxyauth.Auth, model string, payload []byte, opts cliproxyexecutor.Options, prompt string) string {
	seed := sessionPromptCacheSeed(opts, payload)
	if seed == "" {
		seed = strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String())
	}
	if seed == "" {
		if prompt = strings.TrimSpace(prompt); prompt == "" {
			return ""
		}
		head := prompt
		if len(head) > ollamaPromptCacheFingerprintChars {
			head = head[:ollamaPromptCacheFingerprintChars]
		}
		sum := sha256.Sum256([]byte(head))
		seed = "prompt-prefix:" + hex.EncodeToString(sum[:16])
	}
	return scopedPromptCacheKey(auth, model, seed)
}

// ollamaPromptCacheText flattens the conversation of a request payload into the
// text used for prefix comparison. It accepts every source format Ollama Cloud
// serves (Claude messages, OpenAI chat, OpenAI responses, Gemini contents) since
// the executor sees the client's original payload, not a normalized one.
func ollamaPromptCacheText(payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	root := gjson.ParseBytes(payload)
	var builder strings.Builder
	appendNode(&builder, root.Get("system"))
	appendNode(&builder, root.Get("system_instruction"))
	appendNode(&builder, root.Get("systemInstruction"))
	for _, key := range []string{"messages", "input", "contents"} {
		node := root.Get(key)
		if !node.Exists() {
			continue
		}
		appendNode(&builder, node)
		break
	}
	return builder.String()
}

// appendNode walks the message tree and keeps only textual leaves, so that
// per-request noise such as ids or booleans cannot break the shared prefix.
func appendNode(builder *strings.Builder, node gjson.Result) {
	if !node.Exists() {
		return
	}
	switch {
	case node.IsArray():
		node.ForEach(func(_, value gjson.Result) bool {
			appendNode(builder, value)
			return true
		})
	case node.IsObject():
		for _, key := range []string{"role", "type", "name", "text", "content", "parts", "arguments", "input"} {
			appendNode(builder, node.Get(key))
		}
	default:
		if text := node.String(); text != "" {
			builder.WriteString(text)
			builder.WriteByte('\n')
		}
	}
}

func commonPrefixLen(a, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}
