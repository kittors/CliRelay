package management

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

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

type modelConfigPayload struct {
	ID          string `json:"id"`
	OwnedBy     string `json:"owned_by"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Pricing     struct {
		Mode                  string  `json:"mode"`
		InputPricePerMillion  float64 `json:"input_price_per_million"`
		OutputPricePerMillion float64 `json:"output_price_per_million"`
		CachedPricePerMillion float64 `json:"cached_price_per_million"`
		PricePerCall          float64 `json:"price_per_call"`
	} `json:"pricing"`
}

func modelConfigResponse(row usage.ModelConfigRow) map[string]any {
	return map[string]any{
		"id":          row.ModelID,
		"owned_by":    row.OwnedBy,
		"description": row.Description,
		"enabled":     row.Enabled,
		"pricing": map[string]any{
			"mode":                     row.PricingMode,
			"input_price_per_million":  row.InputPricePerMillion,
			"output_price_per_million": row.OutputPricePerMillion,
			"cached_price_per_million": row.CachedPricePerMillion,
			"price_per_call":           row.PricePerCall,
		},
		"source":     row.Source,
		"updated_at": row.UpdatedAt,
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

func configuredPathRoutes(cfg *config.Config) []configuredModelPathRoute {
	seen := make(map[string]struct{})
	out := []configuredModelPathRoute{}
	appendRoute := func(label, path, group string) {
		path = internalrouting.NormalizeNamespacePath(path)
		group = internalrouting.NormalizeGroupName(group)
		if path == "" || group == "" {
			return
		}
		key := strings.ToLower(path)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(label) == "" {
			label = path
		}
		out = append(out, configuredModelPathRoute{
			Label: strings.TrimSpace(label),
			Path:  path,
			Group: group,
		})
	}

	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.SanitizeRouting()
	for _, route := range cfg.Routing.PathRoutes {
		appendRoute(route.Path, route.Path, route.Group)
	}
	for _, row := range usage.ListCcSwitchImportConfigs() {
		if row.RoutePath == "" || len(row.AllowedChannelGroups) == 0 {
			continue
		}
		appendRoute(row.ProviderName, row.RoutePath, row.AllowedChannelGroups[0])
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}

func (h *Handler) modelPathRouteScopedModels(models []map[string]any, routeGroup string) []map[string]any {
	routeGroup = internalrouting.NormalizeGroupName(routeGroup)
	if h == nil || h.authManager == nil || routeGroup == "" {
		return models
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(modelPathStringValue(model["id"]))
		if id == "" {
			continue
		}
		if h.authManager.CanServeModelWithScopes(id, nil, nil, routeGroup) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func (h *Handler) modelRootRouteScopedModels(models []map[string]any) []map[string]any {
	if h == nil || h.authManager == nil || h.cfg == nil || !h.cfg.Routing.IncludeDefaultGroup {
		return models
	}
	restricted := false
	for _, group := range h.cfg.Routing.ChannelGroups {
		if internalrouting.NormalizeGroupName(group.Name) == "default" && len(group.AllowedModels) > 0 {
			restricted = true
			break
		}
	}
	if !restricted {
		return models
	}
	allowedGroups := map[string]struct{}{"default": {}}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(modelPathStringValue(model["id"]))
		if id == "" {
			continue
		}
		if h.authManager.CanServeModelWithScopes(id, nil, allowedGroups, "") {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func modelConfigScope(c *gin.Context) string {
	scope := strings.ToLower(strings.TrimSpace(c.Query("scope")))
	switch scope {
	case "all", "library":
		return scope
	default:
		return "active"
	}
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

func filterModelConfigRowsByScope(rows []usage.ModelConfigRow, scope string) []usage.ModelConfigRow {
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

func modelConfigPayloadToRow(payload modelConfigPayload, scope string) usage.ModelConfigRow {
	source := "user"
	if scope == "library" {
		source = "seed"
	}
	return usage.ModelConfigRow{
		ModelID:               strings.TrimSpace(payload.ID),
		OwnedBy:               strings.TrimSpace(payload.OwnedBy),
		Description:           strings.TrimSpace(payload.Description),
		Enabled:               payload.Enabled,
		PricingMode:           strings.TrimSpace(payload.Pricing.Mode),
		InputPricePerMillion:  payload.Pricing.InputPricePerMillion,
		OutputPricePerMillion: payload.Pricing.OutputPricePerMillion,
		CachedPricePerMillion: payload.Pricing.CachedPricePerMillion,
		PricePerCall:          payload.Pricing.PricePerCall,
		Source:                source,
	}
}

func modelConfigParamID(c *gin.Context) string {
	return strings.TrimPrefix(strings.TrimSpace(c.Param("id")), "/")
}

// GetModels returns the list of all available models from the global registry
// along with their pricing information.
//
// Endpoint:
//
//	GET /v0/management/models
func (h *Handler) GetModels(c *gin.Context) {
	modelRegistry := registry.GetGlobalRegistry()
	allModels := modelRegistry.GetAvailableModels("openai")

	// Optional: filter to models that can be served by the selected channel set.
	// Used by the management UI when editing API keys.
	allowedRaw := strings.TrimSpace(c.Query("allowed_channels"))
	if allowedRaw == "" {
		allowedRaw = strings.TrimSpace(c.Query("allowed-channels"))
	}
	allowedGroupsRaw := strings.TrimSpace(c.Query("allowed_channel_groups"))
	if allowedGroupsRaw == "" {
		allowedGroupsRaw = strings.TrimSpace(c.Query("allowed-channel-groups"))
	}
	allowedGroups := internalrouting.ParseNormalizedSet(allowedGroupsRaw, internalrouting.NormalizeGroupName)
	if allowedRaw != "" && allowedRaw != "*" && !strings.EqualFold(allowedRaw, "all") {
		allowed := make(map[string]struct{})
		for _, part := range strings.Split(allowedRaw, ",") {
			key := strings.ToLower(strings.TrimSpace(part))
			if key == "" {
				continue
			}
			allowed[key] = struct{}{}
		}
		if len(allowed) > 0 && h != nil && h.authManager != nil {
			filtered := make([]map[string]any, 0, len(allModels))
			for _, model := range allModels {
				id, _ := model["id"].(string)
				if id == "" {
					continue
				}
				if h.authManager.CanServeModelWithScopes(id, allowed, allowedGroups, "") {
					filtered = append(filtered, model)
				}
			}
			allModels = filtered
		}
	} else if len(allowedGroups) > 0 && h != nil && h.authManager != nil {
		filtered := make([]map[string]any, 0, len(allModels))
		for _, model := range allModels {
			id, _ := model["id"].(string)
			if id == "" {
				continue
			}
			if h.authManager.CanServeModelWithScopes(id, nil, allowedGroups, "") {
				filtered = append(filtered, model)
			}
		}
		allModels = filtered
	}

	// Get all pricing data
	pricingMap := usage.GetAllModelPricing()

	filteredModels := make([]map[string]any, len(allModels))
	for i, model := range allModels {
		filteredModel := map[string]any{
			"id":     model["id"],
			"object": model["object"],
		}
		if created, exists := model["created"]; exists {
			filteredModel["created"] = created
		}
		if ownedBy, exists := model["owned_by"]; exists {
			filteredModel["owned_by"] = ownedBy
		}

		// Attach pricing if available
		if modelID, ok := model["id"].(string); ok {
			if pricing, exists := pricingMap[modelID]; exists {
				filteredModel["pricing"] = map[string]any{
					"input_price_per_million":  pricing.InputPricePerMillion,
					"output_price_per_million": pricing.OutputPricePerMillion,
					"cached_price_per_million": pricing.CachedPricePerMillion,
				}
			}
		}

		filteredModels[i] = filteredModel
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   filteredModels,
	})
}

// GetModelPathAvailability returns client-visible model IDs with the request paths
// where those IDs can be discovered or called from the management UI.
//
// Endpoint:
//
//	GET /v0/management/model-path-availability
func (h *Handler) GetModelPathAvailability(c *gin.Context) {
	modelRegistry := registry.GetGlobalRegistry()
	items := make(map[string]*modelPathAvailabilityResponse)

	rootOpenAICapabilities := openAIV1Capabilities("/")
	rootGeminiCapabilities := geminiV1BetaCapabilities("/")
	appendModelPaths(items, h.modelRootRouteScopedModels(modelRegistry.GetAvailableModels("openai")), "/", rootOpenAICapabilities)
	appendModelPaths(items, h.modelRootRouteScopedModels(modelRegistry.GetAvailableModels("gemini")), "/", rootGeminiCapabilities)

	routes := []modelPathRouteResponse{
		{
			Label:        "系统默认",
			Path:         "/",
			System:       true,
			ReadOnly:     true,
			Capabilities: append(append([]modelPathCapabilityResponse{}, rootOpenAICapabilities...), rootGeminiCapabilities...),
		},
	}

	for _, route := range configuredPathRoutes(h.cfg) {
		capabilities := append(append([]modelPathCapabilityResponse{}, openAIV1Capabilities(route.Path)...), geminiV1BetaCapabilities(route.Path)...)
		routes = append(routes, modelPathRouteResponse{
			Label:        route.Label,
			Path:         route.Path,
			Group:        route.Group,
			System:       false,
			ReadOnly:     false,
			Capabilities: capabilities,
		})
		appendModelPaths(items, h.modelPathRouteScopedModels(modelRegistry.GetAvailableModels("openai"), route.Group), route.Path, openAIV1Capabilities(route.Path))
		appendModelPaths(items, h.modelPathRouteScopedModels(modelRegistry.GetAvailableModels("gemini"), route.Group), route.Path, geminiV1BetaCapabilities(route.Path))
	}

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

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   data,
		"routes": routes,
	})
}

// GetModelConfigs returns database-backed model configuration rows.
//
// Endpoint:
//
//	GET /v0/management/model-configs
func (h *Handler) GetModelConfigs(c *gin.Context) {
	rows := filterModelConfigRowsByScope(usage.ListModelConfigs(), modelConfigScope(c))
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, modelConfigResponse(row))
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": items})
}

// PostModelConfig creates or updates a database-backed model configuration row.
//
// Endpoint:
//
//	POST /v0/management/model-configs
func (h *Handler) PostModelConfig(c *gin.Context) {
	var payload modelConfigPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	row := modelConfigPayloadToRow(payload, modelConfigScope(c))
	if row.ModelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model id is required"})
		return
	}
	if err := usage.UpsertModelConfig(row); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	saved, _ := usage.GetModelConfig(row.ModelID)
	c.JSON(http.StatusOK, modelConfigResponse(saved))
}

// PutModelConfig updates a database-backed model configuration row.
//
// Endpoint:
//
//	PUT /v0/management/model-configs/:id
func (h *Handler) PutModelConfig(c *gin.Context) {
	var payload modelConfigPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	originalID := modelConfigParamID(c)
	row := modelConfigPayloadToRow(payload, modelConfigScope(c))
	if row.ModelID == "" {
		row.ModelID = originalID
	}
	if row.ModelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model id is required"})
		return
	}
	if originalID != "" && originalID != row.ModelID {
		if err := usage.DeleteModelConfig(originalID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	if err := usage.UpsertModelConfig(row); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	saved, _ := usage.GetModelConfig(row.ModelID)
	c.JSON(http.StatusOK, modelConfigResponse(saved))
}

// DeleteModelConfig deletes a database-backed model configuration row.
//
// Endpoint:
//
//	DELETE /v0/management/model-configs/:id
func (h *Handler) DeleteModelConfig(c *gin.Context) {
	modelID := modelConfigParamID(c)
	if modelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model id is required"})
		return
	}
	if err := usage.DeleteModelConfig(modelID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// GetModelOwnerPresets returns editable model owner presets.
//
// Endpoint:
//
//	GET /v0/management/model-owner-presets
func (h *Handler) GetModelOwnerPresets(c *gin.Context) {
	modelCounts := make(map[string]int)
	for _, model := range usage.ListModelConfigs() {
		if model.OwnedBy != "" {
			modelCounts[model.OwnedBy]++
		}
	}
	rows := usage.ListModelOwnerPresets()
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"value":       row.Value,
			"label":       row.Label,
			"description": row.Description,
			"enabled":     row.Enabled,
			"updated_at":  row.UpdatedAt,
			"model_count": modelCounts[row.Value],
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutModelOwnerPresets replaces editable model owner presets.
//
// Endpoint:
//
//	PUT /v0/management/model-owner-presets
func (h *Handler) PutModelOwnerPresets(c *gin.Context) {
	var body struct {
		Items []usage.ModelOwnerPresetRow `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := usage.ReplaceModelOwnerPresets(body.Items); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "updated": len(body.Items)})
}

