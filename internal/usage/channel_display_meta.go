package usage

import "strings"

// Provider / auth-type inference for channel filter chips. Split out of
// request_log_query.go: these are presentation heuristics over a stored row,
// not part of the query path.

// InferChannelDisplayMeta derives provider + auth_type for channel filter chips
// when live auth metadata is missing (historical / deleted credentials).
func InferChannelDisplayMeta(label, source, model, providerHint string) (provider, authType string) {
	provider = normalizeChannelProvider(providerHint)
	label = strings.TrimSpace(label)
	source = strings.TrimSpace(source)
	model = strings.ToLower(strings.TrimSpace(model))

	apiKeySource := looksLikeAPIKeySource(source)
	if provider == "" && !apiKeySource {
		// Never treat raw API keys as provider ids.
		provider = normalizeChannelProvider(guessProviderFromSource(source))
	}
	if provider == "" {
		provider = normalizeChannelProvider(guessProviderFromLabel(label))
	}
	if provider == "" {
		provider = normalizeChannelProvider(guessProviderFromModel(model))
	}

	switch {
	case apiKeySource:
		authType = "api"
	case strings.Contains(label, "@") || strings.Contains(source, "@"):
		authType = "oauth"
	case provider == "opencode-go" || provider == "openai-compatibility":
		authType = "api"
	case provider != "":
		// Named upstream providers default to oauth-style channels when not an API key source.
		authType = "oauth"
	default:
		authType = ""
	}
	return provider, authType
}

func normalizeChannelProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "":
		return ""
	case "openai-compatibility", "openai_compat", "openai-compat":
		return "opencode-go"
	case "opencode", "opencode go", "opencode_go", "opencodego":
		return "opencode-go"
	case "grok":
		return "xai"
	case "gemini-cli", "geminicli":
		return "gemini"
	default:
		return value
	}
}

func guessProviderFromLabel(label string) string {
	key := strings.ToLower(strings.TrimSpace(label))
	switch {
	case key == "":
		return ""
	case key == "opencode go" || strings.HasPrefix(key, "opencode go") || strings.Contains(key, "opencode-go"):
		return "opencode-go"
	case strings.Contains(key, "codex"):
		return "codex"
	case strings.Contains(key, "claude") || strings.Contains(key, "anthropic"):
		return "claude"
	case strings.Contains(key, "gemini") || strings.Contains(key, "antigravity"):
		return "gemini"
	case strings.Contains(key, "xai") || strings.Contains(key, "grok"):
		return "xai"
	case strings.Contains(key, "kimi"):
		return "kimi"
	case strings.Contains(key, "iflow"):
		return "iflow"
	default:
		return ""
	}
}

func guessProviderFromModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case model == "":
		return ""
	case strings.HasPrefix(model, "grok") || strings.Contains(model, "grok-"):
		return "xai"
	case strings.HasPrefix(model, "gpt-") || strings.Contains(model, "codex") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4"):
		return "codex"
	case strings.HasPrefix(model, "claude"):
		return "claude"
	case strings.HasPrefix(model, "gemini") || strings.Contains(model, "antigravity"):
		return "gemini"
	case strings.HasPrefix(model, "kimi") || strings.Contains(model, "moonshot"):
		return "kimi"
	case strings.HasPrefix(model, "deepseek") || strings.HasPrefix(model, "glm") || strings.HasPrefix(model, "qwen") || strings.HasPrefix(model, "minimax") || strings.HasPrefix(model, "mimo"):
		// Common OpenCode Go / compat model ids when channel label is missing.
		return "opencode-go"
	default:
		return ""
	}
}

func looksLikeAPIKeySource(source string) bool {
	source = strings.TrimSpace(source)
	if source == "" {
		return false
	}
	lower := strings.ToLower(source)
	return strings.HasPrefix(lower, "sk-") ||
		strings.HasPrefix(lower, "api-") ||
		strings.HasPrefix(lower, "key-") ||
		strings.HasPrefix(lower, "api_key:") ||
		(len(source) >= 24 && !strings.Contains(source, "@") && !strings.Contains(source, " "))
}

func guessProviderFromSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return ""
	}
	// Source may be an email or a provider id; only return known-looking short keys.
	if strings.Contains(source, "@") || strings.Contains(source, " ") {
		return ""
	}
	if len(source) > 32 {
		return ""
	}
	return source
}
