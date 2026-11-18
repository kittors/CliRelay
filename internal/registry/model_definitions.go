// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is stored in model_definitions_static_data.go.
package registry

import (
	"sort"
	"strings"
)

const (
	DefaultClaudeMaxInputTokens  = 200000
	DefaultClaudeMaxOutputTokens = 8192
)

// GetCodexFreeModels returns the Codex model catalog used for free-plan OAuth
// accounts. The local catalog does not split plan tiers, so it includes the
// common OpenAI catalog plus the built-in Codex capability definitions.
func GetCodexFreeModels() []*ModelInfo {
	return codexOAuthModels()
}

// GetCodexTeamModels returns the Codex model catalog used for team-plan OAuth
// accounts.
func GetCodexTeamModels() []*ModelInfo {
	return codexOAuthModels()
}

// GetCodexPlusModels returns the Codex model catalog used for plus-plan OAuth
// accounts.
func GetCodexPlusModels() []*ModelInfo {
	return codexOAuthModels()
}

// GetCodexProModels returns the Codex model catalog used for pro-plan OAuth
// accounts.
func GetCodexProModels() []*ModelInfo {
	return codexOAuthModels()
}

func codexOAuthModels() []*ModelInfo {
	models := append([]*ModelInfo(nil), GetOpenAIModels()...)
	models = append(models, getGPT56ModelDefinitions()...)
	models = append(models, getGPT6ModelDefinitions()...)
	return cloneModelInfosUnique(models)
}

func normalizeAntigravityCapabilityModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if slash := strings.LastIndexByte(modelID, '/'); slash >= 0 {
		modelID = modelID[slash+1:]
	}
	return modelID
}

// AntigravityWebSearchModelFor returns the Antigravity model that should run a
// native web search request for modelID.
func AntigravityWebSearchModelFor(modelID string) string {
	modelID = normalizeAntigravityCapabilityModelID(modelID)
	if modelID == "" {
		return ""
	}
	for _, model := range GetGlobalRegistry().GetAvailableModelsByProvider("antigravity") {
		if model == nil {
			continue
		}
		current := normalizeAntigravityCapabilityModelID(model.ID)
		if current != modelID {
			continue
		}
		if model.SupportsWebSearch || (model.NativeCapabilities != nil && model.NativeCapabilities.WebSearch != nil && *model.NativeCapabilities.WebSearch) {
			return current
		}
		return ""
	}
	return ""
}

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - vertex
//   - gemini-cli
//   - aistudio
//   - bedrock
//   - codex
//   - xai
//   - minimax (image models only)
//   - qwen
//   - iflow
//   - kimi
//   - opencode-go
//   - cline
//   - ollama-cloud
//   - commandcode
//   - antigravity (returns static overrides only)
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "bedrock":
		return GetBedrockModels()
	case "gemini":
		return GetGeminiModels()
	case "vertex":
		return GetGeminiVertexModels()
	case "gemini-cli":
		return GetGeminiCLIModels()
	case "aistudio":
		return GetAIStudioModels()
	case "codex":
		return GetOpenAIModels()
	case "xai", "x-ai", "grok":
		return GetXAIModels()
	case "minimax":
		return GetMiniMaxModels()
	case "qwen":
		return GetQwenModels()
	case "iflow":
		return GetIFlowModels()
	case "kimi":
		return GetKimiModels()
	case "opencode-go":
		return GetOpenCodeGoModels()
	case "cline":
		return GetClineModels()
	case "ollama-cloud":
		return GetOllamaCloudModels()
	case "commandcode":
		return GetCommandCodeModels()
	case "antigravity":
		cfg := GetAntigravityModelConfig()
		if len(cfg) == 0 {
			return nil
		}
		models := make([]*ModelInfo, 0, len(cfg))
		for modelID, entry := range cfg {
			if modelID == "" || entry == nil {
				continue
			}
			models = append(models, &ModelInfo{
				ID:                  modelID,
				Object:              "model",
				OwnedBy:             "antigravity",
				Type:                "antigravity",
				Thinking:            entry.Thinking,
				MaxCompletionTokens: entry.MaxCompletionTokens,
			})
		}
		sort.Slice(models, func(i, j int) bool {
			return strings.ToLower(models[i].ID) < strings.ToLower(models[j].ID)
		})
		return models
	default:
		return nil
	}
}

// GetAntigravityModels returns the built-in Antigravity capability catalog.
func GetAntigravityModels() []*ModelInfo {
	return GetStaticModelDefinitionsByChannel("antigravity")
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	allModels := [][]*ModelInfo{
		GetClaudeModels(),
		GetBedrockModels(),
		GetGeminiModels(),
		GetGeminiVertexModels(),
		GetGeminiCLIModels(),
		GetAIStudioModels(),
		GetOpenAIModels(),
		GetXAIModels(),
		GetMiniMaxModels(),
		GetQwenModels(),
		GetIFlowModels(),
		GetKimiModels(),
		GetOpenCodeGoModels(),
		GetClineModels(),
		GetOllamaCloudModels(),
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return m
			}
		}
	}

	// Check Antigravity static config
	if cfg := GetAntigravityModelConfig()[modelID]; cfg != nil {
		return &ModelInfo{
			ID:                  modelID,
			Thinking:            cfg.Thinking,
			MaxCompletionTokens: cfg.MaxCompletionTokens,
		}
	}

	return nil
}

// LookupStaticModelInfoByChannel resolves a model using one provider channel's
// static catalog.
func LookupStaticModelInfoByChannel(modelID, channel string) *ModelInfo {
	for _, model := range GetStaticModelDefinitionsByChannel(channel) {
		if model != nil && strings.EqualFold(strings.TrimSpace(model.ID), strings.TrimSpace(modelID)) {
			return model
		}
	}
	return nil
}

// GetBedrockModels returns the Claude-family model definitions exposed through AWS Bedrock.
//
// Built from the static snapshot, not GetClaudeModels: the latest-generation ids
// (claude-opus-5, claude-fable-5-1, ...) are served by Anthropic's Claude Code
// OAuth surface and are not exposed through Bedrock, so advertising them here
// would list models the channel cannot serve.
func GetBedrockModels() []*ModelInfo {
	claudeModels := claudeStaticSnapshot()
	if len(claudeModels) == 0 {
		return nil
	}
	out := make([]*ModelInfo, 0, len(claudeModels))
	for _, model := range claudeModels {
		if model == nil {
			continue
		}
		clone := *model
		clone.OwnedBy = "aws"
		clone.Type = "bedrock"
		out = append(out, &clone)
	}
	return out
}
