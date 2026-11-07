package management

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	log "github.com/sirupsen/logrus"
)

// The outbound secret masking switch of a tenant (see
// internal/contentmoderation/secretmask_rules.go). It is off until a tenant
// switches it on, so nothing here changes how a tenant that never calls these
// endpoints is served.
//
//	GET/PUT /content-moderation/secret-masking
//
// The tenant's placeholder key is made and kept by the backend; neither
// endpoint ever returns it.

func (h *Handler) secretMaskingRegistry() *contentmoderation.SecretMaskingRegistry {
	if h != nil && h.secretMasking != nil {
		return h.secretMasking
	}
	return contentmoderation.SecretMasking()
}

type secretMaskingWrite struct {
	Enabled *bool `json:"enabled"`
}

type secretMaskingResponse struct {
	Enabled bool `json:"enabled"`
	// Persistent is false when no settings store is attached; the switch can
	// then be read but not saved.
	Persistent bool       `json:"persistent"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

func (h *Handler) secretMaskingView(settings contentmoderation.SecretMaskingSettings) secretMaskingResponse {
	view := secretMaskingResponse{Enabled: settings.Enabled, Persistent: h.secretMaskingRegistry().Persistent()}
	if !settings.UpdatedAt.IsZero() {
		updated := settings.UpdatedAt
		view.UpdatedAt = &updated
	}
	return view
}

// GetSecretMasking returns the secret masking switch of the tenant the session
// acts in.
func (h *Handler) GetSecretMasking(c *gin.Context) {
	c.JSON(http.StatusOK, h.secretMaskingView(h.secretMaskingRegistry().For(effectiveTenantID(c))))
}

// PutSecretMasking switches the tenant's secret masking on or off.
func (h *Handler) PutSecretMasking(c *gin.Context) {
	var body secretMaskingWrite
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": `the body must be {"enabled": true} or {"enabled": false}`}})
		return
	}
	saved, err := h.secretMaskingRegistry().SetEnabled(c.Request.Context(), effectiveTenantID(c), *body.Enabled)
	if err != nil {
		// The switch a write could not save has not changed, so each of these is
		// safe to retry.
		switch {
		case errors.Is(err, contentmoderation.ErrSecretMaskingNotPersistent):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "secret_masking_not_persistent", "message": err.Error()}})
		case errors.Is(err, contentmoderation.ErrSecretMaskingConflict):
			c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "secret_masking_conflict", "message": "the secret masking switch was changed by someone else while saving; reload and try again"}})
		default:
			log.WithError(err).Warn("secret masking: saving the switch failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "secret_masking_failed", "message": "saving the secret masking switch failed"}})
		}
		return
	}
	c.JSON(http.StatusOK, h.secretMaskingView(saved))
}
