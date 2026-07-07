package config

import (
	"bytes"
	"encoding/json"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

// SanitizePayloadRules validates raw JSON payload rule params and drops invalid rules.
func (cfg *Config) SanitizePayloadRules() {
	if cfg == nil {
		return
	}
	cfg.Payload.DefaultRaw = sanitizePayloadRawRules(cfg.Payload.DefaultRaw, "default-raw")
	cfg.Payload.OverrideRaw = sanitizePayloadRawRules(cfg.Payload.OverrideRaw, "override-raw")
}

func sanitizePayloadRawRules(rules []PayloadRule, section string) []PayloadRule {
	if len(rules) == 0 {
		return rules
	}
	out := make([]PayloadRule, 0, len(rules))
	for i := range rules {
		rule := rules[i]
		if len(rule.Params) == 0 {
			continue
		}
		invalid := false
		for path, value := range rule.Params {
			raw, ok := payloadRawString(value)
			if !ok {
				continue
			}
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || !json.Valid(trimmed) {
				log.WithFields(log.Fields{
					"section":    section,
					"rule_index": i + 1,
					"param":      path,
				}).Warn("payload rule dropped: invalid raw JSON")
				invalid = true
				break
			}
		}
		if invalid {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func payloadRawString(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		return nil, false
	}
}

// SanitizeAutoUpdate normalizes Docker update settings while preserving an explicit disabled flag.
func (cfg *Config) SanitizeAutoUpdate() {
	if cfg == nil {
		return
	}
	channel := strings.ToLower(strings.TrimSpace(cfg.AutoUpdate.Channel))
	switch channel {
	case "":
		cfg.AutoUpdate.Channel = DefaultAutoUpdateChannel
	case "main", "dev", "auto":
		cfg.AutoUpdate.Channel = channel
	default:
		cfg.AutoUpdate.Channel = DefaultAutoUpdateChannel
	}
	cfg.AutoUpdate.Repository = strings.TrimSpace(cfg.AutoUpdate.Repository)
	if cfg.AutoUpdate.Repository == "" {
		cfg.AutoUpdate.Repository = DefaultAutoUpdateRepository
	}
	cfg.AutoUpdate.DockerImage = strings.TrimSpace(cfg.AutoUpdate.DockerImage)
	if cfg.AutoUpdate.DockerImage == "" {
		cfg.AutoUpdate.DockerImage = DefaultAutoUpdateDockerImage
	}
	cfg.AutoUpdate.UpdaterURL = strings.TrimSpace(cfg.AutoUpdate.UpdaterURL)
	if cfg.AutoUpdate.UpdaterURL == "" {
		cfg.AutoUpdate.UpdaterURL = DefaultAutoUpdateUpdaterURL
	}
}

// SanitizeOAuthModelAlias normalizes and deduplicates global OAuth model name aliases.
// It trims whitespace, normalizes channel keys to lower-case, drops empty entries,
// allows multiple aliases per upstream name, and ensures aliases are unique within each channel.
func (cfg *Config) SanitizeOAuthModelAlias() {
	if cfg == nil || len(cfg.OAuthModelAlias) == 0 {
		return
	}
	out := make(map[string][]OAuthModelAlias, len(cfg.OAuthModelAlias))
	for rawChannel, aliases := range cfg.OAuthModelAlias {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(aliases) == 0 {
			continue
		}
		seenAlias := make(map[string]struct{}, len(aliases))
		clean := make([]OAuthModelAlias, 0, len(aliases))
		for _, entry := range aliases {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			if strings.EqualFold(name, alias) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, ok := seenAlias[aliasKey]; ok {
				continue
			}
			seenAlias[aliasKey] = struct{}{}
			clean = append(clean, OAuthModelAlias{Name: name, Alias: alias, Fork: entry.Fork})
		}
		if len(clean) > 0 {
			out[channel] = clean
		}
	}
	cfg.OAuthModelAlias = out
}

// SanitizeOpenAICompatibility removes OpenAI-compatibility provider entries that are
// not actionable, specifically those missing a BaseURL. It trims whitespace before
// evaluation and preserves the relative order of remaining entries.
func (cfg *Config) SanitizeOpenAICompatibility() {
	if cfg == nil || len(cfg.OpenAICompatibility) == 0 {
		return
	}
	out := make([]OpenAICompatibility, 0, len(cfg.OpenAICompatibility))
	for i := range cfg.OpenAICompatibility {
		e := cfg.OpenAICompatibility[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Prefix = normalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.Headers = NormalizeHeaders(e.Headers)
		for j := range e.APIKeyEntries {
			e.APIKeyEntries[j].ProxyURL = strings.TrimSpace(e.APIKeyEntries[j].ProxyURL)
			e.APIKeyEntries[j].ProxyID = strings.TrimSpace(e.APIKeyEntries[j].ProxyID)
		}
		if e.BaseURL == "" {
			continue
		}
		out = append(out, e)
	}
	cfg.OpenAICompatibility = out
}

// SanitizeCodexKeys removes Codex API key entries missing a BaseURL.
// It trims whitespace and preserves order for remaining entries.
func (cfg *Config) SanitizeCodexKeys() {
	if cfg == nil || len(cfg.CodexKey) == 0 {
		return
	}
	out := make([]CodexKey, 0, len(cfg.CodexKey))
	for i := range cfg.CodexKey {
		e := cfg.CodexKey[i]
		e.Prefix = normalizeModelPrefix(e.Prefix)
		e.BaseURL = strings.TrimSpace(e.BaseURL)
		e.ProxyURL = strings.TrimSpace(e.ProxyURL)
		e.ProxyID = strings.TrimSpace(e.ProxyID)
		e.Headers = NormalizeHeaders(e.Headers)
		e.ExcludedModels = NormalizeExcludedModels(e.ExcludedModels)
		if e.BaseURL == "" {
			continue
		}
		out = append(out, e)
	}
	cfg.CodexKey = out
}

// SanitizeClaudeKeys removes empty Claude API-key rows and normalizes headers.
func (cfg *Config) SanitizeClaudeKeys() {
	if cfg == nil || len(cfg.ClaudeKey) == 0 {
		return
	}
	out := make([]ClaudeKey, 0, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		entry := cfg.ClaudeKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ProxyID = strings.TrimSpace(entry.ProxyID)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		out = append(out, entry)
	}
	cfg.ClaudeKey = out
}

// SanitizeOpenCodeGoKeys deduplicates and normalizes OpenCode Go credentials.
func (cfg *Config) SanitizeOpenCodeGoKeys() {
	if cfg == nil || len(cfg.OpenCodeGoKey) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(cfg.OpenCodeGoKey))
	out := make([]OpenCodeGoKey, 0, len(cfg.OpenCodeGoKey))
	for i := range cfg.OpenCodeGoKey {
		entry := cfg.OpenCodeGoKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		if _, exists := seen[entry.APIKey]; exists {
			continue
		}
		seen[entry.APIKey] = struct{}{}
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ProxyID = strings.TrimSpace(entry.ProxyID)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.Models = NormalizeOpenCodeGoModels(entry.Models)
		entry.ExcludedModels = NormalizeProviderModelAccessExcludedModels(entry.ExcludedModels)
		entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
		entry.WorkspaceID = strings.TrimSpace(entry.WorkspaceID)
		entry.AuthCookie = strings.TrimSpace(entry.AuthCookie)
		out = append(out, entry)
	}
	cfg.OpenCodeGoKey = out
}

func NormalizeOpenCodeGoModels(models []OpenCodeGoModel) []OpenCodeGoModel {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]OpenCodeGoModel, 0, len(models))
	for i := range models {
		name := strings.TrimSpace(models[i].Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		alias := strings.TrimSpace(models[i].Alias)
		out = append(out, OpenCodeGoModel{Name: name, Alias: alias})
	}
	return out
}

// SanitizeClineKeys deduplicates and normalizes Cline credentials.
func (cfg *Config) SanitizeClineKeys() {
	if cfg == nil || len(cfg.ClineKey) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(cfg.ClineKey))
	out := make([]ClineKey, 0, len(cfg.ClineKey))
	for i := range cfg.ClineKey {
		entry := cfg.ClineKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		if _, exists := seen[entry.APIKey]; exists {
			continue
		}
		seen[entry.APIKey] = struct{}{}
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = normalizeClineBaseURL(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ProxyID = strings.TrimSpace(entry.ProxyID)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.Models = NormalizeClineModels(entry.Models)
		entry.ExcludedModels = NormalizeProviderModelAccessExcludedModels(entry.ExcludedModels)
		entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
		out = append(out, entry)
	}
	cfg.ClineKey = out
}

func NormalizeClineModels(models []ClineModel) []ClineModel {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]ClineModel, 0, len(models))
	for i := range models {
		name := strings.TrimSpace(models[i].Name)
		if name == "" {
			continue
		}
		alias := strings.TrimSpace(models[i].Alias)
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ClineModel{Name: name, Alias: alias})
	}
	return out
}

