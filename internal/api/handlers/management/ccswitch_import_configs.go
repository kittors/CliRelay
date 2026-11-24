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
		writeReadBodyError(c, err, 0)
		return
	}

	var items []usage.CcSwitchImportConfigRow
	if err = json.Unmarshal(data, &items); err != nil {
		var body struct {
			Items []usage.CcSwitchImportConfigRow `json:"items"`
		}
		if err2 := json.Unmarshal(data, &body); err2 != nil {
			writeInvalidBodyError(c, err)
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
			writeInvalidFieldError(c, "client-type", "仅支持以下客户端类型："+strings.Join(usage.CcSwitchClientTypes(), ", "))
			return
		}

		if items[idx].ID == "" {
			writeMissingFieldError(c, "id", "每条 CC Switch 导入配置必须包含非空 id")
			return
		}
		if items[idx].ProviderName == "" {
			writeMissingFieldError(c, "provider-name", "每条 CC Switch 导入配置必须包含非空 provider-name")
			return
		}
		if items[idx].DefaultModel == "" {
			writeMissingFieldError(c, "default-model", "每条 CC Switch 导入配置必须包含非空 default-model")
			return
		}
	}

	version, err := usage.ReplaceAllCcSwitchImportConfigsForTenantExpect(c.Request.Context(), effectiveTenantID(c), items, requestExpectedVersion(c))
	if err != nil {
		if writeConfigConflict(c, err) {
			return
		}
		writeError(c, http.StatusBadRequest, "save_ccswitch_configs_failed", "保存 CC Switch 导入配置失败："+err.Error())
		return
	}
	setVersionHeader(c, version)

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
