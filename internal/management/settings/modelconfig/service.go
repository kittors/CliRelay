package modelconfig

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

var ErrModelIDRequired = errors.New("model id is required")
var ErrAuthGroupRequired = errors.New("auth group is required")

type UpsertConfigInput struct {
	OriginalID                string
	Scope                     string
	ModelID                   string
	OwnedBy                   string
	DisplayName               string
	Description               string
	Enabled                   bool
	InputModalities           *[]string
	OutputModalities          *[]string
	ContextLength             *int
	MaxCompletionTokens       *int
	SupportedParameters       *[]string
	Reasoning                 *any
	KnowledgeCutoff           string
	PricingMode               string
	InputPricePerMillion      float64
	OutputPricePerMillion     float64
	CachedPricePerMillion     float64
	CacheReadPricePerMillion  float64
	CacheWritePricePerMillion float64
	PricePerCall              float64
}

type OwnerPresetWithCount struct {
	usage.ModelOwnerPresetRow
	ModelCount int `json:"model_count"`
}

type PricingUpsertItem struct {
	ModelID                   string
	InputPricePerMillion      float64
	OutputPricePerMillion     float64
	CachedPricePerMillion     float64
	CacheReadPricePerMillion  float64
	CacheWritePricePerMillion float64
}

func NormalizeScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "all", "library":
		return strings.ToLower(strings.TrimSpace(scope))
	default:
		return "active"
	}
}

func ListConfigs(scope string) []usage.ModelConfigRow { return ListConfigsForTenant("", scope) }
func ListConfigsForTenant(tenantID, scope string) []usage.ModelConfigRow {
	return filterRowsByScope(usage.ListModelConfigsForTenant(tenantID), NormalizeScope(scope))
}

func ListAllConfigs() []usage.ModelConfigRow { return ListAllConfigsForTenant("") }
func ListAllConfigsForTenant(tenantID string) []usage.ModelConfigRow {
	return usage.ListModelConfigsForTenant(tenantID)
}

func GetConfig(modelID string) (usage.ModelConfigRow, bool) { return GetConfigForTenant("", modelID) }
func GetConfigForTenant(tenantID, modelID string) (usage.ModelConfigRow, bool) {
	return usage.GetModelConfigForTenant(tenantID, strings.TrimSpace(modelID))
}

func UpsertConfig(input UpsertConfigInput) (usage.ModelConfigRow, error) {
	return UpsertConfigForTenant("", input)
}
func UpsertConfigForTenant(tenantID string, input UpsertConfigInput) (usage.ModelConfigRow, error) {
	scope := NormalizeScope(input.Scope)
	originalID := strings.TrimSpace(input.OriginalID)
	row := usage.ModelConfigRow{
		ModelID:                   strings.TrimSpace(input.ModelID),
		OwnedBy:                   strings.TrimSpace(input.OwnedBy),
		DisplayName:               strings.TrimSpace(input.DisplayName),
		Description:               strings.TrimSpace(input.Description),
		Enabled:                   input.Enabled,
		KnowledgeCutoff:           strings.TrimSpace(input.KnowledgeCutoff),
		PricingMode:               strings.TrimSpace(input.PricingMode),
		InputPricePerMillion:      input.InputPricePerMillion,
		OutputPricePerMillion:     input.OutputPricePerMillion,
		CachedPricePerMillion:     input.CachedPricePerMillion,
		CacheReadPricePerMillion:  input.CacheReadPricePerMillion,
		CacheWritePricePerMillion: input.CacheWritePricePerMillion,
		PricePerCall:              input.PricePerCall,
		Source:                    sourceForScope(scope),
	}
	if input.InputModalities != nil {
		row.InputModalities = *input.InputModalities
	}
	if input.OutputModalities != nil {
		row.OutputModalities = *input.OutputModalities
	}
	if input.ContextLength != nil {
		row.ContextLength = *input.ContextLength
	}
	if input.MaxCompletionTokens != nil {
		row.MaxCompletionTokens = *input.MaxCompletionTokens
	}
	if input.SupportedParameters != nil {
		row.SupportedParameters = *input.SupportedParameters
	}
	if input.Reasoning != nil {
		row.Reasoning = encodeReasoningPayload(*input.Reasoning)
	}
	if row.ModelID == "" {
		row.ModelID = originalID
	}
	if row.ModelID == "" {
		return usage.ModelConfigRow{}, ErrModelIDRequired
	}

	lookupID := row.ModelID
	if originalID != "" {
		lookupID = originalID
	}
	if existing, ok := usage.GetModelConfigForTenant(tenantID, lookupID); ok {
		if input.InputModalities == nil {
			row.InputModalities = existing.InputModalities
		}
		if input.OutputModalities == nil {
			row.OutputModalities = existing.OutputModalities
		}
		if strings.TrimSpace(input.DisplayName) == "" {
			row.DisplayName = existing.DisplayName
		}
		if input.ContextLength == nil {
			row.ContextLength = existing.ContextLength
		}
		if input.MaxCompletionTokens == nil {
			row.MaxCompletionTokens = existing.MaxCompletionTokens
		}
		if input.SupportedParameters == nil {
			row.SupportedParameters = existing.SupportedParameters
		}
		if input.Reasoning == nil {
			row.Reasoning = existing.Reasoning
		}
		if strings.TrimSpace(input.KnowledgeCutoff) == "" {
			row.KnowledgeCutoff = existing.KnowledgeCutoff
		}
	}

	if originalID != "" && originalID != row.ModelID {
		if err := usage.DeleteModelConfigForTenant(tenantID, originalID); err != nil {
			return usage.ModelConfigRow{}, err
		}
	}
	if err := usage.UpsertModelConfigForTenant(tenantID, row); err != nil {
		return usage.ModelConfigRow{}, err
	}
	InvalidateDisabledModelCache()

	saved, ok := usage.GetModelConfigForTenant(tenantID, row.ModelID)
	if !ok {
		return row, nil
	}
	return saved, nil
}

