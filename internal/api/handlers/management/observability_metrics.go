package management

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/observability"
)

// observabilityMetricsView is what the panel sees of the metrics endpoint
// settings. The token's hash is never returned; the token itself only in the
// response of the write that generated it.
type observabilityMetricsView struct {
	Enabled         bool       `json:"enabled"`
	TokenConfigured bool       `json:"token_configured"`
	TokenHint       string     `json:"token_hint,omitempty"`
	TokenCreatedAt  *time.Time `json:"token_created_at,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	Path            string     `json:"path"`
	// Persistent is false when no settings store is attached; writes are then
	// refused rather than lost on the next restart.
	Persistent bool `json:"persistent"`
	// Token is set once, in the response that generated it.
	Token string `json:"token,omitempty"`
}

func newObservabilityMetricsView(exporter *observability.Exporter, token string) observabilityMetricsView {
	settings := exporter.Settings()
	view := observabilityMetricsView{
		Enabled:         settings.Enabled,
		TokenConfigured: settings.HasToken(),
		TokenHint:       settings.TokenHint,
		Path:            observability.MetricsPath,
		Persistent:      exporter.Persistent(),
		Token:           token,
	}
	if !settings.TokenCreatedAt.IsZero() {
		created := settings.TokenCreatedAt
		view.TokenCreatedAt = &created
	}
	if !settings.UpdatedAt.IsZero() {
		updated := settings.UpdatedAt
		view.UpdatedAt = &updated
	}
	return view
}

func (h *Handler) metricsExporter() *observability.Exporter {
	if h != nil && h.metrics != nil {
		return h.metrics
	}
	return observability.Default()
}

// GetObservabilityMetrics reports whether /metrics is on and which token it
// takes.
func (h *Handler) GetObservabilityMetrics(c *gin.Context) {
	c.JSON(http.StatusOK, newObservabilityMetricsView(h.metricsExporter(), ""))
}

type putObservabilityMetricsBody struct {
	Enabled *bool `json:"enabled"`
}

// PutObservabilityMetrics switches /metrics on or off. Switching it on the
// first time also generates the scrape token, returned in this response only.
func (h *Handler) PutObservabilityMetrics(c *gin.Context) {
	var body putObservabilityMetricsBody
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_request", "message": "body must be {\"enabled\": true|false}"}})
		return
	}
	exporter := h.metricsExporter()
	_, token, err := exporter.SetEnabled(*body.Enabled)
	if err != nil {
		respondObservabilityWriteError(c, err)
		return
	}
	writeObservabilityMetricsView(c, exporter, token)
}

// PostObservabilityMetricsToken replaces the scrape token. The old token stops
// working immediately; the new one is returned in this response only.
func (h *Handler) PostObservabilityMetricsToken(c *gin.Context) {
	exporter := h.metricsExporter()
	_, token, err := exporter.RotateToken()
	if err != nil {
		respondObservabilityWriteError(c, err)
		return
	}
	writeObservabilityMetricsView(c, exporter, token)
}

func writeObservabilityMetricsView(c *gin.Context, exporter *observability.Exporter, token string) {
	if token != "" {
		c.Header("Cache-Control", "no-store")
	}
	c.JSON(http.StatusOK, newObservabilityMetricsView(exporter, token))
}

func respondObservabilityWriteError(c *gin.Context, err error) {
	if errors.Is(err, observability.ErrNotPersistent) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "observability_unavailable", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "observability_save_failed", "message": err.Error()}})
}
