package management

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func (h *Handler) endUserService() *enduser.Service {
	if h == nil {
		return nil
	}
	if s := enduser.Default(); s != nil {
		return s
	}
	// Tests / late wiring: fall back to runtime DB when SetDefault was not called.
	db := usage.RuntimeDB()
	if db == nil {
		return nil
	}
	s := enduser.NewService(db)
	enduser.SetDefault(s)
	return s
}

func endUserError(c *gin.Context, err error) {
	// Password policy and cooldown are checked before the generic sentinels
	// below, because both satisfy errors.Is against those sentinels and would
	// otherwise collapse into a code the panel cannot translate.
	if writePasswordPolicyError(c, err) {
		return
	}
	switch {
	case errors.Is(err, enduser.ErrInvalidCredentials):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_credentials", "message": err.Error()}})
	case errors.Is(err, enduser.ErrAccountDisabled):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "account_disabled", "message": err.Error()}})
	case errors.Is(err, enduser.ErrAccountLocked):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "account_locked", "message": err.Error()}})
	case errors.Is(err, enduser.ErrLoginCooldowned):
		abortPortalCooldown(c, err)
	case errors.Is(err, enduser.ErrMustChangePassword):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "must_change_password", "message": err.Error()}})
	case errors.Is(err, enduser.ErrSessionExpired):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "session_expired", "message": err.Error()}})
	case errors.Is(err, enduser.ErrSessionRevoked):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "session_revoked", "message": err.Error()}})
	case errors.Is(err, enduser.ErrPermissionDenied), errors.Is(err, enduser.ErrTenantScope):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "permission_denied", "message": err.Error()}})
	case errors.Is(err, enduser.ErrTenantSuspended):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "tenant_suspended", "message": err.Error()}})
	case errors.Is(err, enduser.ErrTenantExpired):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "tenant_expired", "message": err.Error()}})
	case errors.Is(err, enduser.ErrLastKey):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": gin.H{"code": "last_key", "message": err.Error()}})
	case errors.Is(err, enduser.ErrDuplicateKeyName):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": gin.H{"code": "duplicate_key_name", "message": err.Error()}})
	case errors.Is(err, enduser.ErrNotFound), errors.Is(err, apikeysettings.ErrItemNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "not_found", "message": err.Error()}})
	case errors.Is(err, enduser.ErrPeriodDayLegacyConflict):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "period_day_legacy_conflict", "message": "daily-spending-limit conflicts with period-spending-limits.day"}})
	case func() bool { var target *quota.LimitExceedsAccountError; return errors.As(err, &target) }():
		var target *quota.LimitExceedsAccountError
		_ = errors.As(err, &target)
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"code": "key_period_limit_exceeds_account", "message": target.Error(),
			"details": gin.H{"period": target.Period, "key_limit": target.KeyLimit, "account_limit": target.AccountLimit},
		}})
	case errors.Is(err, enduser.ErrValidation):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "validation_failed", "message": err.Error()}})
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": err.Error()}})
	}
}