func DeleteConfig(modelID string) error { return DeleteConfigForTenant("", modelID) }
func DeleteConfigForTenant(tenantID, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return ErrModelIDRequired
	}
	if err := usage.DeleteModelConfigForTenant(tenantID, modelID); err != nil {
		return err
	}
	InvalidateDisabledModelCache()
	return nil
}

func ListOwnerPresetsWithCounts() []OwnerPresetWithCount {
	return ListOwnerPresetsWithCountsForTenant("")
}
func ListOwnerPresetsWithCountsForTenant(tenantID string) []OwnerPresetWithCount {
	modelCounts := make(map[string]int)
	for _, model := range usage.ListModelConfigsForTenant(tenantID) {
		if model.OwnedBy != "" {
			modelCounts[model.OwnedBy]++
		}
	}

	rows := usage.ListModelOwnerPresetsForTenant(tenantID)
	items := make([]OwnerPresetWithCount, 0, len(rows))
	for _, row := range rows {
		items = append(items, OwnerPresetWithCount{
			ModelOwnerPresetRow: row,
			ModelCount:          modelCounts[row.Value],
		})
	}
	return items
}

func ReplaceOwnerPresets(rows []usage.ModelOwnerPresetRow) error {
	return ReplaceOwnerPresetsForTenant("", rows)
}
func ReplaceOwnerPresetsForTenant(tenantID string, rows []usage.ModelOwnerPresetRow) error {
	return usage.ReplaceModelOwnerPresetsForTenant(tenantID, rows)
}

func ListAuthGroupOwnerMappings() []usage.AuthGroupOwnerMappingRow {
	return ListAuthGroupOwnerMappingsForTenant("")
}
func ListAuthGroupOwnerMappingsForTenant(tenantID string) []usage.AuthGroupOwnerMappingRow {
	return usage.ListAuthGroupOwnerMappingsForTenant(tenantID)
}

func UpsertAuthGroupOwnerMapping(authGroup, owner string) (usage.AuthGroupOwnerMappingRow, error) {
	return UpsertAuthGroupOwnerMappingForTenant("", authGroup, owner)
}
func UpsertAuthGroupOwnerMappingForTenant(tenantID, authGroup, owner string) (usage.AuthGroupOwnerMappingRow, error) {
	row := usage.AuthGroupOwnerMappingRow{
		AuthGroup: strings.TrimSpace(authGroup),
		Owner:     strings.TrimSpace(owner),
	}
	if row.AuthGroup == "" {
		return usage.AuthGroupOwnerMappingRow{}, ErrAuthGroupRequired
	}
	if err := usage.UpsertAuthGroupOwnerMappingForTenant(tenantID, row); err != nil {
		return usage.AuthGroupOwnerMappingRow{}, err
	}
	saved, ok := usage.GetAuthGroupOwnerMappingForTenant(tenantID, row.AuthGroup)
	if !ok {
		return row, nil
	}
	return saved, nil
}