func normalizeClineBaseURL(baseURL string) string {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		return DefaultClineBaseURL
	}
	return strings.TrimSuffix(base, "/")
}

// SanitizeOllamaCloudKeys deduplicates and normalizes Ollama Cloud credentials.
func (cfg *Config) SanitizeOllamaCloudKeys() {
	if cfg == nil || len(cfg.OllamaCloudKey) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(cfg.OllamaCloudKey))
	out := make([]OllamaCloudKey, 0, len(cfg.OllamaCloudKey))
	for i := range cfg.OllamaCloudKey {
		entry := cfg.OllamaCloudKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.BaseURL = normalizeOllamaCloudBaseURL(entry.BaseURL)
		seenKey := entry.APIKey + "\x00" + entry.BaseURL
		if _, exists := seen[seenKey]; exists {
			continue
		}
		seen[seenKey] = struct{}{}
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ProxyID = strings.TrimSpace(entry.ProxyID)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.Models = NormalizeOllamaCloudModels(entry.Models)
		entry.ExcludedModels = NormalizeProviderModelAccessExcludedModels(entry.ExcludedModels)
		entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
		out = append(out, entry)
	}
	cfg.OllamaCloudKey = out
}

func NormalizeOllamaCloudModels(models []OllamaCloudModel) []OllamaCloudModel {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]OllamaCloudModel, 0, len(models))
	for i := range models {
		name := strings.TrimSpace(models[i].Name)
		if name == "" {
			continue
		}
		alias := strings.TrimSpace(models[i].Alias)
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, OllamaCloudModel{Name: name, Alias: alias})
	}
	return out
}

