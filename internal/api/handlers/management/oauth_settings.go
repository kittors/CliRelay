package management

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	oauthsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/oauth"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
)

func oauthSettingsService(h *ProviderKeysHandler, c *gin.Context) *oauthsettings.Service {
	if h == nil {
		return oauthsettings.NewService(nil)
	}
	return oauthsettings.NewService(h.providerConfigForTenant(c))
}

// oauth-excluded-models: map[string][]string
func (h *ProviderKeysHandler) GetOAuthExcludedModels(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingOAuthExcludedModels, gin.H{"oauth-excluded-models": oauthSettingsService(h, c).ExcludedModels()})
}

func (h *ProviderKeysHandler) PutOAuthExcludedModels(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		writeReadBodyError(c, err, 0)
		return
	}
	var entries map[string][]string
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			writeInvalidBodyError(c, err)
			return
		}
		entries = wrapper.Items
	}
	oauthSettingsService(h, c).SetExcludedModels(entries)
	h.commitRequestConfig(c, h.providerConfigForTenant(c))
}

func (h *ProviderKeysHandler) PatchOAuthExcludedModels(c *gin.Context) {
	var body struct {
		Provider *string  `json:"provider"`
		Models   []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}
	if body.Provider == nil {
		writeMissingFieldError(c, "provider", "请指定要更新排除模型的 OAuth 提供商名称")
		return
	}
	_, err := oauthSettingsService(h, c).PatchExcludedModels(*body.Provider, body.Models)
	if err == oauthsettings.ErrInvalidProvider {
		writeInvalidFieldError(c, "provider", "提供商名称不能为空或无效")
		return
	}
	if err == oauthsettings.ErrProviderNotFound {
		writeNotFoundError(c, "OAuth 排除模型提供商配置", *body.Provider)
		return
	}
	h.commitRequestConfig(c, h.providerConfigForTenant(c))
}

func (h *ProviderKeysHandler) DeleteOAuthExcludedModels(c *gin.Context) {
	provider := strings.TrimSpace(c.Query("provider"))
	_, err := oauthSettingsService(h, c).DeleteExcludedModels(provider)
	if err == oauthsettings.ErrInvalidProvider {
		writeMissingFieldError(c, "provider", "请通过查询参数 provider 指定要删除排除模型配置的提供商名称")
		return
	}
	if err == oauthsettings.ErrProviderNotFound {
		writeNotFoundError(c, "OAuth 排除模型提供商配置", provider)
		return
	}
	h.commitRequestConfig(c, h.providerConfigForTenant(c))
}

// oauth-model-alias: map[string][]OAuthModelAlias
func (h *ProviderKeysHandler) GetOAuthModelAlias(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingOAuthModelAlias, gin.H{"oauth-model-alias": oauthSettingsService(h, c).ModelAlias()})
}

func (h *ProviderKeysHandler) PutOAuthModelAlias(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		writeReadBodyError(c, err, 0)
		return
	}
	var entries map[string][]config.OAuthModelAlias
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]config.OAuthModelAlias `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			writeInvalidBodyError(c, err)
			return
		}
		entries = wrapper.Items
	}
	oauthSettingsService(h, c).SetModelAlias(entries)
	h.commitRequestConfig(c, h.providerConfigForTenant(c))
}

func (h *ProviderKeysHandler) PatchOAuthModelAlias(c *gin.Context) {
	var body struct {
		Provider *string                  `json:"provider"`
		Channel  *string                  `json:"channel"`
		Aliases  []config.OAuthModelAlias `json:"aliases"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		writeInvalidBodyError(c, errBindJSON)
		return
	}
	channelRaw := ""
	if body.Channel != nil {
		channelRaw = *body.Channel
	} else if body.Provider != nil {
		channelRaw = *body.Provider
	}
	_, err := oauthSettingsService(h, c).PatchModelAlias(channelRaw, body.Aliases)
	if err == oauthsettings.ErrInvalidChannel {
		writeInvalidFieldError(c, "channel", "请提供有效的渠道或提供商名称")
		return
	}
	if err == oauthsettings.ErrChannelNotFound {
		writeNotFoundError(c, "OAuth 模型别名渠道配置", channelRaw)
		return
	}
	h.commitRequestConfig(c, h.providerConfigForTenant(c))
}

func (h *ProviderKeysHandler) DeleteOAuthModelAlias(c *gin.Context) {
	channel := strings.ToLower(strings.TrimSpace(c.Query("channel")))
	if channel == "" {
		channel = strings.ToLower(strings.TrimSpace(c.Query("provider")))
	}
	if channel == "" {
		writeMissingFieldError(c, "channel", "请通过查询参数 channel 或 provider 指定要删除模型别名的渠道")
		return
	}
	_, err := oauthSettingsService(h, c).DeleteModelAlias(channel)
	if err == oauthsettings.ErrChannelNotFound {
		writeNotFoundError(c, "OAuth 模型别名渠道配置", channel)
		return
	}
	h.commitRequestConfig(c, h.providerConfigForTenant(c))
}