func (h *Handler) GetEndUsers(c *gin.Context) {
	principal, _ := principalFromContext(c)
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	items, err := svc.ListUsers(c.Request.Context(), principal, effectiveTenantID(c))
	if err != nil {
		endUserError(c, err)
		return
	}
	// Group by tenant and batch-load period/lifetime usage and reset counts.
	byTenant := map[string][]string{}
	for i := range items {
		byTenant[items[i].TenantID] = append(byTenant[items[i].TenantID], items[i].ID)
	}
	usageByID := map[string]quota.PeriodSpendingUsage{}
	resetCounts := map[string]int{}
	profilesByTenant := map[string]map[string]usage.APIKeyPermissionProfileRow{}
	for tenantID, ids := range byTenant {
		part, usageErr := usage.QueryPeriodSpendingByEndUsersForTenant(tenantID, ids)
		if usageErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": usageErr.Error()})
			return
		}
		for id, used := range part {
			usageByID[id] = used
		}
		countPart, usageErr := usage.ListEndUserDailySpendingResetEventCounts(tenantID, ids)
		if usageErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": usageErr.Error()})
			return
		}
		for id, count := range countPart {
			resetCounts[id] = count
		}
		profiles := make(map[string]usage.APIKeyPermissionProfileRow)
		for _, profile := range usage.ListAPIKeyPermissionProfilesForTenant(tenantID) {
			profiles[profile.ID] = profile
		}
		profilesByTenant[tenantID] = profiles
	}
	for i := range items {
		limits := items[i].PeriodSpendingLimits
		if profile, ok := profilesByTenant[items[i].TenantID][strings.TrimSpace(items[i].PermissionProfileID)]; ok {
			limits = profile.PeriodSpendingLimits
			items[i].DailySpendingLimit = profile.DailySpendingLimit
		}
		limits.Day = items[i].DailySpendingLimit
		items[i].PeriodSpendingLimits = limits
		used := usageByID[items[i].ID]
		items[i].DailySpendingUsed = used.Day
		items[i].LifetimeSpendingUsed = used.Lifetime
		items[i].PeriodSpending = quota.BuildPeriodSpending(limits, used)
		items[i].DailySpendingResetCount = resetCounts[items[i].ID]
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) PostEndUser(c *gin.Context) {
	principal, _ := principalFromContext(c)
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	result, err := svc.CreateUser(c.Request.Context(), principal, effectiveTenantID(c), body.Username, body.DisplayName, body.Password)
	if err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusCreated, result)
}

