package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// Quota exceeded toggles
func (h *Handler) GetSwitchProject(c *gin.Context) {
	c.JSON(200, gin.H{"switch-project": h.cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	c.JSON(200, gin.H{"switch-preview-model": h.cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchPreviewModel = v })
}

type quotaReconcileRequest struct {
	AuthIndexSnake  *string `json:"auth_index"`
	AuthIndexCamel  *string `json:"authIndex"`
	AuthIndexPascal *string `json:"AuthIndex"`
}

func (h *Handler) PostQuotaReconcile(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}

	var body quotaReconcileRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	changed, err := h.authManager.ReconcileQuota(c.Request.Context(), auth.ID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"changed": changed,
	})
}

type quotaSnapshotRequest struct {
	AuthIndexSnake   *string                     `json:"auth_index"`
	AuthIndexCamel   *string                     `json:"authIndex"`
	AuthIndexPascal  *string                     `json:"AuthIndex"`
	Provider         string                      `json:"provider"`
	Quotas           map[string]*float64         `json:"quotas"`
	QuotaPoints      []quotaSnapshotPointRequest `json:"quota_points"`
	QuotaPointsCamel []quotaSnapshotPointRequest `json:"quotaPoints"`
}

type quotaSnapshotPointRequest struct {
	RecordedAtSnake    *time.Time `json:"recorded_at"`
	RecordedAtCamel    *time.Time `json:"recordedAt"`
	QuotaKeySnake      string     `json:"quota_key"`
	QuotaKeyCamel      string     `json:"quotaKey"`
	QuotaLabelSnake    string     `json:"quota_label"`
	QuotaLabelCamel    string     `json:"quotaLabel"`
	Percent            *float64   `json:"percent"`
	ResetAtSnake       *time.Time `json:"reset_at"`
	ResetAtCamel       *time.Time `json:"resetAt"`
	WindowSecondsSnake int64      `json:"window_seconds"`
	WindowSecondsCamel int64      `json:"windowSeconds"`
}

func (h *Handler) PostAuthFileQuotaSnapshot(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "handler unavailable"})
		return
	}

	var body quotaSnapshotRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	if strings.TrimSpace(authIndex) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	quotaPoints := body.QuotaPoints
	if len(quotaPoints) == 0 && len(body.QuotaPointsCamel) > 0 {
		quotaPoints = body.QuotaPointsCamel
	}
	if len(body.Quotas) == 0 && len(quotaPoints) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quotas or quota_points is required"})
		return
	}

	provider := strings.TrimSpace(body.Provider)
	if h.authManager != nil {
		auth := h.authByIndex(authIndex)
		if auth == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
			return
		}
		if provider == "" {
			provider = strings.TrimSpace(auth.Provider)
		}
	}

	if len(body.Quotas) > 0 {
		if err := usage.RecordDailyQuotaSnapshot(authIndex, provider, body.Quotas); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	if len(quotaPoints) > 0 {
		points := make([]usage.QuotaSnapshotPoint, 0, len(quotaPoints))
		for _, point := range quotaPoints {
			quotaKey := strings.TrimSpace(firstNonEmptyValue(point.QuotaKeySnake, point.QuotaKeyCamel))
			if quotaKey == "" {
				continue
			}
			recordedAt := time.Time{}
			if point.RecordedAtSnake != nil {
				recordedAt = point.RecordedAtSnake.UTC()
			} else if point.RecordedAtCamel != nil {
				recordedAt = point.RecordedAtCamel.UTC()
			}
			var resetAt *time.Time
			if point.ResetAtSnake != nil {
				value := point.ResetAtSnake.UTC()
				resetAt = &value
			} else if point.ResetAtCamel != nil {
				value := point.ResetAtCamel.UTC()
				resetAt = &value
			}
			windowSeconds := point.WindowSecondsSnake
			if windowSeconds == 0 {
				windowSeconds = point.WindowSecondsCamel
			}
			points = append(points, usage.QuotaSnapshotPoint{
				RecordedAt:    recordedAt,
				QuotaKey:      quotaKey,
				QuotaLabel:    firstNonEmptyValue(point.QuotaLabelSnake, point.QuotaLabelCamel),
				Percent:       point.Percent,
				ResetAt:       resetAt,
				WindowSeconds: windowSeconds,
			})
		}
		if err := usage.RecordQuotaSnapshotPoints(authIndex, provider, points); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	h.clearTrendCache()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func firstNonEmptyValue(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
