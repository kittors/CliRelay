package management

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func (h *Handler) GetCcSwitchImportConfigs(c *gin.Context) {
	setVersionHeader(c, usage.ConfigCollectionVersion(configsync.DomainCcSwitch, effectiveTenantID(c)))
	items := usage.ListCcSwitchImportConfigsForTenant(effectiveTenantID(c))
	if items == nil {
		items = []usage.CcSwitchImportConfigRow{}
	}
	items = usage.AttachCcSwitchCodexModelCatalogs(items)
	c.JSON(http.StatusOK, gin.H{
		"ccswitch-import-configs": items,
		"items":                   items,
		// The panel auto-updates and may run against an older backend; it only
		// offers the apps listed here (and falls back to claude/codex/gemini when
		// the field is missing).
		"client-types": usage.CcSwitchClientTypes(),
		// Fields an older backend would silently drop on PUT; the panel only
		// offers the matching controls when they are listed as true here.
		"capabilities": gin.H{"codex-context-window": true},
	})
}

func (h *Handler) PutCcSwitchImportConfigs(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	var items []usage.CcSwitchImportConfigRow
	if err = json.Unmarshal(data, &items); err != nil {
		var body struct {
			Items []usage.CcSwitchImportConfigRow `json:"items"`
		}
		if err2 := json.Unmarshal(data, &body); err2 != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		items = body.Items
	}

	for idx := range items {
		items[idx] = usage.CcSwitchImportConfigRow{
			ID:                   strings.TrimSpace(items[idx].ID),
			ClientType:           strings.ToLower(strings.TrimSpace(items[idx].ClientType)),
			ProviderName:         strings.TrimSpace(items[idx].ProviderName),
			Note:                 strings.TrimSpace(items[idx].Note),
			DefaultModel:         strings.TrimSpace(items[idx].DefaultModel),
			ModelMappings:        items[idx].ModelMappings,
			AllowedChannelGroups: items[idx].AllowedChannelGroups,
			RoutePath:            items[idx].RoutePath,
			EndpointPath:         items[idx].EndpointPath,
			UsageAutoInterval:    items[idx].UsageAutoInterval,
			APIKeyField:          strings.TrimSpace(items[idx].APIKeyField),
			CodexContextWindow:   items[idx].CodexContextWindow,
			CreatedAt:            strings.TrimSpace(items[idx].CreatedAt),
			UpdatedAt:            strings.TrimSpace(items[idx].UpdatedAt),
		}

		if !usage.IsCcSwitchClientType(items[idx].ClientType) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "client-type must be one of " + strings.Join(usage.CcSwitchClientTypes(), ", ")})
			return
		}

		if items[idx].ID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
			return
		}
		if items[idx].ProviderName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider-name is required"})
			return
		}
		if items[idx].DefaultModel == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "default-model is required"})
			return
		}
	}

	version, err := usage.ReplaceAllCcSwitchImportConfigsForTenantExpect(c.Request.Context(), effectiveTenantID(c), items, requestExpectedVersion(c))
	if err != nil {
		if writeConfigConflict(c, err) {
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	setVersionHeader(c, version)

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
