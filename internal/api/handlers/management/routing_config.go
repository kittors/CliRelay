package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func currentRoutingConfig(cfg *config.Config) config.RoutingConfig {
	if cfg == nil {
		return config.RoutingConfig{IncludeDefaultGroup: true}
	}
	return cfg.Routing
}

func sqliteAPIKeyEntries() []config.APIKeyEntry {
	rows := usage.ListAPIKeys()
	entries := make([]config.APIKeyEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, row.ToConfigEntry())
	}
	return entries
}

func (h *Handler) GetRoutingConfig(c *gin.Context) {
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.List()
	}
	routing := currentRoutingConfig(h.cfg)
	if known, err := collectKnownChannels(h.cfg, auths, ""); err == nil {
		routing = canonicalizeRoutingConfigChannels(routing, known)
	}
	c.JSON(http.StatusOK, routing)
}

func (h *Handler) PutRoutingConfig(c *gin.Context) {
	var body config.RoutingConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	candidate := &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeyEntries: sqliteAPIKeyEntries(),
		},
		Routing: body,
	}
	candidate.SanitizeRouting()

	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.List()
	}
	if known, err := collectKnownChannels(h.cfg, auths, ""); err == nil {
		candidate.Routing = canonicalizeRoutingConfigChannels(candidate.Routing, known)
		candidate.APIKeyEntries = canonicalizeAPIKeyEntriesChannels(candidate.APIKeyEntries, known)
	}
	if err := validateRoutingAndAPIKeyRestrictions(candidate, auths); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := usage.UpsertRoutingConfig(candidate.Routing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
		h.cfg.Routing.IncludeDefaultGroup = true
	}
	h.cfg.Routing = candidate.Routing
	cfgRef := h.cfg
	h.mu.Unlock()

	if h.authManager != nil {
		h.authManager.SetConfig(cfgRef)
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	if h != nil && h.onConfigMutated != nil {
		h.onConfigMutated(cfgRef)
	}
}
