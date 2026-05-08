package management

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func (h *Handler) GetCcSwitchImportConfigs(c *gin.Context) {
	items := usage.ListCcSwitchImportConfigs()
	if items == nil {
		items = []usage.CcSwitchImportConfigRow{}
	}
	c.JSON(http.StatusOK, gin.H{
		"ccswitch-import-configs": items,
		"items":                   items,
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
			AllowedChannelGroups: items[idx].AllowedChannelGroups,
			EndpointPath:         items[idx].EndpointPath,
			UsageAutoInterval:    items[idx].UsageAutoInterval,
			APIKeyField:          strings.TrimSpace(items[idx].APIKeyField),
			CreatedAt:            strings.TrimSpace(items[idx].CreatedAt),
			UpdatedAt:            strings.TrimSpace(items[idx].UpdatedAt),
		}

		switch items[idx].ClientType {
		case "claude", "codex", "gemini":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "client-type must be one of claude, codex, gemini"})
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

	if err := usage.ReplaceAllCcSwitchImportConfigs(items); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
