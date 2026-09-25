package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type requestLogBodyStorageUpdate struct {
	Value         *bool `json:"value"`
	ClearExisting bool  `json:"clear_existing"`
}

func (h *Handler) GetRequestLogBodyStorage(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingRequestLogStorage, gin.H{"enabled": h.cfg.RequestLogStorage.StoreContent})
}

// GetRequestLogStorageStatus returns retention policy + live table sizes.
// Cleaning request_logs never clears usage_rollup_buckets stats.
func (h *Handler) GetRequestLogStorageStatus(c *gin.Context) {
	status, err := usage.GetRequestLogStorageStatus()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}

func (h *Handler) PutRequestLogBodyStorage(c *gin.Context) {
	var body requestLogBodyStorageUpdate
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if !*body.Value && !body.ClearExisting {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "confirmation_required",
			"message": "clear_existing must be true when disabling request log body storage",
		})
		return
	}
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}

	enabled := *body.Value
	cfg, err := h.commitRequestLogBodyStorage(c, enabled)
	if err != nil {
		writeConfigSaveError(c, "failed to save config", err)
		return
	}
	h.mu.Lock()
	mutated := h.onConfigMutated
	h.mu.Unlock()

	// Stop new body writes before deleting historical bodies. Request details and
	// lightweight request records remain available.
	usage.SetRequestLogBodyStorageEnabled(enabled)
	if mutated != nil {
		mutated(cfg)
	}

	response := gin.H{"enabled": enabled}
	if !enabled {
		result, err := usage.PurgeStoredRequestBodies()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "cleanup_failed",
				"message": err.Error(),
				"enabled": false,
			})
			return
		}
		response["cleanup"] = result
	}
	c.JSON(http.StatusOK, response)
}

// commitRequestLogBodyStorage stores the toggle and returns the live config it
// was applied to. It edits a fresh copy like mutateSystemConfig, but the
// caller has to answer the request itself: disabling also purges bodies.
func (h *Handler) commitRequestLogBodyStorage(c *gin.Context, enabled bool) (*config.Config, error) {
	if !settingsstore.StoreAvailable() {
		h.mu.Lock()
		defer h.mu.Unlock()
		previous := h.cfg.RequestLogStorage.StoreContent
		h.cfg.RequestLogStorage.StoreContent = enabled
		if err := settingsstore.SaveConfig(h.cfg, h.configFilePath); err != nil {
			h.cfg.RequestLogStorage.StoreContent = previous
			return nil, err
		}
		return h.cfg, nil
	}
	fresh := h.freshConfig(identity.SystemTenantID)
	fresh.RequestLogStorage.StoreContent = enabled
	keys, err := settingsstore.CommitTenantConfig(c.Request.Context(), identity.SystemTenantID, fresh, requestVersion(c), h.configFilePath)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	settingsstore.AdoptKeys(h.cfg, fresh, keys...)
	return h.cfg, nil
}
