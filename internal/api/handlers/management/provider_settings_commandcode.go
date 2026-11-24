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
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingCommandCodeKeys, providerListBody("commandcode-api-key", providerSettingsService(h, c).CommandCodeKeys()))
}

func (h *ProviderKeysHandler) PutCommandCodeKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		writeReadBodyError(c, err, 0)
		return
	}
	var arr []config.CommandCodeKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.CommandCodeKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			writeInvalidBodyError(c, err)
			return
		}
		arr = obj.Items
	}
	if err := providerSettingsService(h, c).ReplaceCommandCodeKeys(arr); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_provider_config", "Command Code 密钥配置校验失败："+err.Error())
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
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}
	if body.Value == nil {
		writeMissingFieldError(c, "value", "请提供要修改的 Command Code 密钥字段对象 value")
		return
	}
	if err := providerSettingsService(h, c).PatchCommandCodeKey(body.Index, body.APIKey, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			writeNotFoundError(c, "Command Code 密钥条目", "")
			return
		}
		writeError(c, http.StatusBadRequest, "invalid_provider_config", "更新 Command Code 密钥配置失败："+err.Error())
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
		writeNotFoundError(c, "Command Code 密钥条目", "")
		return
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		if providerSettingsService(h, c).DeleteCommandCodeKeyByName(name) {
			h.persistProviderSettings(c)
			return
		}
		writeNotFoundError(c, "Command Code 密钥条目", name)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h, c).DeleteCommandCodeKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	writeMissingFieldError(c, "api-key/name/index", "删除 Command Code 密钥时必须通过查询参数提供 api-key、name 或 index")
}
