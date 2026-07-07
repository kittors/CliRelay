package synthesizer

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/diff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// ConfigSynthesizer generates Auth entries from configuration API keys.
// It handles Gemini, Claude, Bedrock, Codex, OpenCode Go, Cline, Ollama Cloud, OpenAI-compat, and Vertex-compat providers.
type ConfigSynthesizer struct{}

// NewConfigSynthesizer creates a new ConfigSynthesizer instance.
func NewConfigSynthesizer() *ConfigSynthesizer {
	return &ConfigSynthesizer{}
}

// Synthesize generates Auth entries from config API keys.
func (s *ConfigSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 32)
	if ctx == nil || ctx.Config == nil {
		return out, nil
	}

	// Gemini API Keys
	out = append(out, s.synthesizeGeminiKeys(ctx)...)
	// Claude API Keys
	out = append(out, s.synthesizeClaudeKeys(ctx)...)
	// AWS Bedrock Runtime credentials
	out = append(out, s.synthesizeBedrockKeys(ctx)...)
	// Codex API Keys
	out = append(out, s.synthesizeCodexKeys(ctx)...)
	// OpenCode Go API Keys
	out = append(out, s.synthesizeOpenCodeGoKeys(ctx)...)
	// Cline API Keys
	out = append(out, s.synthesizeClineKeys(ctx)...)
	// Ollama Cloud API Keys
	out = append(out, s.synthesizeOllamaCloudKeys(ctx)...)
	// OpenAI-compat
	out = append(out, s.synthesizeOpenAICompat(ctx)...)
	// Vertex-compat
	out = append(out, s.synthesizeVertexCompat(ctx)...)

	return out, nil
}

// synthesizeGeminiKeys creates Auth entries for Gemini API keys.
func (s *ConfigSynthesizer) synthesizeGeminiKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.GeminiKey))
	for i := range cfg.GeminiKey {
		entry := cfg.GeminiKey[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		base := strings.TrimSpace(entry.BaseURL)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		proxyID := strings.TrimSpace(entry.ProxyID)
		id, token := idGen.Next("gemini:apikey", key, base)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:gemini[%s]", token),
			"api_key": key,
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeGeminiModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		label := strings.TrimSpace(entry.Name)
		if label == "" {
			label = "gemini-apikey"
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "gemini",
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			ProxyID:    proxyID,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		ApplyDisableAllModelsState(a, entry.ExcludedModels)
		out = append(out, a)
	}
	return out
}

// synthesizeClaudeKeys creates Auth entries for Claude API keys.
func (s *ConfigSynthesizer) synthesizeClaudeKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		ck := cfg.ClaudeKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		base := strings.TrimSpace(ck.BaseURL)
		id, token := idGen.Next("claude:apikey", key, base)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:claude[%s]", token),
			"api_key": key,
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeClaudeModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		if ck.SkipAnthropicProcessing {
			attrs["skip_anthropic_processing"] = "true"
		}
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		proxyID := strings.TrimSpace(ck.ProxyID)
		label := strings.TrimSpace(ck.Name)
		if label == "" {
			label = "claude-apikey"
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "claude",
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			ProxyID:    proxyID,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		ApplyDisableAllModelsState(a, ck.ExcludedModels)
		out = append(out, a)
	}
	return out
}

