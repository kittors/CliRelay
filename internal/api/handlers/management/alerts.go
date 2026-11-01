package management

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	log "github.com/sirupsen/logrus"
)

// The alert routes are tenant-scoped: each tenant has its own channels and
// thresholds and hears only about its own accounts, keys and end users (see the
// alerting package doc). They sit behind tenant_settings.read / .write, the pair
// that guards the other tenant-level knobs, and act on the effective tenant, so a
// platform administrator editing a tenant's alerts does it with the same header
// the rest of the panel uses.

// GetAlertSettings returns the tenant's alert settings with every secret masked.
func (h *Handler) GetAlertSettings(c *gin.Context) {
	view, err := h.alerts().View(effectiveTenantID(c))
	if err != nil {
		respondAlertError(c, err)
		return
	}
	// The view holds the tail of webhook addresses; keep it out of shared caches.
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, view)
}

// PutAlertSettings applies a partial update and returns the result. An omitted
// field keeps what is stored; a secret echoed back as the mask it was shown as
// keeps its value (see alerting.ChannelPatch).
func (h *Handler) PutAlertSettings(c *gin.Context) {
	var patch alerting.Patch
	if err := c.ShouldBindJSON(&patch); err != nil {
		respondInvalidAlertRequest(c, "the body must be a JSON object of alert settings")
		return
	}
	view, err := h.alerts().Update(effectiveTenantID(c), patch)
	if err != nil {
		respondAlertError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, view)
}

// PostAlertTest sends a test message. The body may be empty (every enabled
// channel), name a saved channel, or carry a draft that is not saved yet.
func (h *Handler) PostAlertTest(c *gin.Context) {
	var request alerting.TestRequest
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		respondInvalidAlertRequest(c, "the body must be a JSON object")
		return
	}
	response, err := h.alerts().Test(c.Request.Context(), effectiveTenantID(c), request)
	if err != nil {
		respondAlertError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

// GetAlertEvents lists the newest entries of the tenant's alert history.
func (h *Handler) GetAlertEvents(c *gin.Context) {
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			respondInvalidAlertRequest(c, "limit must be a positive number")
			return
		}
		limit = parsed
	}
	response, err := h.alerts().Events(effectiveTenantID(c), limit)
	if err != nil {
		respondAlertError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func respondInvalidAlertRequest(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": message}})
}

// respondAlertError answers with the structured error body the panel knows how
// to show. Messages are written for the operator: a rejected field names itself,
// and nothing here echoes a stored secret or the text of an internal error.
func respondAlertError(c *gin.Context, err error) {
	var fieldErr *alerting.FieldError
	switch {
	case errors.As(err, &fieldErr):
		// The field goes under details, where the panel (and the other settings
		// endpoints) put it, so it can point at the input that was refused.
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_alert_settings", "message": fieldErr.Error(), "details": gin.H{"field": fieldErr.Field}}})
	case errors.Is(err, alerting.ErrNotPersistent):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "alerts_unavailable", "message": err.Error()}})
	case errors.Is(err, alerting.ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "alerts_conflict", "message": "the alert settings were changed by someone else at the same time; reload and try again"}})
	case errors.Is(err, alerting.ErrTooManyTests):
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "too_many_tests", "message": err.Error()}})
	default:
		log.WithError(err).Warn("alerting: management request failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "alerts_failed", "message": "the alert settings could not be read or saved"}})
	}
}
