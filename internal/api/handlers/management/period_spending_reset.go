package management

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type periodSpendingResetRequest struct {
	Periods []quota.Period `json:"periods"`
}

func bindPeriodSpendingResetRequest(c *gin.Context) ([]quota.Period, bool) {
	var body periodSpendingResetRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_body", "message": "Request body must be valid JSON"}})
		return nil, false
	}
	periods, err := quota.NormalizePeriods(body.Periods)
	if err == nil {
		return periods, true
	}
	var invalid *quota.InvalidPeriodError
	switch {
	case errors.Is(err, quota.ErrPeriodsRequired):
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "periods_required", "message": "periods must be a non-empty array"}})
	case errors.As(err, &invalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_period", "message": fmt.Sprintf("Invalid period %q; allowed periods are 5h, day, week, month", invalid.Period)}})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_period", "message": err.Error()}})
	}
	return nil, false
}

func periodResetActorFromContext(c *gin.Context) apikeysettings.DailySpendingResetActor {
	actor := apikeysettings.DailySpendingResetActor{Kind: "service_credential"}
	if principal, ok := principalFromContext(c); ok {
		actor.Kind = strings.TrimSpace(principal.Kind)
		if actor.Kind == "" {
			actor.Kind = "service_credential"
		}
		actor.UserID = strings.TrimSpace(principal.User.ID)
		actor.Username = strings.TrimSpace(principal.User.Username)
		if actor.Username == "" {
			actor.Username = strings.TrimSpace(principal.User.DisplayName)
		}
	}
	return actor
}

func writeKeyPeriodResetError(c *gin.Context, err error, scope string) bool {
	var missing *apikeysettings.PeriodLimitMissingError
	if errors.As(err, &missing) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"code":    "period_limit_missing",
			"message": fmt.Sprintf("%s %s spending limit is not configured", scope, missing.Period),
			"period":  missing.Period,
		}})
		return true
	}
	return false
}

func (h *Handler) PostEndUserPeriodSpendingReset(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	periods, ok := bindPeriodSpendingResetRequest(c)
	if !ok {
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	tenantID := effectiveTenantID(c)
	user, err := svc.GetUser(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		endUserError(c, err)
		return
	}
	accountQuota := usage.GetEndUserQuota(user.ID)
	if accountQuota == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "period_limit_missing", "message": "Account period spending limits are not configured"}})
		return
	}
	effective := usage.EffectiveEndUserQuota(*accountQuota)
	effective.PeriodSpendingLimits.Day = effective.DailySpendingLimit
	matrix := effective.QuotaMatrix()
	for _, period := range periods {
		// A window can be reset when anything limits it: a spending, request or
		// token cell of the account matrix, or a per-model rule. The reset clears
		// all of them for that window at once.
		if !quota.WindowHasLimit(matrix, effective.AccessPolicy.ModelRules, period) {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
				"code": "period_limit_missing", "message": fmt.Sprintf("Account %s limit is not configured", period), "period": period,
			}})
			return
		}
	}
	actor := periodResetActorFromContext(c)
	reset, err := usage.ResetPeriodSpendingByEndUserForTenant(tenantID, user.ID, periods, usage.PeriodSpendingResetActor{
		UserID: actor.UserID, Username: actor.Username, Kind: actor.Kind,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	count, err := usage.CountEndUserPeriodSpendingResetEvents(tenantID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "periods": reset.Periods,
		"period-spending": quota.BuildPeriodSpending(effective.PeriodSpendingLimits, reset.EffectiveAfter),
		"reset-count":     count, "effective-used-before": reset.EffectiveUsedBefore, "raw": reset.Raw,
	})
}

func (h *Handler) resetOwnedAPIKeyPeriodSpending(c *gin.Context, tenantID, endUserID, keyID string, periods []quota.Period, actor apikeysettings.DailySpendingResetActor) {
	if _, err := h.endUserService().ResolveOwnedKeySecret(c.Request.Context(), tenantID, endUserID, keyID); err != nil {
		endUserError(c, err)
		return
	}
	result, err := h.apiKeySettingsForTenant(tenantID).ResetPeriodSpending(&keyID, nil, periods, actor)
	if err != nil {
		if writeKeyPeriodResetError(c, err, "Key") {
			return
		}
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "periods": result.Periods, "period-spending": result.PeriodSpending,
		"reset-count": result.ResetCount, "effective-used-before": result.EffectiveUsedBefore, "raw": result.Raw,
	})
}

func (h *Handler) PostEndUserAPIKeyPeriodSpendingReset(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.write") && !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	periods, ok := bindPeriodSpendingResetRequest(c)
	if !ok {
		return
	}
	h.resetOwnedAPIKeyPeriodSpending(c, effectiveTenantID(c), c.Param("id"), c.Param("key_id"), periods, periodResetActorFromContext(c))
}

func (h *Handler) PostPortalAPIKeyPeriodSpendingReset(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	periods, ok := bindPeriodSpendingResetRequest(c)
	if !ok {
		return
	}
	h.resetOwnedAPIKeyPeriodSpending(c, user.TenantID, user.ID, c.Param("id"), periods, apikeysettings.DailySpendingResetActor{
		UserID: user.ID, Username: user.Username, Kind: "end_user",
	})
}