// synthesizeBedrockKeys creates Auth entries for AWS Bedrock Runtime credentials.
func (s *ConfigSynthesizer) synthesizeBedrockKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.BedrockKey))
	for i := range cfg.BedrockKey {
		entry := cfg.BedrockKey[i]
		authMode := strings.ToLower(strings.TrimSpace(entry.AuthMode))
		switch authMode {
		case "apikey", "api_key", "api-key":
			authMode = "api-key"
		default:
			authMode = "sigv4"
		}

		region := strings.TrimSpace(entry.Region)
		if region == "" {
			region = "us-east-1"
		}
		base := strings.TrimSpace(entry.BaseURL)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		proxyID := strings.TrimSpace(entry.ProxyID)
		prefix := strings.TrimSpace(entry.Prefix)

		attrs := map[string]string{
			"auth_mode": authMode,
			"region":    region,
		}
		idParts := []string{authMode, region, base, proxyURL}
		switch authMode {
		case "api-key":
			key := strings.TrimSpace(entry.APIKey)
			if key == "" {
				continue
			}
			attrs["api_key"] = key
			idParts = append(idParts, key)
		default:
			accessKeyID := strings.TrimSpace(entry.AccessKeyID)
			secretAccessKey := strings.TrimSpace(entry.SecretAccessKey)
			if accessKeyID == "" || secretAccessKey == "" {
				continue
			}
			attrs["api_key"] = accessKeyID
			attrs["access_key_id"] = accessKeyID
			attrs["secret_access_key"] = secretAccessKey
			if sessionToken := strings.TrimSpace(entry.SessionToken); sessionToken != "" {
				attrs["session_token"] = sessionToken
			}
			idParts = append(idParts, accessKeyID, secretAccessKey, strings.TrimSpace(entry.SessionToken))
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if entry.ForceGlobal {
			attrs["force_global"] = "true"
		}
		if hash := diff.ComputeBedrockModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		id, token := idGen.Next("bedrock:apikey", idParts...)
		attrs["source"] = fmt.Sprintf("config:bedrock[%s]", token)
		label := strings.TrimSpace(entry.Name)
		if label == "" {
			label = "bedrock-apikey"
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "bedrock",
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			ProxyID:    proxyID,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		ApplyDisableAllModelsState(a, entry.ExcludedModels)
		out = append(out, a)
	}
	return out
}

// synthesizeCodexKeys creates Auth entries for Codex API keys.
func (s *ConfigSynthesizer) synthesizeCodexKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.CodexKey))
	for i := range cfg.CodexKey {
		ck := cfg.CodexKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		id, token := idGen.Next("codex:apikey", key, ck.BaseURL)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:codex[%s]", token),
			"api_key": key,
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		if ck.BaseURL != "" {
			attrs["base_url"] = ck.BaseURL
		}
		if ck.Websockets {
			attrs["websockets"] = "true"
		}
		if hash := diff.ComputeCodexModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		proxyID := strings.TrimSpace(ck.ProxyID)
		label := strings.TrimSpace(ck.Name)
		if label == "" {
			label = "codex-apikey"
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "codex",
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			ProxyID:    proxyID,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		ApplyDisableAllModelsState(a, ck.ExcludedModels)
		out = append(out, a)
	}
	return out
}