func (h *Handler) PatchEndUser(c *gin.Context) {
	principal, _ := principalFromContext(c)
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		Username             *string                          `json:"username"`
		DisplayName          *string                          `json:"display_name"`
		Password             *string                          `json:"password"`
		Status               *string                          `json:"status"`
		PermissionProfileID  *string                          `json:"permission-profile-id"`
		DailyLimit           *int                             `json:"daily-limit"`
		TotalQuota           *int                             `json:"total-quota"`
		SpendingLimit        *float64                         `json:"spending-limit"`
		DailySpendingLimit   *float64                         `json:"daily-spending-limit"`
		PeriodSpendingLimits *quota.PeriodSpendingLimitsPatch `json:"period-spending-limits"`
		ConcurrencyLimit     *int                             `json:"concurrency-limit"`
		RPMLimit             *int                             `json:"rpm-limit"`
		TPMLimit             *int                             `json:"tpm-limit"`
		AllowedModels        *[]string                        `json:"allowed-models"`
		AllowedChannels      *[]string                        `json:"allowed-channels"`
		AllowedChannelGroups *[]string                        `json:"allowed-channel-groups"`
		SystemPrompt         *string                          `json:"system-prompt"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	periodPatch, err := quota.ResolveLegacyDay(body.DailySpendingLimit, body.PeriodSpendingLimits)
	if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
		endUserError(c, enduser.ErrPeriodDayLegacyConflict)
		return
	}
	if err != nil {
		endUserError(c, fmt.Errorf("%w: %v", enduser.ErrValidation, err))
		return
	}
	quotaPatch := &enduser.QuotaPatch{
		PermissionProfileID:  body.PermissionProfileID,
		DailyLimit:           body.DailyLimit,
		TotalQuota:           body.TotalQuota,
		SpendingLimit:        body.SpendingLimit,
		DailySpendingLimit:   nil,
		PeriodSpendingLimits: periodPatch,
		ConcurrencyLimit:     body.ConcurrencyLimit,
		RPMLimit:             body.RPMLimit,
		TPMLimit:             body.TPMLimit,
		AllowedModels:        body.AllowedModels,
		AllowedChannels:      body.AllowedChannels,
		AllowedChannelGroups: body.AllowedChannelGroups,
		SystemPrompt:         body.SystemPrompt,
	}
	// Only pass quota when at least one field is set (avoid no-op patch noise).
	hasQuota := body.PermissionProfileID != nil || body.DailyLimit != nil || body.TotalQuota != nil ||
		body.SpendingLimit != nil || periodPatch != nil || body.ConcurrencyLimit != nil ||
		body.RPMLimit != nil || body.TPMLimit != nil || body.AllowedModels != nil ||
		body.AllowedChannels != nil || body.AllowedChannelGroups != nil || body.SystemPrompt != nil
	if !hasQuota {
		quotaPatch = nil
	}
	user, err := svc.UpdateUser(c.Request.Context(), principal, effectiveTenantID(c), c.Param("id"), body.Username, body.DisplayName, body.Password, body.Status, quotaPatch)
	if err != nil {
		endUserError(c, err)
		return
	}
	// Status or account quota changes affect auth metadata for owned keys.
	if body.Status != nil || hasQuota {
		if err := h.refreshAPIKeyCache(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
				"code":    "cache_refresh_failed",
				"message": err.Error(),
			}})
			return
		}
	}
	c.JSON(http.StatusOK, user)
}

func (h *Handler) DeleteEndUser(c *gin.Context) {
	principal, _ := principalFromContext(c)
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	if err := svc.DeleteUser(c.Request.Context(), principal, effectiveTenantID(c), c.Param("id")); err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) PostEndUserResetPassword(c *gin.Context) {
	principal, _ := principalFromContext(c)
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	_ = c.ShouldBindJSON(&body)
	generated, err := svc.ResetPassword(c.Request.Context(), principal, effectiveTenantID(c), c.Param("id"), body.Password)
	if err != nil {
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"generated_password": generated})
}

func (h *Handler) PostEndUserDailySpendingReset(c *gin.Context) {
	principal, _ := principalFromContext(c)
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	if !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	tenantID := effectiveTenantID(c)
	user, err := svc.GetUser(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		endUserError(c, err)
		return
	}
	accountQuota := usage.GetEndUserQuota(user.ID)
	if accountQuota == nil || usage.EffectiveEndUserQuota(*accountQuota).DailySpendingLimit <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "daily_spending_limit_missing", "message": "Account daily spending limit is unlimited"}})
		return
	}
	actor := periodResetActorFromContext(c)
	reset, err := usage.ResetPeriodSpendingByEndUserForTenant(tenantID, user.ID, []quota.Period{quota.PeriodDay}, usage.PeriodSpendingResetActor{
		UserID: actor.UserID, Username: actor.Username, Kind: actor.Kind,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	usedBefore := reset.EffectiveUsedBefore.Day
	rawToday := reset.Raw.Day
	if err := usage.UpsertEndUserDailySpendingReset(tenantID, user.ID, rawToday); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := usage.InsertEndUserDailySpendingResetEvent(usage.EndUserDailySpendingResetEvent{
		TenantID:            tenantID,
		EndUserID:           user.ID,
		CostBaseline:        rawToday,
		EffectiveUsedBefore: usedBefore,
		RawTodayCost:        rawToday,
		ActorUserID:         actor.UserID,
		ActorUsername:       actor.Username,
		ActorKind:           actor.Kind,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resetCount, err := usage.CountEndUserDailySpendingResetEvents(tenantID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":                     "ok",
		"end_user_id":                user.ID,
		"daily-spending-used":        0,
		"daily-spending-reset-count": resetCount,
		"effective-used-before":      usedBefore,
		"raw-today-cost":             rawToday,
	})
}

// GetEndUserDailySpendingResetHistory lists all-time manual reset events newest-first.
func (h *Handler) GetEndUserDailySpendingResetHistory(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("end_users.read") && !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
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
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, parseErr := strconv.Atoi(raw); parseErr == nil {
			limit = n
		}
	}
	events, err := usage.ListEndUserDailySpendingResetEvents(tenantID, user.ID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if events == nil {
		events = []usage.EndUserDailySpendingResetEvent{}
	}
	total, err := usage.CountEndUserDailySpendingResetEvents(tenantID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rawToday, err := usage.QueryRawTodayCostByEndUserForTenant(tenantID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	used, err := usage.QueryTodayEffectiveCostByEndUserForTenant(tenantID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items":               events,
		"total":               total,
		"raw-today-cost":      rawToday,
		"daily-spending-used": used,
	})
}

func (h *Handler) GetEndUserAPIKeys(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.read") && !principal.Has("end_users.read") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	items, err := svc.ListKeys(c.Request.Context(), effectiveTenantID(c), c.Param("id"))
	if err != nil {
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) PostEndUserAPIKey(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.write") && !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		Name                 string                           `json:"name"`
		DailySpendingLimit   *float64                         `json:"daily-spending-limit"`
		PeriodSpendingLimits *quota.PeriodSpendingLimitsPatch `json:"period-spending-limits"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	periodPatch, err := quota.ResolveLegacyDay(body.DailySpendingLimit, body.PeriodSpendingLimits)
	if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
		endUserError(c, enduser.ErrPeriodDayLegacyConflict)
		return
	}
	if err != nil {
		endUserError(c, fmt.Errorf("%w: %v", enduser.ErrValidation, err))
		return
	}
	result, err := svc.CreateKeyWithPeriodLimits(c.Request.Context(), effectiveTenantID(c), c.Param("id"), body.Name, periodPatch)
	if err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusCreated, result)
}

