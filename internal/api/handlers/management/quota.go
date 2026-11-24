package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// Quota exceeded toggles
func (h *Handler) GetSwitchProject(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingQuotaExceeded, gin.H{"switch-project": h.cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	h.jsonWithLiveVersion(c, runtimeconfig.RuntimeSettingQuotaExceeded, gin.H{"switch-preview-model": h.cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.QuotaExceeded.SwitchPreviewModel = v })
}

type quotaReconcileRequest struct {
	AuthIndexSnake  *string `json:"auth_index"`
	AuthIndexCamel  *string `json:"authIndex"`
	AuthIndexPascal *string `json:"AuthIndex"`
}

func (h *Handler) PostQuotaReconcile(c *gin.Context) {
	if h == nil || h.authManager == nil {
		writeServiceUnavailableError(c, "核心凭据管理服务")
		return
	}

	var body quotaReconcileRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := h.authByIndexForTenant(effectiveTenantID(c), authIndex)
	if auth == nil {
		writeNotFoundError(c, "账号凭据", authIndex)
		return
	}

	changed, err := h.authManager.ReconcileQuota(c.Request.Context(), auth.ID)
	if err != nil {
		writeUpstreamError(c, http.StatusBadGateway, "校准账号配额状态", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"changed": changed,
	})
}

func (h *Handler) PostQuotaClearStatus(c *gin.Context) {
	if h == nil || h.authManager == nil {
		writeServiceUnavailableError(c, "核心凭据管理服务")
		return
	}

	var body quotaReconcileRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := h.authByIndexForTenant(effectiveTenantID(c), authIndex)
	if auth == nil {
		writeNotFoundError(c, "账号凭据", authIndex)
		return
	}

	changed, err := h.authManager.ClearQuotaStatus(c.Request.Context(), auth.ID)
	if err != nil {
		writeInternalError(c, "清除账号配额受限状态", err)
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
		writeServiceUnavailableError(c, "配额快照服务")
		return
	}

	var body quotaSnapshotRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	if strings.TrimSpace(authIndex) == "" {
		writeMissingFieldError(c, "auth_index", "请指定要写入配额快照的账号凭据编号（auth_index）")
		return
	}
	quotaPoints := body.QuotaPoints
	if len(quotaPoints) == 0 && len(body.QuotaPointsCamel) > 0 {
		quotaPoints = body.QuotaPointsCamel
	}
	if len(body.Quotas) == 0 && len(quotaPoints) == 0 {
		writeMissingFieldError(c, "quotas", "请至少提供 quotas 或 quota_points 之一")
		return
	}

	provider := strings.TrimSpace(body.Provider)
	authSubjectID := ""
	if h.authManager != nil {
		auth := h.authByIndexForTenant(effectiveTenantID(c), authIndex)
		if auth == nil {
			writeNotFoundError(c, "账号凭据", authIndex)
			return
		}
		if provider == "" {
			provider = strings.TrimSpace(auth.Provider)
		}
		if identity := usage.ResolveAuthSubjectIdentity(auth); identity != nil {
			authSubjectID = identity.ID
		}
	}

	if len(body.Quotas) > 0 {
		if err := usage.RecordDailyQuotaSnapshotIdentityForTenant(effectiveTenantID(c), authIndex, authSubjectID, provider, body.Quotas); err != nil {
			writeInternalError(c, "保存每日配额快照", err)
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
		if err := usage.RecordQuotaSnapshotPointsIdentityForTenant(effectiveTenantID(c), authIndex, authSubjectID, provider, points); err != nil {
			writeInternalError(c, "保存配额时间点快照", err)
			return
		}
	}
	// Targeted invalidation only: never wipe dashboard/entity-stats caches.
	h.invalidateAIAccountCaches(effectiveTenantID(c), authIndex, authSubjectID)
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
