package usage

import "strings"

const CcSwitchCodexModelCatalogFilename = "cc-switch-model-catalog.json"

type CcSwitchCodexModelCatalog struct {
	Models []CcSwitchCodexModelCatalogEntry `json:"models"`
}

type CcSwitchCodexReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type CcSwitchCodexServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type CcSwitchCodexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

type CcSwitchCodexModelMessages struct {
	InstructionsTemplate          string                        `json:"instructions_template"`
	InstructionsVariables         map[string]string             `json:"instructions_variables"`
	SupportsReasoningSummaries    bool                          `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary       string                        `json:"default_reasoning_summary"`
	SupportVerbosity              bool                          `json:"support_verbosity"`
	DefaultVerbosity              string                        `json:"default_verbosity"`
	ApplyPatchToolType            string                        `json:"apply_patch_tool_type"`
	WebSearchToolType             string                        `json:"web_search_tool_type"`
	TruncationPolicy              CcSwitchCodexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls     bool                          `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal   bool                          `json:"supports_image_detail_original"`
	ContextWindow                 int                           `json:"context_window"`
	MaxContextWindow              int                           `json:"max_context_window"`
	EffectiveContextWindowPercent int                           `json:"effective_context_window_percent"`
	ExperimentalSupportedTools    []string                      `json:"experimental_supported_tools"`
	InputModalities               []string                      `json:"input_modalities"`
	SupportsSearchTool            bool                          `json:"supports_search_tool"`
	UseResponsesLite              bool                          `json:"use_responses_lite"`
}

type CcSwitchCodexModelCatalogEntry struct {
	Slug                          string                        `json:"slug"`
	Model                         string                        `json:"model"`
	DisplayName                   string                        `json:"display_name"`
	Description                   string                        `json:"description"`
	DefaultReasoningLevel         string                        `json:"default_reasoning_level"`
	SupportedReasoningLevels      []CcSwitchCodexReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                     string                        `json:"shell_type"`
	Visibility                    string                        `json:"visibility"`
	SupportedInAPI                bool                          `json:"supported_in_api"`
	Priority                      int                           `json:"priority"`
	AdditionalSpeedTiers          []string                      `json:"additional_speed_tiers"`
	ServiceTiers                  []CcSwitchCodexServiceTier    `json:"service_tiers"`
	AvailabilityNUX               any                           `json:"availability_nux"`
	Upgrade                       any                           `json:"upgrade"`
	BaseInstructions              string                        `json:"base_instructions"`
	ModelMessages                 CcSwitchCodexModelMessages    `json:"model_messages"`
	SupportsReasoningSummaries    bool                          `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary       string                        `json:"default_reasoning_summary"`
	SupportVerbosity              bool                          `json:"support_verbosity"`
	DefaultVerbosity              string                        `json:"default_verbosity"`
	ApplyPatchToolType            string                        `json:"apply_patch_tool_type"`
	WebSearchToolType             string                        `json:"web_search_tool_type"`
	TruncationPolicy              CcSwitchCodexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls     bool                          `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal   bool                          `json:"supports_image_detail_original"`
	ContextWindow                 int                           `json:"context_window"`
	MaxContextWindow              int                           `json:"max_context_window"`
	EffectiveContextWindowPercent int                           `json:"effective_context_window_percent"`
	ExperimentalSupportedTools    []string                      `json:"experimental_supported_tools"`
	InputModalities               []string                      `json:"input_modalities"`
	SupportsSearchTool            bool                          `json:"supports_search_tool"`
	UseResponsesLite              bool                          `json:"use_responses_lite"`
}

const ccSwitchCodexDefaultContextWindow = 128000
const ccSwitchCodexBaseInstructions = "You are Codex, a coding agent."

var ccSwitchCodexSupportedReasoningLevels = []CcSwitchCodexReasoningLevel{
	{Effort: "low", Description: "Fast responses with lighter reasoning"},
	{Effort: "medium", Description: "Balances speed and reasoning depth for everyday tasks"},
	{Effort: "high", Description: "Greater reasoning depth for complex problems"},
	{Effort: "xhigh", Description: "Extra high reasoning depth for complex problems"},
}