func (h *Handler) PatchEndUserAPIKey(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.write") && !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		Name                 *string                          `json:"name"`
		DailySpendingLimit   *float64                         `json:"daily-spending-limit"`
		PeriodSpendingLimits *quota.PeriodSpendingLimitsPatch `json:"period-spending-limits"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	periodPatch, err := quota.ResolveLegacyDay(body.DailySpendingLimit, body.PeriodSpendingLimits)
	if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
		endUserError(c, enduser.ErrPeriodDayLegacyConflict)
		return
	}
	if err != nil {
		endUserError(c, fmt.Errorf("%w: %v", enduser.ErrValidation, err))
		return
	}
	if body.Name == nil && periodPatch == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	tenantID := effectiveTenantID(c)
	userID := c.Param("id")
	keyID := c.Param("key_id")
	if err := svc.UpdateKey(c.Request.Context(), tenantID, userID, keyID, body.Name, periodPatch); err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	items, err := svc.ListKeys(c.Request.Context(), tenantID, userID)
	if err != nil {
		endUserError(c, err)
		return
	}
	for _, item := range items {
		if item.ID == keyID {
			c.JSON(http.StatusOK, item)
			return
		}
	}
	endUserError(c, enduser.ErrNotFound)
}

func (h *Handler) PostEndUserAPIKeyRotate(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.write") && !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	result, err := svc.RotateKey(c.Request.Context(), effectiveTenantID(c), c.Param("id"), c.Param("key_id"))
	if err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) DeleteEndUserAPIKey(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("end_users.write") && !principal.Has("api_keys.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	if err := svc.DeleteKey(c.Request.Context(), effectiveTenantID(c), c.Param("id"), c.Param("key_id")); err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) PostEndUserAPIKeyDefault(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("end_users.write") && !principal.Has("api_keys.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	if err := svc.SetDefaultKey(c.Request.Context(), effectiveTenantID(c), c.Param("id"), c.Param("key_id")); err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) GetPortalAPIKeys(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	items, err := h.endUserService().ListKeys(c.Request.Context(), user.TenantID, user.ID)
	if err != nil {
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// GetPortalAPIKeySecret returns the plaintext secret for an owned key (for usage lookup after login).
func (h *Handler) GetPortalAPIKeySecret(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	secret, err := h.endUserService().ResolveOwnedKeySecret(
		c.Request.Context(), user.TenantID, user.ID, c.Param("id"),
	)
	if err != nil {
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"key": secret, "id": c.Param("id")})
}

func (h *Handler) PostPortalAPIKey(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	var body struct {
		Name                 string                           `json:"name"`
		DailySpendingLimit   *float64                         `json:"daily-spending-limit"`
		PeriodSpendingLimits *quota.PeriodSpendingLimitsPatch `json:"period-spending-limits"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	periodPatch, err := quota.ResolveLegacyDay(body.DailySpendingLimit, body.PeriodSpendingLimits)
	if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
		endUserError(c, enduser.ErrPeriodDayLegacyConflict)
		return
	}
	if err != nil {
		endUserError(c, fmt.Errorf("%w: %v", enduser.ErrValidation, err))
		return
	}
	result, err := h.endUserService().CreateKeyWithPeriodLimits(c.Request.Context(), user.TenantID, user.ID, body.Name, periodPatch)
	if err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusCreated, result)
}