func DeleteAuthGroupOwnerMapping(authGroup string) error {
	return DeleteAuthGroupOwnerMappingForTenant("", authGroup)
}
func DeleteAuthGroupOwnerMappingForTenant(tenantID, authGroup string) error {
	authGroup = strings.TrimSpace(authGroup)
	if authGroup == "" {
		return ErrAuthGroupRequired
	}
	return usage.DeleteAuthGroupOwnerMappingForTenant(tenantID, authGroup)
}

func ListPricing() []usage.ModelPricingRow { return ListPricingForTenant("") }
func ListPricingForTenant(tenantID string) []usage.ModelPricingRow {
	pricingMap := usage.GetAllModelPricingForTenant(tenantID)
	items := make([]usage.ModelPricingRow, 0, len(pricingMap))
	for _, row := range pricingMap {
		items = append(items, row)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(strings.TrimSpace(items[i].ModelID)) < strings.ToLower(strings.TrimSpace(items[j].ModelID))
	})
	return items
}

func UpsertPricing(items []PricingUpsertItem) (int, error) { return UpsertPricingForTenant("", items) }
func UpsertPricingForTenant(tenantID string, items []PricingUpsertItem) (int, error) {
	updated := 0
	for _, item := range items {
		modelID := strings.TrimSpace(item.ModelID)
		if modelID == "" {
			continue
		}
		if err := usage.UpsertModelPricingV2ForTenant(tenantID,
			modelID,
			item.InputPricePerMillion,
			item.OutputPricePerMillion,
			item.CachedPricePerMillion,
			item.CacheReadPricePerMillion,
			item.CacheWritePricePerMillion,
		); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

func GetOpenRouterSyncState() usage.OpenRouterModelSyncState {
	return GetOpenRouterSyncStateForTenant("")
}

func GetOpenRouterSyncStateForTenant(tenantID string) usage.OpenRouterModelSyncState {
	return usage.GetOpenRouterModelSyncStateForTenant(tenantID)
}

func UpdateOpenRouterSyncSettings(enabled bool, intervalMinutes int) (usage.OpenRouterModelSyncState, error) {
	return UpdateOpenRouterSyncSettingsForTenant("", enabled, intervalMinutes)
}

func UpdateOpenRouterSyncSettingsForTenant(tenantID string, enabled bool, intervalMinutes int) (usage.OpenRouterModelSyncState, error) {
	return usage.UpdateOpenRouterModelSyncSettingsForTenant(tenantID, enabled, intervalMinutes)
}

func RunOpenRouterSync(ctx context.Context) (usage.OpenRouterModelSyncResult, usage.OpenRouterModelSyncState, error) {
	return RunOpenRouterSyncForTenant(ctx, "")
}

func RunOpenRouterSyncForTenant(ctx context.Context, tenantID string) (usage.OpenRouterModelSyncResult, usage.OpenRouterModelSyncState, error) {
	return usage.RunOpenRouterModelSyncForTenant(ctx, tenantID)
}

func sourceForScope(scope string) string {
	if scope == "library" {
		return "seed"
	}
	return "user"
}

func availableModelIDSet() map[string]bool {
	modelRegistry := registry.GetGlobalRegistry()
	availableModels := modelRegistry.GetAvailableModels("openai")
	result := make(map[string]bool, len(availableModels))
	for _, model := range availableModels {
		id, _ := model["id"].(string)
		id = strings.TrimSpace(id)
		if id != "" {
			result[id] = true
		}
	}
	return result
}

func filterRowsByScope(rows []usage.ModelConfigRow, scope string) []usage.ModelConfigRow {
	availableIDs := map[string]bool(nil)
	if scope == "active" {
		availableIDs = availableModelIDSet()
	}

	filtered := make([]usage.ModelConfigRow, 0, len(rows))
	for _, row := range rows {
		source := strings.ToLower(strings.TrimSpace(row.Source))
		switch scope {
		case "all":
			filtered = append(filtered, row)
		case "library":
			if source == "seed" || source == "openrouter" {
				filtered = append(filtered, row)
			}
		default:
			if source == "user" || (source == "seed" && availableIDs[row.ModelID]) {
				filtered = append(filtered, row)
			}
		}
	}
	return filtered
}

func encodeReasoningPayload(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}
