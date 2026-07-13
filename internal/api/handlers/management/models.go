package management

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/modelcatalog"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type ModelsHandler struct {
	*Handler
}

func (h *Handler) Models() *ModelsHandler {
	if h == nil {
		return nil
	}
	return &ModelsHandler{Handler: h}
}

func (h *ModelsHandler) service(c *gin.Context) *modelcatalog.Service {
	tenantID := effectiveTenantID(c)
	if h == nil {
		return modelcatalog.NewForTenant(tenantID, nil, nil)
	}
	// Overlay the effective tenant's routing onto the process cfg so channel-group
	// allowed-models apply on plaza/catalog. Non-system tenants store routing in DB;
	// system keeps h.cfg.Routing when no DB row exists.
	cfgCopy := &config.Config{}
	if h.cfg != nil {
		copied := *h.cfg
		cfgCopy = &copied
	}
	if routing := usage.GetRoutingConfigForTenant(tenantID); routing != nil {
		cfgCopy.Routing = *routing
	} else if tenantID != "" && tenantID != identity.SystemTenantID {
		cfgCopy.Routing = config.RoutingConfig{IncludeDefaultGroup: true}
	}
	return modelcatalog.NewForTenant(tenantID, cfgCopy, h.authManager)
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

func modelConfigParamID(c *gin.Context) string {
	return strings.TrimPrefix(strings.TrimSpace(c.Param("id")), "/")
}

func queryAlias(c *gin.Context, primary, fallback string) string {
	value := strings.TrimSpace(c.Query(primary))
	if value == "" && fallback != "" {
		value = strings.TrimSpace(c.Query(fallback))
	}
	return value
}

// queryIgnoreGroupAllowedModels is set by the channel-group editor so the picker
// lists every model the group's channels can serve, not only the saved allow-list.
// Plaza/catalog omit this flag and keep AllowedModels filtering.
func queryIgnoreGroupAllowedModels(c *gin.Context) bool {
	raw := strings.TrimSpace(queryAlias(c, "ignore_group_allowed_models", "ignore-group-allowed-models"))
	if raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func availabilityFilterOptionsFromQuery(c *gin.Context) modelcatalog.AvailabilityFilterOptions {
	return modelcatalog.AvailabilityFilterOptions{
		IgnoreGroupAllowedModels: queryIgnoreGroupAllowedModels(c),
	}
}

// GetConfiguredModelAvailability returns the currently configured and serviceable
// model IDs with pricing/metadata and active_metadata for owner/source filtering.
func (h *ModelsHandler) GetConfiguredModelAvailability(c *gin.Context) {
	c.JSON(http.StatusOK, h.service(c).ConfiguredAvailability(
		queryAlias(c, "allowed_channels", "allowed-channels"),
		queryAlias(c, "allowed_channel_groups", "allowed-channel-groups"),
		availabilityFilterOptionsFromQuery(c),
	))
}

// GetModels returns the list of all available models from the global registry
// along with their pricing information.
func (h *ModelsHandler) GetModels(c *gin.Context) {
	c.JSON(http.StatusOK, h.service(c).Models(
		queryAlias(c, "allowed_channels", "allowed-channels"),
		queryAlias(c, "allowed_channel_groups", "allowed-channel-groups"),
		availabilityFilterOptionsFromQuery(c),
	))
}

// GetModelPathAvailability returns client-visible model IDs with the request paths
// where those IDs can be discovered or called from the management UI.
func (h *ModelsHandler) GetModelPathAvailability(c *gin.Context) {
	c.JSON(http.StatusOK, h.service(c).PathAvailability())
}

// GetModelConfigs returns database-backed model configuration rows.
func (h *ModelsHandler) GetModelConfigs(c *gin.Context) {
	c.JSON(http.StatusOK, h.service(c).ListModelConfigs(modelConfigScope(c)))
}

// PostModelConfig creates or updates a database-backed model configuration row.
func (h *ModelsHandler) PostModelConfig(c *gin.Context) {
	var payload modelcatalog.ModelConfigPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	saved, err := h.service(c).UpsertModelConfig(payload, "", modelConfigScope(c))
	if err != nil {
		if errors.Is(err, modelcatalog.ErrModelIDRequired) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.notifyModelConfigMutated(c)
	c.JSON(http.StatusOK, saved)
}

// PutModelConfig updates a database-backed model configuration row.
func (h *ModelsHandler) PutModelConfig(c *gin.Context) {
	var payload modelcatalog.ModelConfigPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	saved, err := h.service(c).UpsertModelConfig(payload, modelConfigParamID(c), modelConfigScope(c))
	if err != nil {
		if errors.Is(err, modelcatalog.ErrModelIDRequired) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.notifyModelConfigMutated(c)
	c.JSON(http.StatusOK, saved)
}

// DeleteModelConfig deletes a database-backed model configuration row.
func (h *ModelsHandler) DeleteModelConfig(c *gin.Context) {
	if err := h.service(c).DeleteModelConfig(modelConfigParamID(c)); err != nil {
		if errors.Is(err, modelcatalog.ErrModelIDRequired) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.notifyModelConfigMutated(c)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// GetModelOwnerPresets returns editable model owner presets.
func (h *ModelsHandler) GetModelOwnerPresets(c *gin.Context) {
	c.JSON(http.StatusOK, h.service(c).OwnerPresets())
}

// PutModelOwnerPresets replaces editable model owner presets.
func (h *ModelsHandler) PutModelOwnerPresets(c *gin.Context) {
	var body struct {
		Items []usage.ModelOwnerPresetRow `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	payload, err := h.service(c).ReplaceOwnerPresets(body.Items)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

// GetAuthGroupModelOwnerMappings returns persisted auth-group to model-owner mappings.
func (h *ModelsHandler) GetAuthGroupModelOwnerMappings(c *gin.Context) {
	c.JSON(http.StatusOK, h.service(c).AuthGroupOwnerMappings())
}

// PatchAuthGroupModelOwnerMapping upserts or deletes a persisted auth-group to model-owner mapping.
func (h *ModelsHandler) PatchAuthGroupModelOwnerMapping(c *gin.Context) {
	var body struct {
		AuthGroup string `json:"auth_group"`
		Owner     string `json:"owner"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	payload, err := h.service(c).PatchAuthGroupOwnerMapping(body.AuthGroup, body.Owner)
	if err != nil {
		if errors.Is(err, modelcatalog.ErrAuthGroupRequired) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

// GetModelPricing returns all model pricing entries.
func (h *ModelsHandler) GetModelPricing(c *gin.Context) {
	c.JSON(http.StatusOK, h.service(c).Pricing())
}

// PutModelPricing updates or creates model pricing entries in bulk.
func (h *ModelsHandler) PutModelPricing(c *gin.Context) {
	var body struct {
		Items []modelcatalog.ModelPricingUpdateItem `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	payload, err := h.service(c).UpdatePricing(body.Items)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

// GetOpenRouterModelSync returns OpenRouter model sync settings and last run status.
func (h *ModelsHandler) GetOpenRouterModelSync(c *gin.Context) {
	c.JSON(http.StatusOK, h.service(c).OpenRouterModelSyncState())
}

// PutOpenRouterModelSync updates OpenRouter model sync settings.
func (h *ModelsHandler) PutOpenRouterModelSync(c *gin.Context) {
	var body struct {
		Enabled         bool `json:"enabled"`
		IntervalMinutes int  `json:"interval_minutes"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	state, err := h.service(c).UpdateOpenRouterModelSyncSettings(body.Enabled, body.IntervalMinutes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

// PostOpenRouterModelSyncRun manually runs OpenRouter model sync now.
func (h *ModelsHandler) PostOpenRouterModelSyncRun(c *gin.Context) {
	ctx := c.Request.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	result, state, err := h.service(c).RunOpenRouterModelSync(ctx)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "state": state})
		return
	}
	h.notifyModelConfigMutated(c)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "result": result, "state": state})
}

func (h *ModelsHandler) notifyModelConfigMutated(c *gin.Context) {
	if effectiveTenantID(c) != identity.SystemTenantID {
		return
	}
	if h == nil || h.Handler == nil || h.onModelConfigMutated == nil {
		return
	}
	h.onModelConfigMutated()
}
