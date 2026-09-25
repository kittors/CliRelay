package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	providersettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/providers"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
)

// Command Code provider key endpoints.
//
// Kept beside provider_settings.go rather than inside it so that file stays
// under the 800-line structure gate.

// commandcode-api-key: []CommandCodeKey
func (h *ProviderKeysHandler) GetCommandCodeKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingCommandCodeKeys, gin.H{"commandcode-api-key": providerSettingsService(h, c).CommandCodeKeys()})
}

func (h *ProviderKeysHandler) PutCommandCodeKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.CommandCodeKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.CommandCodeKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	if err := providerSettingsService(h, c).ReplaceCommandCodeKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchCommandCodeKey(c *gin.Context) {
	var body struct {
		APIKey *string                            `json:"api-key"`
		Name   *string                            `json:"name"`
		Index  *int                               `json:"index"`
		Value  *providersettings.CommandCodePatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchCommandCodeKey(body.Index, body.APIKey, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteCommandCodeKey(c *gin.Context) {
	if apiKey := strings.TrimSpace(c.Query("api-key")); apiKey != "" {
		if providerSettingsService(h, c).DeleteCommandCodeKeyByAPIKey(apiKey) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		if providerSettingsService(h, c).DeleteCommandCodeKeyByName(name) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h, c).DeleteCommandCodeKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, name, or index"})
}