// synthesizeOpenCodeGoKeys creates Auth entries for OpenCode Go API keys.
func (s *ConfigSynthesizer) synthesizeOpenCodeGoKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.OpenCodeGoKey))
	for i := range cfg.OpenCodeGoKey {
		entry := cfg.OpenCodeGoKey[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		proxyID := strings.TrimSpace(entry.ProxyID)
		id, token := idGen.Next("opencode-go:apikey", key, proxyURL)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:opencode-go[%s]", token),
			"api_key": key,
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if hash := diff.ComputeOpenCodeGoModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		if visionFallbackModel := strings.TrimSpace(entry.VisionFallbackModel); visionFallbackModel != "" {
			attrs["vision_fallback_model"] = visionFallbackModel
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		label := strings.TrimSpace(entry.Name)
		if label == "" {
			label = "opencode-go-apikey"
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "opencode-go",
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			ProxyID:    proxyID,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		ApplyConfigDisabledState(a, entry.Disabled)
		out = append(out, a)
	}
	return out
}

// synthesizeClineKeys creates Auth entries for ClinePass API keys.
func (s *ConfigSynthesizer) synthesizeClineKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.ClineKey))
	for i := range cfg.ClineKey {
		entry := cfg.ClineKey[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		base := strings.TrimSpace(entry.BaseURL)
		if base == "" {
			base = config.DefaultClineBaseURL
		}
		base = strings.TrimSuffix(base, "/")
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		proxyID := strings.TrimSpace(entry.ProxyID)
		id, token := idGen.Next("cline:apikey", key, base, proxyURL)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:cline[%s]", token),
			"api_key":      key,
			"base_url":     base,
			"compat_name":  "ClinePass",
			"provider_key": "cline",
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if hash := diff.ComputeClineModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		if visionFallbackModel := strings.TrimSpace(entry.VisionFallbackModel); visionFallbackModel != "" {
			attrs["vision_fallback_model"] = visionFallbackModel
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		label := strings.TrimSpace(entry.Name)
		if label == "" {
			label = "cline-apikey"
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "cline",
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			ProxyID:    proxyID,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		ApplyConfigDisabledState(a, entry.Disabled)
		out = append(out, a)
	}
	return out
}

// synthesizeOllamaCloudKeys creates Auth entries for Ollama Cloud API keys.
func (s *ConfigSynthesizer) synthesizeOllamaCloudKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.OllamaCloudKey))
	for i := range cfg.OllamaCloudKey {
		entry := cfg.OllamaCloudKey[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		base := strings.TrimSpace(entry.BaseURL)
		if base == "" {
			base = config.DefaultOllamaCloudBaseURL
		}
		base = strings.TrimSuffix(base, "/")
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		proxyID := strings.TrimSpace(entry.ProxyID)
		id, token := idGen.Next("ollama-cloud:apikey", key, base, proxyURL)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:ollama-cloud[%s]", token),
			"api_key":      key,
			"base_url":     base,
			"compat_name":  "Ollama Cloud",
			"provider_key": "ollama-cloud",
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if hash := diff.ComputeOllamaCloudModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		if visionFallbackModel := strings.TrimSpace(entry.VisionFallbackModel); visionFallbackModel != "" {
			attrs["vision_fallback_model"] = visionFallbackModel
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		label := strings.TrimSpace(entry.Name)
		if label == "" {
			label = "ollama-cloud-apikey"
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "ollama-cloud",
			Label:      label,
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			ProxyID:    proxyID,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		ApplyConfigDisabledState(a, entry.Disabled)
		out = append(out, a)
	}
	return out
}

// synthesizeOpenAICompat creates Auth entries for OpenAI-compatible providers.
func (s *ConfigSynthesizer) synthesizeOpenAICompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0)
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		prefix := strings.TrimSpace(compat.Prefix)
		providerName := strings.ToLower(strings.TrimSpace(compat.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		base := strings.TrimSpace(compat.BaseURL)

		// Handle new APIKeyEntries format (preferred)
		hasAPIKeyEntries := len(compat.APIKeyEntries) > 0
		for j := range compat.APIKeyEntries {
			entry := &compat.APIKeyEntries[j]
			if entry.Disabled {
				continue
			}
			key := strings.TrimSpace(entry.APIKey)
			proxyURL := strings.TrimSpace(entry.ProxyURL)
			proxyID := strings.TrimSpace(entry.ProxyID)
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.Next(idKind, key, base, proxyURL)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if key != "" {
				attrs["api_key"] = key
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			a := &coreauth.Auth{
				ID:         id,
				Provider:   providerName,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				ProxyURL:   proxyURL,
				ProxyID:    proxyID,
				Attributes: attrs,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			out = append(out, a)
		}
		// Fallback: create entry without API key if no APIKeyEntries
		if !hasAPIKeyEntries {
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.Next(idKind, base)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			a := &coreauth.Auth{
				ID:         id,
				Provider:   providerName,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				Attributes: attrs,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			out = append(out, a)
		}
	}
	return out
}

// synthesizeVertexCompat creates Auth entries for Vertex-compatible providers.
func (s *ConfigSynthesizer) synthesizeVertexCompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.VertexCompatAPIKey))
	for i := range cfg.VertexCompatAPIKey {
		compat := &cfg.VertexCompatAPIKey[i]
		providerName := "vertex"
		base := strings.TrimSpace(compat.BaseURL)

		key := strings.TrimSpace(compat.APIKey)
		prefix := strings.TrimSpace(compat.Prefix)
		proxyURL := strings.TrimSpace(compat.ProxyURL)
		proxyID := strings.TrimSpace(compat.ProxyID)
		idKind := "vertex:apikey"
		id, token := idGen.Next(idKind, key, base, proxyURL)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:vertex-apikey[%s]", token),
			"base_url":     base,
			"provider_key": providerName,
		}
		if compat.Priority != 0 {
			attrs["priority"] = strconv.Itoa(compat.Priority)
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if hash := diff.ComputeVertexCompatModelsHash(compat.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(compat.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   providerName,
			Label:      "vertex-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			ProxyID:    proxyID,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, nil, "apikey")
		out = append(out, a)
	}
	return out
}
