package api

import (
	"bytes"
	"strings"

	"github.com/gin-gonic/gin"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v8/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// piCatalogRequest reports whether the caller is the pi CLIProxyAPI provider, which
// builds its model picker from GET {root}/v1/models?client_version=pi.
func piCatalogRequest(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(c.Query("client_version")), "pi")
}

// enrichOpenAIModelsWithCatalog fills description/pricing/modalities from the
// tenant model catalog. Safe for missing rows — leaves the base model map alone.
func enrichOpenAIModelsWithCatalog(tenantID string, models []map[string]interface{}) {
	if len(models) == 0 {
		return
	}
	configRows := modelconfigsettings.ListAllConfigsForTenant(tenantID)
	configByID := make(map[string]usage.ModelConfigRow, len(configRows))
	for _, row := range configRows {
		configByID[strings.ToLower(strings.TrimSpace(row.ModelID))] = row
	}
	pricingRows := usage.GetAllModelPricingForTenant(tenantID)
	pricingByID := make(map[string]usage.ModelPricingRow, len(pricingRows))
	for modelID, row := range pricingRows {
		pricingByID[strings.ToLower(strings.TrimSpace(modelID))] = row
	}
	for _, model := range models {
		if model == nil {
			continue
		}
		id, _ := model["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if row, ok := configByID[strings.ToLower(id)]; ok {
			if ownedBy := strings.TrimSpace(row.OwnedBy); ownedBy != "" {
				if existing, _ := model["owned_by"].(string); strings.TrimSpace(existing) == "" {
					model["owned_by"] = ownedBy
				}
			}
			if desc := strings.TrimSpace(row.Description); desc != "" {
				model["description"] = desc
			}
			if displayName := strings.TrimSpace(row.DisplayName); displayName != "" {
				if existing, _ := model["display_name"].(string); strings.TrimSpace(existing) == "" {
					model["display_name"] = displayName
				}
			}
			if len(row.InputModalities) > 0 {
				model["input_modalities"] = append([]string(nil), row.InputModalities...)
			}
			if len(row.OutputModalities) > 0 {
				model["output_modalities"] = append([]string(nil), row.OutputModalities...)
			}
			supportsVision := false
			for _, modality := range row.InputModalities {
				if strings.EqualFold(strings.TrimSpace(modality), "image") {
					supportsVision = true
					break
				}
			}
			model["supports_vision"] = supportsVision
		}
		if pricing, ok := usage.ResolveModelPricingRow(id, configByID, pricingByID); ok {
			model["pricing"] = map[string]any{
				"mode":                          pricing.PricingMode,
				"input_price_per_million":       pricing.InputPricePerMillion,
				"output_price_per_million":      pricing.OutputPricePerMillion,
				"cached_price_per_million":      pricing.CachedPricePerMillion,
				"cache_read_price_per_million":  pricing.CacheReadPricePerMillion,
				"cache_write_price_per_million": pricing.CacheWritePricePerMillion,
				"price_per_call":                pricing.PricePerCall,
			}
		}
	}
}