func BuildCcSwitchCodexModelCatalog(row CcSwitchImportConfigRow) *CcSwitchCodexModelCatalog {
	if !strings.EqualFold(strings.TrimSpace(row.ClientType), "codex") {
		return nil
	}

	models := make([]string, 0, len(row.ModelMappings)+1)
	if model := strings.TrimSpace(row.DefaultModel); model != "" {
		models = append(models, model)
	}
	for _, mapping := range row.ModelMappings {
		if strings.TrimSpace(mapping.Role) != "" {
			continue
		}
		model := strings.TrimSpace(mapping.RequestModel)
		if model == "" {
			model = strings.TrimSpace(mapping.TargetModel)
		}
		if model != "" {
			models = append(models, model)
		}
	}

	seen := make(map[string]struct{}, len(models))
	entries := make([]CcSwitchCodexModelCatalogEntry, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		key := strings.ToLower(model)
		if model == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, buildCcSwitchCodexModelCatalogEntry(model, len(entries)))
	}
	if len(entries) == 0 {
		return nil
	}

	return &CcSwitchCodexModelCatalog{Models: entries}
}

func AttachCcSwitchCodexModelCatalog(row CcSwitchImportConfigRow) CcSwitchImportConfigRow {
	catalog := BuildCcSwitchCodexModelCatalog(row)
	if catalog == nil {
		return row
	}
	row.CodexModelCatalogFilename = CcSwitchCodexModelCatalogFilename
	row.CodexModelCatalog = catalog
	return row
}

func AttachCcSwitchCodexModelCatalogs(rows []CcSwitchImportConfigRow) []CcSwitchImportConfigRow {
	if len(rows) == 0 {
		return rows
	}
	out := make([]CcSwitchImportConfigRow, len(rows))
	for i, row := range rows {
		out[i] = AttachCcSwitchCodexModelCatalog(row)
	}
	return out
}

func buildCcSwitchCodexModelCatalogEntry(model string, priority int) CcSwitchCodexModelCatalogEntry {
	messages := CcSwitchCodexModelMessages{
		InstructionsTemplate:          ccSwitchCodexBaseInstructions,
		InstructionsVariables:         map[string]string{},
		SupportsReasoningSummaries:    true,
		DefaultReasoningSummary:       "none",
		SupportVerbosity:              true,
		DefaultVerbosity:              "low",
		ApplyPatchToolType:            "freeform",
		WebSearchToolType:             "text_and_image",
		TruncationPolicy:              CcSwitchCodexTruncationPolicy{Mode: "tokens", Limit: 10000},
		SupportsParallelToolCalls:     true,
		SupportsImageDetailOriginal:   true,
		ContextWindow:                 ccSwitchCodexDefaultContextWindow,
		MaxContextWindow:              ccSwitchCodexDefaultContextWindow,
		EffectiveContextWindowPercent: 95,
		ExperimentalSupportedTools:    []string{},
		InputModalities:               []string{"text", "image"},
		SupportsSearchTool:            true,
		UseResponsesLite:              false,
	}

	return CcSwitchCodexModelCatalogEntry{
		Slug:                          model,
		Model:                         model,
		DisplayName:                   model,
		Description:                   model,
		DefaultReasoningLevel:         "medium",
		SupportedReasoningLevels:      ccSwitchCodexSupportedReasoningLevels,
		ShellType:                     "shell_command",
		Visibility:                    "list",
		SupportedInAPI:                true,
		Priority:                      1000 + priority,
		AdditionalSpeedTiers:          []string{},
		ServiceTiers:                  []CcSwitchCodexServiceTier{},
		AvailabilityNUX:               nil,
		Upgrade:                       nil,
		BaseInstructions:              ccSwitchCodexBaseInstructions,
		ModelMessages:                 messages,
		SupportsReasoningSummaries:    true,
		DefaultReasoningSummary:       "none",
		SupportVerbosity:              true,
		DefaultVerbosity:              "low",
		ApplyPatchToolType:            "freeform",
		WebSearchToolType:             "text_and_image",
		TruncationPolicy:              CcSwitchCodexTruncationPolicy{Mode: "tokens", Limit: 10000},
		SupportsParallelToolCalls:     true,
		SupportsImageDetailOriginal:   true,
		ContextWindow:                 ccSwitchCodexDefaultContextWindow,
		MaxContextWindow:              ccSwitchCodexDefaultContextWindow,
		EffectiveContextWindowPercent: 95,
		ExperimentalSupportedTools:    []string{},
		InputModalities:               []string{"text", "image"},
		SupportsSearchTool:            true,
		UseResponsesLite:              false,
	}
}
