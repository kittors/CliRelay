package management

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
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
	c.JSON(http.StatusOK, gin.H{"enabled": h.cfg.RequestLogStorage.StoreContent})
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
	h.mu.Lock()
	previous := h.cfg.RequestLogStorage.StoreContent
	h.cfg.RequestLogStorage.StoreContent = enabled
	if err := settingsstore.SaveConfig(h.cfg, h.configFilePath); err != nil {
		h.cfg.RequestLogStorage.StoreContent = previous
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
		return
	}
	cfg := h.cfg
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