// enrichOpenAIModelsWithStaticCapabilities publishes the capability metadata the
// static registry already knows about (context window, completion cap, reasoning
// efforts, modalities) on the OpenAI-compatible /v1/models payload.
//
// Clients that build their model catalog from this endpoint (Codex CLI, the pi
// CLIProxyAPI provider, ...) otherwise have to fall back to conservative
// defaults such as a 128k context window and no reasoning support.
// Existing keys are never overwritten.
func enrichOpenAIModelsWithStaticCapabilities(models []map[string]interface{}) {
	for _, model := range models {
		if model == nil {
			continue
		}
		id, _ := model["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		info := registry.LookupStaticModelInfo(id)
		if info == nil {
			if resolved := registry.LookupModelInfo(id); resolved != nil {
				info = resolved
			}
		}
		if info == nil {
			continue
		}

		contextWindow := info.ContextLength
		if contextWindow <= 0 {
			contextWindow = info.InputTokenLimit
		}
		if contextWindow > 0 {
			if _, exists := model["context_window"]; !exists {
				model["context_window"] = contextWindow
			}
			if _, exists := model["max_context_window"]; !exists {
				model["max_context_window"] = contextWindow
			}
		}

		maxCompletion := info.MaxCompletionTokens
		if maxCompletion <= 0 {
			maxCompletion = info.OutputTokenLimit
		}
		if maxCompletion > 0 {
			if _, exists := model["max_completion_tokens"]; !exists {
				model["max_completion_tokens"] = maxCompletion
			}
			if _, exists := model["max_output_tokens"]; !exists {
				model["max_output_tokens"] = maxCompletion
			}
		}

		if displayName := strings.TrimSpace(info.DisplayName); displayName != "" {
			if existing, _ := model["display_name"].(string); strings.TrimSpace(existing) == "" {
				model["display_name"] = displayName
			}
		}

		if levels := staticReasoningLevels(info); len(levels) > 0 {
			if _, exists := model["supported_reasoning_levels"]; !exists {
				model["supported_reasoning_levels"] = levels
			}
		}

		// Modalities are not synthesised here: the registry holds no modality data, so
		// this could only guess by provider family. enrichOpenAIModelsWithCatalog
		// publishes the real values from model_configs.
	}
}

// staticReasoningLevels maps the registry thinking capability onto the discrete
// effort vocabulary that OpenAI-responses style clients expect.
func staticReasoningLevels(info *registry.ModelInfo) []string {
	if info == nil || info.Thinking == nil {
		return nil
	}
	thinking := info.Thinking
	// Checked before building the list so a lone "none" cannot be assembled and
	// then discarded with it.
	if len(thinking.Levels) == 0 && thinking.Max <= 0 && !thinking.DynamicAllowed {
		return nil
	}
	levels := make([]string, 0, 6)
	if thinking.ZeroAllowed {
		levels = append(levels, "none")
	}
	if len(thinking.Levels) > 0 {
		for _, level := range thinking.Levels {
			level = strings.ToLower(strings.TrimSpace(level))
			if level == "" || level == "none" {
				continue
			}
			levels = append(levels, level)
		}
		return levels
	}
	return append(levels, "low", "medium", "high")
}

func ccSwitchRequestModelAllowedForTarget(id string, route *internalrouting.PathRouteContext, allowedModels map[string]struct{}) bool {
	id = strings.TrimSpace(id)
	if id == "" || route == nil || route.CcSwitch == nil || len(allowedModels) == 0 {
		return false
	}
	for _, mapping := range route.CcSwitch.ModelMappings {
		target := strings.TrimSpace(mapping.TargetModel)
		request := strings.TrimSpace(mapping.RequestModel)
		if target == "" || request == "" || !strings.EqualFold(target, id) {
			continue
		}
		if modelInSet(request, allowedModels) {
			return true
		}
	}
	return false
}

// filterModelsForCcSwitchRoute shapes the listing of a CC Switch preset route.
// A preset's mappings rename models; they do not narrow the list. The route
// serves what its channel group serves, and the scope filter before this one
// already applied the group, so a model the upstream adds later is listed here
// as soon as the group admits it, the same way the request gate treats it.
func filterModelsForCcSwitchRoute(models []map[string]interface{}, route *internalrouting.PathRouteContext) []map[string]interface{} {
	if route == nil || route.CcSwitch == nil {
		return models
	}
	// OpenAI-protocol apps send the request names and the route rewrites them
	// (ccSwitchOpenAIModelMapping), so a mapped model is offered under those.
	// Other apps send real ids, which nothing rewrites, so they get the list as is.
	if usage.CcSwitchClientUsesOpenAIProtocol(route.CcSwitch.ClientType) {
		return renameMappedModelsForCcSwitchRoute(models, route.CcSwitch)
	}
	return models
}

func renameMappedModelsForCcSwitchRoute(models []map[string]interface{}, ccSwitch *internalrouting.CcSwitchRouteContext) []map[string]interface{} {
	targetToRequests := make(map[string][]string)
	requestNames := make(map[string]struct{})
	for _, mapping := range ccSwitch.ModelMappings {
		request := strings.TrimSpace(mapping.RequestModel)
		target := strings.TrimSpace(mapping.TargetModel)
		if request == "" || target == "" {
			continue
		}
		key := strings.ToLower(target)
		targetToRequests[key] = appendUniqueModel(targetToRequests[key], request)
		requestNames[strings.ToLower(request)] = struct{}{}
	}
	if len(targetToRequests) == 0 {
		return models
	}

	listed := make([]map[string]interface{}, 0, len(models))
	seen := make(map[string]struct{})
	for _, model := range models {
		id, ok := model["id"].(string)
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(id))
		if requests := targetToRequests[key]; len(requests) > 0 {
			for _, request := range requests {
				appendModelClone(&listed, seen, model, request)
			}
			continue
		}
		// A mapping sends this name to another model, so on this route the name
		// reaches the mapping's target; listing this model under it would lie.
		if _, shadowed := requestNames[key]; shadowed {
			continue
		}
		appendModelClone(&listed, seen, model, id)
	}
	return listed
}

func appendUniqueModel(models []string, model string) []string {
	for _, existing := range models {
		if strings.EqualFold(existing, model) {
			return models
		}
	}
	return append(models, model)
}

func appendModelClone(models *[]map[string]interface{}, seen map[string]struct{}, model map[string]interface{}, id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	key := strings.ToLower(id)
	if _, exists := seen[key]; exists {
		return
	}
	seen[key] = struct{}{}
	clone := make(map[string]interface{}, len(model))
	for k, v := range model {
		clone[k] = v
	}
	clone["id"] = id
	*models = append(*models, clone)
}

// responseRecorder captures the response body for post-processing.
type responseRecorder struct {
	gin.ResponseWriter
	body       *bytes.Buffer
	statusCode int
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.body.Write(b)
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
}

// queryFlagEnabled reads a boolean query flag under either naming convention.
func queryFlagEnabled(c *gin.Context, primary, fallback string) bool {
	raw := strings.TrimSpace(c.Query(primary))
	if raw == "" && fallback != "" {
		raw = strings.TrimSpace(c.Query(fallback))
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
