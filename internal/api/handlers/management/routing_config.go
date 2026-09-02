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

func (h *Handler) GetRoutingConfig(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.ListForTenant(tenantID)
	}
	routing := currentRoutingConfigForTenant(h.cfg, tenantID)
	if known, err := collectKnownChannels(h.cfg, auths, ""); err == nil {
		routing = canonicalizeRoutingConfigChannels(routing, known)
	}
	c.JSON(http.StatusOK, routing)
}

func (h *Handler) PutRoutingConfig(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var body config.RoutingConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
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
		auths = h.authManager.ListForTenant(tenantID)
	}
	if known, err := collectKnownChannels(h.cfg, auths, ""); err == nil {
		candidate.Routing = canonicalizeRoutingConfigChannels(candidate.Routing, known)
		candidate.APIKeyEntries = canonicalizeAPIKeyEntriesChannels(candidate.APIKeyEntries, known)
	}
	if err := validateRoutingAndAPIKeyRestrictions(candidate, auths); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := usage.UpsertRoutingConfigForTenant(tenantID, candidate.Routing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	if tenantID == identity.SystemTenantID && h != nil && h.onConfigMutated != nil {
		h.onConfigMutated(cfgRef)
	}
}
