package management

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	providersettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/providers"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
)

func writeProviderReplaceError(c *gin.Context, label string, err error) {
	writeError(c, http.StatusBadRequest, "invalid_provider_config", fmt.Sprintf("%s配置校验失败：%s", label, err.Error()))
}

func writeProviderPatchError(c *gin.Context, label string, err error) {
	if errors.Is(err, providersettings.ErrItemNotFound) {
		writeNotFoundError(c, label+"条目", "")
		return
	}
	writeError(c, http.StatusBadRequest, "invalid_provider_config", fmt.Sprintf("更新%s配置失败：%s", label, err.Error()))
}

// persistProviderSettingsToYAML is the path without a database, where
// config.yaml is the store: the edited copy's changes are adopted into the
// live config and the file is written, as before versioning existed.
func (h *ProviderKeysHandler) persistProviderSettingsToYAML(c *gin.Context, cfg *config.Config) bool {
	keys := settingsstore.ChangedKeys(cfg)
	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	live := h.cfg
	settingsstore.AdoptKeys(live, cfg, keys...)
	err := settingsstore.SaveConfig(live, h.configFilePath)
	mutated := h.onConfigMutated
	h.mu.Unlock()
	if err != nil {
		writeInternalError(c, "保存提供商配置文件", err)
		return false
	}
	payload := gin.H{"status": "ok"}
	if errCleanup := h.cleanupRemovedProviderModerationBindings(c.Request.Context(), c, effectiveTenantID(c), cfg); errCleanup != nil {
		payload["warning"] = fmt.Sprintf("provider saved but content moderation binding cleanup failed: %v", errCleanup)
	}
	c.JSON(http.StatusOK, payload)
	if mutated != nil {
		mutated(live)
	}
	return true
}