// GetModelPricing returns all model pricing entries.
//
// Endpoint:
//
//	GET /v0/management/model-pricing
func (h *Handler) GetModelPricing(c *gin.Context) {
	pricingMap := usage.GetAllModelPricing()

	// Convert to array for easier frontend consumption
	items := make([]map[string]any, 0, len(pricingMap))
	for _, row := range pricingMap {
		items = append(items, map[string]any{
			"model_id":                 row.ModelID,
			"input_price_per_million":  row.InputPricePerMillion,
			"output_price_per_million": row.OutputPricePerMillion,
			"cached_price_per_million": row.CachedPricePerMillion,
			"updated_at":               row.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutModelPricing updates or creates model pricing entries in bulk.
//
// Endpoint:
//
//	PUT /v0/management/model-pricing
//
// Body: { "items": [{ "model_id": "...", "input_price_per_million": 3.0, ... }] }
func (h *Handler) PutModelPricing(c *gin.Context) {
	var body struct {
		Items []struct {
			ModelID               string  `json:"model_id"`
			InputPricePerMillion  float64 `json:"input_price_per_million"`
			OutputPricePerMillion float64 `json:"output_price_per_million"`
			CachedPricePerMillion float64 `json:"cached_price_per_million"`
		} `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	for _, item := range body.Items {
		if item.ModelID == "" {
			continue
		}
		if err := usage.UpsertModelPricing(
			item.ModelID,
			item.InputPricePerMillion,
			item.OutputPricePerMillion,
			item.CachedPricePerMillion,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "updated": len(body.Items)})
}

// GetOpenRouterModelSync returns OpenRouter model sync settings and last run status.
//
// Endpoint:
//
//	GET /v0/management/model-openrouter-sync
func (h *Handler) GetOpenRouterModelSync(c *gin.Context) {
	c.JSON(http.StatusOK, usage.GetOpenRouterModelSyncState())
}

// PutOpenRouterModelSync updates OpenRouter model sync settings.
//
// Endpoint:
//
//	PUT /v0/management/model-openrouter-sync
func (h *Handler) PutOpenRouterModelSync(c *gin.Context) {
	var body struct {
		Enabled         bool `json:"enabled"`
		IntervalMinutes int  `json:"interval_minutes"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	state, err := usage.UpdateOpenRouterModelSyncSettings(body.Enabled, body.IntervalMinutes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

// PostOpenRouterModelSyncRun manually runs OpenRouter model sync now.
//
// Endpoint:
//
//	POST /v0/management/model-openrouter-sync/run
func (h *Handler) PostOpenRouterModelSyncRun(c *gin.Context) {
	ctx := c.Request.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	result, state, err := usage.RunOpenRouterModelSync(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "state": state})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "result": result, "state": state})
}
