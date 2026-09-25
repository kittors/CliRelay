package management

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
)

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
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
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
