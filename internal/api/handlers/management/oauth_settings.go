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
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]string
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
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
	if err := c.ShouldBindJSON(&body); err != nil || body.Provider == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	_, err := oauthSettingsService(h, c).PatchExcludedModels(*body.Provider, body.Models)
	if err == oauthsettings.ErrInvalidProvider {
		c.JSON(400, gin.H{"error": "invalid provider"})
		return
	}
	if err == oauthsettings.ErrProviderNotFound {
		c.JSON(404, gin.H{"error": "provider not found"})
		return
	}
	h.commitRequestConfig(c, h.providerConfigForTenant(c))
}

func (h *ProviderKeysHandler) DeleteOAuthExcludedModels(c *gin.Context) {
	_, err := oauthSettingsService(h, c).DeleteExcludedModels(c.Query("provider"))
	if err == oauthsettings.ErrInvalidProvider {
		c.JSON(400, gin.H{"error": "missing provider"})
		return
	}
	if err == oauthsettings.ErrProviderNotFound {
		c.JSON(404, gin.H{"error": "provider not found"})
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
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]config.OAuthModelAlias
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]config.OAuthModelAlias `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
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
		c.JSON(400, gin.H{"error": "invalid body"})
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
		c.JSON(400, gin.H{"error": "invalid channel"})
		return
	}
	if err == oauthsettings.ErrChannelNotFound {
		c.JSON(404, gin.H{"error": "channel not found"})
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
		c.JSON(400, gin.H{"error": "missing channel"})
		return
	}
	_, err := oauthSettingsService(h, c).DeleteModelAlias(channel)
	if err == oauthsettings.ErrChannelNotFound {
		c.JSON(404, gin.H{"error": "channel not found"})
		return
	}
	h.commitRequestConfig(c, h.providerConfigForTenant(c))
}
