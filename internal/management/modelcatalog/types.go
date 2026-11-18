package modelcatalog

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

type ModelConfigPricingPayload struct {
	Mode                      string  `json:"mode"`
	InputPricePerMillion      float64 `json:"input_price_per_million"`
	OutputPricePerMillion     float64 `json:"output_price_per_million"`
	CachedPricePerMillion     float64 `json:"cached_price_per_million"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million"`
	PricePerCall              float64 `json:"price_per_call"`
}

type ModelConfigPayload struct {
	ID                  string                    `json:"id"`
	OwnedBy             string                    `json:"owned_by"`
	DisplayName         string                    `json:"display_name"`
	Description         string                    `json:"description"`
	Enabled             bool                      `json:"enabled"`
	InputModalities     *[]string                 `json:"input_modalities"`
	OutputModalities    *[]string                 `json:"output_modalities"`
	ContextLength       *int                      `json:"context_length"`
	MaxCompletionTokens *int                      `json:"max_completion_tokens"`
	SupportedParameters *[]string                 `json:"supported_parameters"`
	Reasoning           *any                      `json:"reasoning"`
	KnowledgeCutoff     string                    `json:"knowledge_cutoff"`
	Pricing             ModelConfigPricingPayload `json:"pricing"`
}

type ModelPricingUpdateItem struct {
	ModelID                   string  `json:"model_id"`
	InputPricePerMillion      float64 `json:"input_price_per_million"`
	OutputPricePerMillion     float64 `json:"output_price_per_million"`
	CachedPricePerMillion     float64 `json:"cached_price_per_million"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million"`
}

type modelPathCapabilityResponse struct {
	Label  string `json:"label"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Family string `json:"family"`
}

type modelPathResponse struct {
	Scope  string `json:"scope"`
	Label  string `json:"label"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Family string `json:"family"`
}

type modelPathAvailabilityResponse struct {
	ID      string              `json:"id"`
	OwnedBy string              `json:"owned_by,omitempty"`
	Kind    string              `json:"kind"`
	Alias   bool                `json:"alias"`
	Paths   []modelPathResponse `json:"paths"`
}

type modelPathRouteResponse struct {
	Label        string                        `json:"label"`
	Path         string                        `json:"path"`
	Group        string                        `json:"group,omitempty"`
	System       bool                          `json:"system"`
	ReadOnly     bool                          `json:"read_only"`
	Capabilities []modelPathCapabilityResponse `json:"capabilities"`
}

type configuredModelPathRoute struct {
	Label string
	Path  string
	Group string
}

func modelConfigResponse(row, pricing usage.ModelConfigRow) map[string]any {
	response := map[string]any{
		"id":          row.ModelID,
		"owned_by":    row.OwnedBy,
		"description": row.Description,
		"enabled":     row.Enabled,
		"pricing":     modelPricingPayload(pricing),
		"source":      row.Source,
		"updated_at":  row.UpdatedAt,
	}
	if row.DisplayName != "" {
		response["display_name"] = row.DisplayName
	}
	attachModelConfigCapabilities(response, row)
	return response
}

func modelConfigModalitiesJSON(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return values
}

