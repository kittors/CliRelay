package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func currentRoutingConfigForTenant(cfg *config.Config, tenantID string) config.RoutingConfig {
	if stored := usage.GetRoutingConfigForTenant(tenantID); stored != nil {
		return *stored
	}
	if tenantID == identity.SystemTenantID && cfg != nil {
		return cfg.Routing
	}
	return config.RoutingConfig{IncludeDefaultGroup: true}
}

func currentRoutingConfig(cfg *config.Config) config.RoutingConfig {
	return currentRoutingConfigForTenant(cfg, identity.SystemTenantID)
}

func storedAPIKeyEntries(tenantID string) []config.APIKeyEntry {
	return apikeysettings.NewService(nil, apikeysettings.WithTenantID(tenantID)).ListEntries()
}

func effectiveTenantID(c *gin.Context) string {
	if principal, ok := principalFromContext(c); ok && principal.EffectiveTenant.ID != "" {
		return principal.EffectiveTenant.ID
	}
	return identity.SystemTenantID
}

// routingConfigResponse is what GET /routing-config returns: the stored config
// plus what this backend can enforce. The panel is updated on its own schedule
// (it follows the latest codeProxy release, not this binary), so it has to ask
// before writing a field an older backend would drop. A panel that wrote
// excluded-models to a backend without them would clear the group's allow list
// and leave it serving every model.
type routingConfigResponse struct {
	config.RoutingConfig
	Capabilities routingConfigCapabilities `json:"capabilities"`
	// Version is sent back with a write to detect a concurrent change.
	Version int64 `json:"version"`
}

type routingConfigCapabilities struct {
	ChannelGroupExcludedModels bool `json:"channel-group-excluded-models"`
	// ChannelGroupFollowUpstreamModels: channel groups store and enforce
	// follow-upstream-models. An older backend drops the field on save, so the
	// panel only shows the switch when this is advertised.
	ChannelGroupFollowUpstreamModels bool `json:"channel-group-follow-upstream-models"`
	// ChannelGroupFirstTokenTimeout and ChannelGroupBusyWait: channel groups
	// store and enforce first-token-timeout-seconds and busy-wait-max-seconds.
	// The panel only shows those fields when these are advertised, because an
	// older backend drops them on save.
	ChannelGroupFirstTokenTimeout bool `json:"channel-group-first-token-timeout"`
	ChannelGroupBusyWait          bool `json:"channel-group-busy-wait"`
	// ChannelGroupResetAwareDistribution: channel groups accept the
	// reset-aware distribution. An older backend reads the unknown value as
	// weighted, so the panel only offers it when this is advertised.
	ChannelGroupResetAwareDistribution bool `json:"channel-group-distribution-reset-aware"`
	// RoutingSimulation: GET /routing-simulation answers. The panel only offers
	// the simulator when this is advertised.
	RoutingSimulation bool `json:"routing-simulation"`
}

func (h *Handler) GetRoutingConfig(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		// Shared platform credentials take part in this tenant's routing, so
		// their channel names are this tenant's to reference.
		auths = h.authManager.ListUsableForTenant(tenantID)
	}
	routing := currentRoutingConfigForTenant(h.cfg, tenantID)
	_, version := usage.GetRoutingConfigWithVersionForTenant(tenantID)
	if known, err := collectKnownChannels(h.cfg, auths, ""); err == nil {
		routing = canonicalizeRoutingConfigChannels(routing, known)
	}
	c.JSON(http.StatusOK, routingConfigResponse{
		RoutingConfig: routing,
		Capabilities: routingConfigCapabilities{
			ChannelGroupExcludedModels:         true,
			ChannelGroupFollowUpstreamModels:   true,
			ChannelGroupFirstTokenTimeout:      true,
			ChannelGroupBusyWait:               true,
			ChannelGroupResetAwareDistribution: true,
			RoutingSimulation:                  h.routingSimulationAvailable(),
		},
		Version: version,
	})
}

func (h *Handler) PutRoutingConfig(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var body config.RoutingConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	// Checked before SanitizeRouting, which would quietly clamp what the
	// operator typed into range.
	if err := config.ValidateRoutingStreamGuards(body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	candidate := &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeyEntries: storedAPIKeyEntries(tenantID),
		},
		Routing: body,
	}
	candidate.SanitizeRouting()

	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.ListUsableForTenant(tenantID)
	}
	if known, err := collectKnownChannels(h.cfg, auths, ""); err == nil {
		candidate.Routing = canonicalizeRoutingConfigChannels(candidate.Routing, known)
		candidate.APIKeyEntries = canonicalizeAPIKeyEntriesChannels(candidate.APIKeyEntries, known)
	}
	if err := validateRoutingAndAPIKeyRestrictions(candidate, auths); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// A PUT replaces the whole routing config, so it is checked against the
	// version the client read, when it sent one.
	version, err := usage.CompareAndSwapRoutingConfigForTenant(c.Request.Context(), tenantID, candidate.Routing, requestExpectedVersion(c))
	if err != nil {
		writeConfigSaveError(c, "failed to save routing config", err)
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
		h.cfg.Routing.IncludeDefaultGroup = true
	}
	tenantCfg := usage.BuildTenantRuntimeConfig(h.cfg, tenantID)
	tenantCfg.Routing = candidate.Routing
	if tenantID == identity.SystemTenantID {
		h.cfg.Routing = candidate.Routing
	}
	cfgRef := h.cfg
	h.mu.Unlock()

	if h.authManager != nil {
		h.authManager.SetConfigForTenant(tenantID, &tenantCfg)
	}

	setVersionHeader(c, version)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	if tenantID == identity.SystemTenantID && h != nil && h.onConfigMutated != nil {
		h.onConfigMutated(cfgRef)
	}
}
