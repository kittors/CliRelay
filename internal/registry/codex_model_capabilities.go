package registry

import "strings"

// CodexModelCapability is the shared source of truth for Codex-facing model
// metadata. Catalog-only levels may include client modes such as ultra, while
// RuntimeWireReasoningLevels contains only values accepted by the upstream API.
type CodexModelCapability struct {
	ModelID                    string
	CanonicalTarget            string
	DisplayName                string
	Description                string
	ContextWindow              int
	MaxContextWindow           int
	MaxCompletionTokens        int
	DefaultReasoningLevel      string
	CatalogReasoningLevels     []string
	RuntimeWireReasoningLevels []string
	CompatibilityAlias         bool
}

var gpt56CatalogReasoningLevels = []string{"low", "medium", "high", "xhigh", "max", "ultra"}
var gpt56RuntimeWireReasoningLevels = []string{"low", "medium", "high", "xhigh", "max"}
var gpt6CatalogReasoningLevels = []string{"low", "medium", "high", "xhigh", "max", "ultra"}
var gpt6RuntimeWireReasoningLevels = []string{"low", "medium", "high", "xhigh", "max"}

var codexModelCapabilities = map[string]CodexModelCapability{
	"gpt-5.6": {
		ModelID:                    "gpt-5.6",
		CanonicalTarget:            "gpt-5.6-sol",
		DisplayName:                "GPT-5.6",
		Description:                "GPT-5.6 family alias routed to GPT-5.6 Sol.",
		ContextWindow:              1050000,
		MaxContextWindow:           1050000,
		MaxCompletionTokens:        128000,
		DefaultReasoningLevel:      "medium",
		CatalogReasoningLevels:     gpt56CatalogReasoningLevels,
		RuntimeWireReasoningLevels: gpt56RuntimeWireReasoningLevels,
	},
	"gpt-5.6-sol": {
		ModelID:                    "gpt-5.6-sol",
		CanonicalTarget:            "gpt-5.6-sol",
		DisplayName:                "GPT-5.6 Sol",
		Description:                "Frontier GPT-5.6 model for complex professional work.",
		ContextWindow:              1050000,
		MaxContextWindow:           1050000,
		MaxCompletionTokens:        128000,
		DefaultReasoningLevel:      "medium",
		CatalogReasoningLevels:     gpt56CatalogReasoningLevels,
		RuntimeWireReasoningLevels: gpt56RuntimeWireReasoningLevels,
	},
	"gpt-5.6-terra": {
		ModelID:                    "gpt-5.6-terra",
		CanonicalTarget:            "gpt-5.6-terra",
		DisplayName:                "GPT-5.6 Terra",
		Description:                "Balanced GPT-5.6 model for everyday professional work.",
		ContextWindow:              1050000,
		MaxContextWindow:           1050000,
		MaxCompletionTokens:        128000,
		DefaultReasoningLevel:      "medium",
		CatalogReasoningLevels:     gpt56CatalogReasoningLevels,
		RuntimeWireReasoningLevels: gpt56RuntimeWireReasoningLevels,
	},
	"gpt-5.6-luna": {
		ModelID:                    "gpt-5.6-luna",
		CanonicalTarget:            "gpt-5.6-luna",
		DisplayName:                "GPT-5.6 Luna",
		Description:                "Fast and affordable GPT-5.6 model for high-volume work.",
		ContextWindow:              1050000,
		MaxContextWindow:           1050000,
		MaxCompletionTokens:        128000,
		DefaultReasoningLevel:      "medium",
		CatalogReasoningLevels:     gpt56CatalogReasoningLevels,
		RuntimeWireReasoningLevels: gpt56RuntimeWireReasoningLevels,
	},
	"gpt-5.6-ultra": {
		ModelID:                    "gpt-5.6-ultra",
		CanonicalTarget:            "gpt-5.6-sol",
		DisplayName:                "GPT-5.6 Sol (Ultra compatibility alias)",
		Description:                "Compatibility alias for GPT-5.6 Sol with Codex Ultra as the catalog default.",
		ContextWindow:              1050000,
		MaxContextWindow:           1050000,
		MaxCompletionTokens:        128000,
		DefaultReasoningLevel:      "ultra",
		CatalogReasoningLevels:     gpt56CatalogReasoningLevels,
		RuntimeWireReasoningLevels: gpt56RuntimeWireReasoningLevels,
		CompatibilityAlias:         true,
	},
	"gpt-6-astra": {
		ModelID:                    "gpt-6-astra",
		CanonicalTarget:            "gpt-6-astra",
		DisplayName:                "GPT-6 Astra",
		Description:                "OpenAI's flagship GPT-6 Astra model for complex coding and agentic tasks.",
		ContextWindow:              1050000,
		MaxContextWindow:           1050000,
		MaxCompletionTokens:        128000,
		DefaultReasoningLevel:      "medium",
		CatalogReasoningLevels:     gpt6CatalogReasoningLevels,
		RuntimeWireReasoningLevels: gpt6RuntimeWireReasoningLevels,
	},
	"gpt-6-astra-pro": {
		ModelID:                    "gpt-6-astra-pro",
		CanonicalTarget:            "gpt-6-astra-pro",
		DisplayName:                "GPT-6 Astra Pro",
		Description:                "OpenAI's GPT-6 Astra Pro model with enhanced reasoning and throughput.",
		ContextWindow:              1050000,
		MaxContextWindow:           1050000,
		MaxCompletionTokens:        128000,
		DefaultReasoningLevel:      "medium",
		CatalogReasoningLevels:     gpt6CatalogReasoningLevels,
		RuntimeWireReasoningLevels: gpt6RuntimeWireReasoningLevels,
	},
}

