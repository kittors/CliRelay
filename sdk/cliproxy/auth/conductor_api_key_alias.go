package auth

import (
	"strings"
)

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return resolved
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg := m.currentRuntimeConfig()

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "cline":
		upstreamModel = resolveUpstreamModelForClineAPIKey(cfg, auth, requestedModel)
	case "bedrock":
		upstreamModel = resolveUpstreamModelForBedrockAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return upstreamModel
	}
	if builtIn := resolveBuiltInCodexModelAlias(auth, requestedModel); builtIn != "" {
		return builtIn
	}
	return requestedModel
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *runtimeConfigSnapshot, auth *Auth) *runtimeAPIKeyModelConfig {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *runtimeConfigSnapshot, auth *Auth) *runtimeAPIKeyModelConfig {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *runtimeConfigSnapshot, auth *Auth) *runtimeAPIKeyModelConfig {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveClineAPIKeyConfig(cfg *runtimeConfigSnapshot, auth *Auth) *runtimeAPIKeyModelConfig {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClineKey, auth)
}

func resolveBedrockAPIKeyConfig(cfg *runtimeConfigSnapshot, auth *Auth) *runtimeBedrockKeyConfig {
	if cfg == nil || auth == nil {
		return nil
	}
	attrKey := ""
	attrAccessKeyID := ""
	attrBase := ""
	attrRegion := ""
	attrMode := ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrAccessKeyID = strings.TrimSpace(auth.Attributes["access_key_id"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
		attrRegion = strings.TrimSpace(auth.Attributes["region"])
		attrMode = strings.ToLower(strings.TrimSpace(auth.Attributes["auth_mode"]))
	}
	if attrMode == "apikey" || attrMode == "api_key" {
		attrMode = "api-key"
	}
	for i := range cfg.BedrockKey {
		entry := &cfg.BedrockKey[i]
		cfgMode := strings.ToLower(strings.TrimSpace(entry.AuthMode))
		if cfgMode == "" {
			cfgMode = "sigv4"
		}
		if cfgMode == "apikey" || cfgMode == "api_key" {
			cfgMode = "api-key"
		}
		if attrMode != "" && cfgMode != attrMode {
			continue
		}
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrBase != "" && !strings.EqualFold(cfgBase, attrBase) {
			continue
		}
		cfgRegion := strings.TrimSpace(entry.Region)
		if cfgRegion == "" {
			cfgRegion = "us-east-1"
		}
		if attrRegion != "" && !strings.EqualFold(cfgRegion, attrRegion) {
			continue
		}
		if cfgMode == "api-key" {
			if attrKey != "" && strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
			continue
		}
		cfgAccessKeyID := strings.TrimSpace(entry.AccessKeyID)
		if attrAccessKeyID != "" && strings.EqualFold(cfgAccessKeyID, attrAccessKeyID) {
			return entry
		}
		if attrKey != "" && strings.EqualFold(cfgAccessKeyID, attrKey) {
			return entry
		}
	}
	return nil
}

func resolveVertexAPIKeyConfig(cfg *runtimeConfigSnapshot, auth *Auth) *runtimeAPIKeyModelConfig {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *runtimeConfigSnapshot, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *runtimeConfigSnapshot, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForCodexAPIKey(cfg *runtimeConfigSnapshot, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClineAPIKey(cfg *runtimeConfigSnapshot, auth *Auth, requestedModel string) string {
	entry := resolveClineAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForBedrockAPIKey(cfg *runtimeConfigSnapshot, auth *Auth, requestedModel string) string {
	entry := resolveBedrockAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForVertexAPIKey(cfg *runtimeConfigSnapshot, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *runtimeConfigSnapshot, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(cfg *runtimeConfigSnapshot, providerKey, compatName, authProvider string) *runtimeOpenAICompatibilityConfig {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}
