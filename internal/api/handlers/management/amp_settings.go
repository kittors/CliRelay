package management

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	ampsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/amp"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
)

func ampSettingsService(h *Handler) *ampsettings.Service {
	if h == nil {
		return ampsettings.NewService(nil)
	}
	return ampsettings.NewService(h.cfg)
}

// GetAmpCode returns the complete ampcode configuration.
func (h *Handler) GetAmpCode(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingAmpCode, gin.H{"ampcode": ampSettingsService(h).Snapshot()})
}

// GetAmpUpstreamURL returns the ampcode upstream URL.
func (h *Handler) GetAmpUpstreamURL(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingAmpCode, gin.H{"upstream-url": ampSettingsService(h).UpstreamURL()})
}

// PutAmpUpstreamURL updates the ampcode upstream URL.
func (h *Handler) PutAmpUpstreamURL(c *gin.Context) {
	h.updateStringField(c, func(cfg *config.Config, v string) { ampsettings.NewService(cfg).SetUpstreamURL(v) })
}

// DeleteAmpUpstreamURL clears the ampcode upstream URL.
func (h *Handler) DeleteAmpUpstreamURL(c *gin.Context) {
	h.mutateSystemConfig(c, func(cfg *config.Config) error { ampsettings.NewService(cfg).ClearUpstreamURL(); return nil })
}

// GetAmpUpstreamAPIKey returns the ampcode upstream API key.
func (h *Handler) GetAmpUpstreamAPIKey(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingAmpCode, gin.H{"upstream-api-key": ampSettingsService(h).UpstreamAPIKey()})
}

// PutAmpUpstreamAPIKey updates the ampcode upstream API key.
func (h *Handler) PutAmpUpstreamAPIKey(c *gin.Context) {
	h.updateStringField(c, func(cfg *config.Config, v string) { ampsettings.NewService(cfg).SetUpstreamAPIKey(v) })
}

// DeleteAmpUpstreamAPIKey clears the ampcode upstream API key.
func (h *Handler) DeleteAmpUpstreamAPIKey(c *gin.Context) {
	h.mutateSystemConfig(c, func(cfg *config.Config) error { ampsettings.NewService(cfg).ClearUpstreamAPIKey(); return nil })
}

// GetAmpRestrictManagementToLocalhost returns the localhost restriction setting.
func (h *Handler) GetAmpRestrictManagementToLocalhost(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingAmpCode, gin.H{"restrict-management-to-localhost": ampSettingsService(h).RestrictManagementToLocalhost()})
}

// PutAmpRestrictManagementToLocalhost updates the localhost restriction setting.
func (h *Handler) PutAmpRestrictManagementToLocalhost(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { ampsettings.NewService(cfg).SetRestrictManagementToLocalhost(v) })
}

// GetAmpModelMappings returns the ampcode model mappings.
func (h *Handler) GetAmpModelMappings(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingAmpCode, gin.H{"model-mappings": ampSettingsService(h).ModelMappings()})
}

// PutAmpModelMappings replaces all ampcode model mappings.
func (h *Handler) PutAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mutateSystemConfig(c, func(cfg *config.Config) error { ampsettings.NewService(cfg).SetModelMappings(body.Value); return nil })
}

// PatchAmpModelMappings adds or updates model mappings.
func (h *Handler) PatchAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	h.mutateSystemConfig(c, func(cfg *config.Config) error { ampsettings.NewService(cfg).PatchModelMappings(body.Value); return nil })
}

// DeleteAmpModelMappings removes specified model mappings by "from" field.
func (h *Handler) DeleteAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Value) == 0 {
		h.mutateSystemConfig(c, func(cfg *config.Config) error { ampsettings.NewService(cfg).DeleteModelMappings(nil); return nil })
		return
	}

	h.mutateSystemConfig(c, func(cfg *config.Config) error {
		ampsettings.NewService(cfg).DeleteModelMappings(body.Value)
		return nil
	})
}

// GetAmpForceModelMappings returns whether model mappings are forced.
func (h *Handler) GetAmpForceModelMappings(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingAmpCode, gin.H{"force-model-mappings": ampSettingsService(h).ForceModelMappings()})
}

// PutAmpForceModelMappings updates the force model mappings setting.
func (h *Handler) PutAmpForceModelMappings(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { ampsettings.NewService(cfg).SetForceModelMappings(v) })
}

// GetAmpUpstreamAPIKeys returns the ampcode upstream API keys mapping.
func (h *Handler) GetAmpUpstreamAPIKeys(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingAmpCode, gin.H{"upstream-api-keys": ampSettingsService(h).UpstreamAPIKeys()})
}

// PutAmpUpstreamAPIKeys replaces all ampcode upstream API keys mappings.
func (h *Handler) PutAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mutateSystemConfig(c, func(cfg *config.Config) error { ampsettings.NewService(cfg).SetUpstreamAPIKeys(body.Value); return nil })
}

// PatchAmpUpstreamAPIKeys adds or updates upstream API keys entries.
// Matching is done by upstream-api-key value.
func (h *Handler) PatchAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	h.mutateSystemConfig(c, func(cfg *config.Config) error {
		ampsettings.NewService(cfg).PatchUpstreamAPIKeys(body.Value)
		return nil
	})
}

// DeleteAmpUpstreamAPIKeys removes specified upstream API keys entries.
// Body must be JSON: {"value": ["<upstream-api-key>", ...]}.
// If "value" is an empty array, clears all entries.
// If JSON is invalid or "value" is missing/null, returns 400 and does not persist any change.
func (h *Handler) DeleteAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	if body.Value == nil {
		c.JSON(400, gin.H{"error": "missing value"})
		return
	}

	if len(body.Value) == 0 {
		h.mutateSystemConfig(c, func(cfg *config.Config) error {
			_ = ampsettings.NewService(cfg).DeleteUpstreamAPIKeys(body.Value)
			return nil
		})
		return
	}

	h.mutateSystemConfig(c, func(cfg *config.Config) error {
		if err := ampsettings.NewService(cfg).DeleteUpstreamAPIKeys(body.Value); err != nil {
			return ampsettings.ErrEmptyValue
		}
		return nil
	})
}