// GetCodexModelCapability returns an isolated copy so callers cannot mutate the
// shared reasoning-level slices.
func GetCodexModelCapability(modelID string) (CodexModelCapability, bool) {
	capability, ok := codexModelCapabilities[strings.ToLower(strings.TrimSpace(modelID))]
	if !ok {
		return CodexModelCapability{}, false
	}
	capability.CatalogReasoningLevels = append([]string(nil), capability.CatalogReasoningLevels...)
	capability.RuntimeWireReasoningLevels = append([]string(nil), capability.RuntimeWireReasoningLevels...)
	return capability, true
}

func getGPT56ModelDefinitions() []*ModelInfo {
	modelIDs := []string{"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.6-ultra"}
	models := make([]*ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		capability, _ := GetCodexModelCapability(modelID)
		models = append(models, &ModelInfo{
			ID:                  capability.ModelID,
			Object:              "model",
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             capability.CanonicalTarget,
			DisplayName:         capability.DisplayName,
			UpstreamModelID:     capability.CanonicalTarget,
			Description:         capability.Description,
			ContextLength:       capability.ContextWindow,
			MaxCompletionTokens: capability.MaxCompletionTokens,
			SupportedParameters: []string{"tools"},
			Thinking: &ThinkingSupport{
				Levels: append([]string(nil), capability.RuntimeWireReasoningLevels...),
			},
		})
	}
	return models
}

func getGPT6ModelDefinitions() []*ModelInfo {
	modelIDs := []string{"gpt-6-astra", "gpt-6-astra-pro"}
	models := make([]*ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		capability, _ := GetCodexModelCapability(modelID)
		models = append(models, &ModelInfo{
			ID:                  capability.ModelID,
			Object:              "model",
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             capability.CanonicalTarget,
			DisplayName:         capability.DisplayName,
			UpstreamModelID:     capability.CanonicalTarget,
			Description:         capability.Description,
			ContextLength:       capability.ContextWindow,
			MaxCompletionTokens: capability.MaxCompletionTokens,
			SupportedParameters: []string{"tools"},
			Thinking: &ThinkingSupport{
				Levels: append([]string(nil), capability.RuntimeWireReasoningLevels...),
			},
		})
	}
	return models
}
