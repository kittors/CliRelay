package config

import "strings"

func sanitizeCodexKeySlice(entries []CodexKey, requireBaseURL bool, defaultBaseURL string) []CodexKey {
	seen := make(map[string]struct{}, len(entries))
	out := entries[:0]
	for i := range entries {
		entry := entries[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		if entry.APIKey == "" {
			continue
		}
		if entry.BaseURL == "" {
			if requireBaseURL {
				continue
			}
			entry.BaseURL = defaultBaseURL
		}
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ProxyID = strings.TrimSpace(entry.ProxyID)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		key := entry.APIKey + "\x00" + entry.BaseURL
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, entry)
	}
	return out
}

// SanitizeXAIKeys normalizes native xAI API-key credentials.
func (cfg *Config) SanitizeXAIKeys() {
	if cfg == nil {
		return
	}
	cfg.XAIKey = sanitizeCodexKeySlice(cfg.XAIKey, true, "")
}

// SanitizeMetaKeys normalizes native Meta API-key credentials.
func (cfg *Config) SanitizeMetaKeys() {
	if cfg == nil {
		return
	}
	cfg.MetaKey = sanitizeCodexKeySlice(cfg.MetaKey, false, "https://api.meta.ai/v1")
}