// NormalizeExcludedModelsForConfiguredModels repairs legacy data where every
// explicitly configured model is also excluded, leaving the key unusable.
func NormalizeExcludedModelsForConfiguredModels(excluded []string, modelNames []string) []string {
	if len(excluded) == 0 || len(modelNames) == 0 {
		return excluded
	}
	configured := make(map[string]struct{}, len(modelNames))
	for _, name := range modelNames {
		key := strings.ToLower(strings.TrimSpace(name))
		if key != "" {
			configured[key] = struct{}{}
		}
	}
	if len(configured) < 2 {
		return excluded
	}
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, model := range excluded {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "*" {
			return excluded
		}
		if key != "" {
			excludedSet[key] = struct{}{}
		}
	}
	for model := range configured {
		if _, exists := excludedSet[model]; !exists {
			return excluded
		}
	}
	out := excluded[:0]
	for _, model := range excluded {
		if _, exists := configured[strings.ToLower(strings.TrimSpace(model))]; exists {
			continue
		}
		out = append(out, model)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeOllamaCloudBaseURL(baseURL string) string {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		return DefaultOllamaCloudBaseURL
	}
	return strings.TrimSuffix(base, "/")
}

// SanitizeGeminiKeys deduplicates and normalizes Gemini credentials.
func (cfg *Config) SanitizeGeminiKeys() {
	if cfg == nil {
		return
	}

	seen := make(map[string]struct{}, len(cfg.GeminiKey))
	out := cfg.GeminiKey[:0]
	for i := range cfg.GeminiKey {
		entry := cfg.GeminiKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ProxyID = strings.TrimSpace(entry.ProxyID)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		if _, exists := seen[entry.APIKey]; exists {
			continue
		}
		seen[entry.APIKey] = struct{}{}
		out = append(out, entry)
	}
	cfg.GeminiKey = out
}

func normalizeModelPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
}

// looksLikeBcrypt returns true if the provided string appears to be a bcrypt hash.
func looksLikeBcrypt(s string) bool {
	return len(s) > 4 && (s[:4] == "$2a$" || s[:4] == "$2b$" || s[:4] == "$2y$")
}

// NormalizeHeaders trims header keys and values and removes empty pairs.
func NormalizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	clean := make(map[string]string, len(headers))
	for k, v := range headers {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		clean[key] = val
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// NormalizeExcludedModels trims, lowercases, and deduplicates model exclusion patterns.
// It preserves the order of first occurrences and drops empty entries.
func NormalizeExcludedModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeProviderModelAccessExcludedModels keeps only the existing disable-all
// marker. OpenCode Go, ClinePass and Ollama Cloud use `models` as the per-model
// allowlist; single-model exclusions are legacy dirty data.
func NormalizeProviderModelAccessExcludedModels(models []string) []string {
	for _, model := range NormalizeExcludedModels(models) {
		if model == "*" {
			return []string{"*"}
		}
	}
	return nil
}

// NormalizeOAuthExcludedModels cleans provider -> excluded models mappings by normalizing provider keys
// and applying model exclusion normalization to each entry.
func NormalizeOAuthExcludedModels(entries map[string][]string) map[string][]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for provider, models := range entries {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		normalized := NormalizeExcludedModels(models)
		if len(normalized) == 0 {
			continue
		}
		out[key] = normalized
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hashSecret hashes the given secret using bcrypt.
func hashSecret(secret string) (string, error) {
	hashedBytes, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashedBytes), nil
}