func (h *Handler) PatchPortalAPIKey(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	var body struct {
		Name                 *string                          `json:"name"`
		DailySpendingLimit   *float64                         `json:"daily-spending-limit"`
		PeriodSpendingLimits *quota.PeriodSpendingLimitsPatch `json:"period-spending-limits"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	periodPatch, err := quota.ResolveLegacyDay(body.DailySpendingLimit, body.PeriodSpendingLimits)
	if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
		endUserError(c, enduser.ErrPeriodDayLegacyConflict)
		return
	}
	if err != nil {
		endUserError(c, fmt.Errorf("%w: %v", enduser.ErrValidation, err))
		return
	}
	keyID := c.Param("id")
	svc := h.endUserService()
	if body.Name != nil || periodPatch != nil {
		if err := svc.UpdateKey(c.Request.Context(), user.TenantID, user.ID, keyID, body.Name, periodPatch); err != nil {
			endUserError(c, err)
			return
		}
	}
	items, err := svc.ListKeys(c.Request.Context(), user.TenantID, user.ID)
	if err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	for _, item := range items {
		if item.ID == keyID {
			c.JSON(http.StatusOK, item)
			return
		}
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) PostPortalAPIKeyRotate(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	result, err := h.endUserService().RotateKey(c.Request.Context(), user.TenantID, user.ID, c.Param("id"))
	if err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) DeletePortalAPIKey(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	if err := h.endUserService().DeleteKey(c.Request.Context(), user.TenantID, user.ID, c.Param("id")); err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) resetOwnedAPIKeyDailySpending(c *gin.Context, tenantID, endUserID, keyID string, actor apikeysettings.DailySpendingResetActor) {
	if _, err := h.endUserService().ResolveOwnedKeySecret(c.Request.Context(), tenantID, endUserID, keyID); err != nil {
		endUserError(c, err)
		return
	}
	result, err := h.apiKeySettingsForTenant(tenantID).ResetDailySpending(&keyID, nil, actor)
	if err != nil {
		if errors.Is(err, apikeysettings.ErrDailySpendingLimitMissing) {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "daily_spending_limit_missing", "message": "Key daily spending limit is unlimited"}})
			return
		}
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) ownedAPIKeyDailySpendingHistory(c *gin.Context, tenantID, endUserID, keyID string) {
	if _, err := h.endUserService().ResolveOwnedKeySecret(c.Request.Context(), tenantID, endUserID, keyID); err != nil {
		endUserError(c, err)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	events, err := h.apiKeySettingsForTenant(tenantID).ListDailySpendingResetHistory(&keyID, nil, limit)
	if err != nil {
		endUserError(c, err)
		return
	}
	total, err := usage.CountDailySpendingResetEvents(tenantID, keyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": events, "total": total})
}

func (h *Handler) PostEndUserAPIKeyDailySpendingReset(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.write") && !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	h.resetOwnedAPIKeyDailySpending(c, effectiveTenantID(c), c.Param("id"), c.Param("key_id"), apikeysettings.DailySpendingResetActor{
		UserID: principal.User.ID, Username: principal.User.Username, Kind: principal.Kind,
	})
}

func (h *Handler) GetEndUserAPIKeyDailySpendingResetHistory(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.read") && !principal.Has("end_users.read") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	h.ownedAPIKeyDailySpendingHistory(c, effectiveTenantID(c), c.Param("id"), c.Param("key_id"))
}