func modelConfigSupportsVision(row usage.ModelConfigRow) bool {
	for _, modality := range row.InputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

func attachModelConfigCapabilities(target map[string]any, row usage.ModelConfigRow) {
	target["input_modalities"] = modelConfigModalitiesJSON(row.InputModalities)
	target["output_modalities"] = modelConfigModalitiesJSON(row.OutputModalities)
	target["supports_vision"] = modelConfigSupportsVision(row)
	if row.DisplayName != "" {
		target["display_name"] = row.DisplayName
	}
	if row.ContextLength > 0 {
		target["context_length"] = row.ContextLength
	}
	if row.MaxCompletionTokens > 0 {
		target["max_completion_tokens"] = row.MaxCompletionTokens
	}
	if len(row.SupportedParameters) > 0 {
		target["supported_parameters"] = modelConfigModalitiesJSON(row.SupportedParameters)
	}
	if reasoning := strings.TrimSpace(row.Reasoning); reasoning != "" {
		var payload any
		if err := json.Unmarshal([]byte(reasoning), &payload); err == nil {
			target["reasoning"] = payload
		} else {
			target["reasoning"] = reasoning
		}
	}
	if row.KnowledgeCutoff != "" {
		target["knowledge_cutoff"] = row.KnowledgeCutoff
	}
}

func capabilityPath(prefix, suffix string) string {
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if prefix == "" || prefix == "/" {
		return suffix
	}
	return prefix + suffix
}

func openAIV1Capabilities(prefix string) []modelPathCapabilityResponse {
	return []modelPathCapabilityResponse{
		{Label: "models", Method: http.MethodGet, Path: capabilityPath(prefix, "/v1/models"), Family: "openai-v1-models"},
		{Label: "chat", Method: http.MethodPost, Path: capabilityPath(prefix, "/v1/chat/completions"), Family: "openai-v1-chat"},
		{Label: "completions", Method: http.MethodPost, Path: capabilityPath(prefix, "/v1/completions"), Family: "openai-v1-completions"},
		{Label: "responses", Method: http.MethodPost, Path: capabilityPath(prefix, "/v1/responses"), Family: "openai-v1-responses"},
		{Label: "messages", Method: http.MethodPost, Path: capabilityPath(prefix, "/v1/messages"), Family: "claude-v1-messages"},
		{Label: "images", Method: http.MethodPost, Path: capabilityPath(prefix, "/v1/images/generations"), Family: "openai-v1-images"},
	}
}

func geminiV1BetaCapabilities(prefix string) []modelPathCapabilityResponse {
	return []modelPathCapabilityResponse{
		{Label: "v1beta", Method: http.MethodGet, Path: capabilityPath(prefix, "/v1beta/models"), Family: "gemini-v1beta-models"},
		{Label: "v1beta", Method: http.MethodPost, Path: capabilityPath(prefix, "/v1beta/models/*action"), Family: "gemini-v1beta-action"},
	}
}

func modelPathScope(prefix string) string {
	if strings.TrimSpace(prefix) == "" || strings.TrimSpace(prefix) == "/" {
		return "root"
	}
	return "group"
}

func appendModelPaths(
	items map[string]*modelPathAvailabilityResponse,
	models []map[string]any,
	scopePrefix string,
	capabilities []modelPathCapabilityResponse,
) {
	scope := modelPathScope(scopePrefix)
	for _, model := range models {
		id := strings.TrimSpace(modelPathStringValue(model["id"]))
		if id == "" {
			continue
		}
		item := items[id]
		if item == nil {
			item = &modelPathAvailabilityResponse{
				ID:      id,
				OwnedBy: strings.TrimSpace(modelPathStringValue(model["owned_by"])),
				Kind:    "canonical",
				Alias:   false,
				Paths:   []modelPathResponse{},
			}
			items[id] = item
		}
		seen := make(map[string]struct{}, len(item.Paths))
		for _, path := range item.Paths {
			seen[path.Method+" "+path.Path+" "+path.Family] = struct{}{}
		}
		for _, capability := range capabilities {
			key := capability.Method + " " + capability.Path + " " + capability.Family
			if _, ok := seen[key]; ok {
				continue
			}
			item.Paths = append(item.Paths, modelPathResponse{
				Scope:  scope,
				Label:  capability.Label,
				Method: capability.Method,
				Path:   capability.Path,
				Family: capability.Family,
			})
			seen[key] = struct{}{}
		}
	}
}

func modelPathStringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

func sortModelPathAvailabilityRows(items map[string]*modelPathAvailabilityResponse) []modelPathAvailabilityResponse {
	data := make([]modelPathAvailabilityResponse, 0, len(items))
	for _, item := range items {
		sort.Slice(item.Paths, func(i, j int) bool {
			if item.Paths[i].Scope != item.Paths[j].Scope {
				return item.Paths[i].Scope < item.Paths[j].Scope
			}
			if item.Paths[i].Path != item.Paths[j].Path {
				return item.Paths[i].Path < item.Paths[j].Path
			}
			return item.Paths[i].Method < item.Paths[j].Method
		})
		data = append(data, *item)
	}
	sort.Slice(data, func(i, j int) bool {
		return data[i].ID < data[j].ID
	})
	return data
}
