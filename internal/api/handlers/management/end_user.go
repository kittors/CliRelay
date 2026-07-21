package management

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
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
	switch {
	case errors.Is(err, enduser.ErrInvalidCredentials):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_credentials", "message": err.Error()}})
	case errors.Is(err, enduser.ErrAccountDisabled):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "account_disabled", "message": err.Error()}})
	case errors.Is(err, enduser.ErrAccountLocked):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "account_locked", "message": err.Error()}})
	case errors.Is(err, enduser.ErrLoginCooldowned):
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "login_cooldown", "message": err.Error()}})
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
	case errors.Is(err, enduser.ErrNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "not_found", "message": err.Error()}})
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
	// Group by tenant then batch-load today costs (avoids per-row N+1).
	byTenant := map[string][]string{}
	for i := range items {
		tid := items[i].TenantID
		byTenant[tid] = append(byTenant[tid], items[i].ID)
	}
	costs := map[string]float64{}
	resetCounts := map[string]int{}
	for tid, ids := range byTenant {
		part, usageErr := usage.QueryTodayEffectiveCostsByEndUsersForTenant(tid, ids)
		if usageErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": usageErr.Error()})
			return
		}
		for id, used := range part {
			costs[id] = used
		}
		countPart, usageErr := usage.ListEndUserDailySpendingResetEventCounts(tid, ids)
		if usageErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": usageErr.Error()})
			return
		}
		for id, count := range countPart {
			resetCounts[id] = count
		}
	}
	for i := range items {
		items[i].DailySpendingUsed = costs[items[i].ID]
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
		Username             *string   `json:"username"`
		DisplayName          *string   `json:"display_name"`
		Password             *string   `json:"password"`
		Status               *string   `json:"status"`
		PermissionProfileID  *string   `json:"permission-profile-id"`
		DailyLimit           *int      `json:"daily-limit"`
		TotalQuota           *int      `json:"total-quota"`
		SpendingLimit        *float64  `json:"spending-limit"`
		DailySpendingLimit   *float64  `json:"daily-spending-limit"`
		ConcurrencyLimit     *int      `json:"concurrency-limit"`
		RPMLimit             *int      `json:"rpm-limit"`
		TPMLimit             *int      `json:"tpm-limit"`
		AllowedModels        *[]string `json:"allowed-models"`
		AllowedChannels      *[]string `json:"allowed-channels"`
		AllowedChannelGroups *[]string `json:"allowed-channel-groups"`
		SystemPrompt         *string   `json:"system-prompt"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	quota := &enduser.QuotaPatch{
		PermissionProfileID:  body.PermissionProfileID,
		DailyLimit:           body.DailyLimit,
		TotalQuota:           body.TotalQuota,
		SpendingLimit:        body.SpendingLimit,
		DailySpendingLimit:   body.DailySpendingLimit,
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
		body.SpendingLimit != nil || body.DailySpendingLimit != nil || body.ConcurrencyLimit != nil ||
		body.RPMLimit != nil || body.TPMLimit != nil || body.AllowedModels != nil ||
		body.AllowedChannels != nil || body.AllowedChannelGroups != nil || body.SystemPrompt != nil
	if !hasQuota {
		quota = nil
	}
	user, err := svc.UpdateUser(c.Request.Context(), principal, effectiveTenantID(c), c.Param("id"), body.Username, body.DisplayName, body.Password, body.Status, quota)
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
	usedBefore, rawToday, err := usage.ResetTodayCostByEndUser(tenantID, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actorKind := "service_credential"
	actorUserID := ""
	actorUsername := ""
	if kind := strings.TrimSpace(principal.Kind); kind != "" {
		actorKind = kind
	}
	actorUserID = strings.TrimSpace(principal.User.ID)
	actorUsername = strings.TrimSpace(principal.User.Username)
	if actorUsername == "" {
		actorUsername = strings.TrimSpace(principal.User.DisplayName)
	}
	if err := usage.InsertEndUserDailySpendingResetEvent(usage.EndUserDailySpendingResetEvent{
		TenantID:            tenantID,
		EndUserID:           user.ID,
		CostBaseline:        rawToday,
		EffectiveUsedBefore: usedBefore,
		RawTodayCost:        rawToday,
		ActorUserID:         actorUserID,
		ActorUsername:       actorUsername,
		ActorKind:           actorKind,
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
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&body)
	result, err := svc.CreateKey(c.Request.Context(), effectiveTenantID(c), c.Param("id"), body.Name)
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
		Name *string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Name == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	tenantID := effectiveTenantID(c)
	userID := c.Param("id")
	keyID := c.Param("key_id")
	if err := svc.UpdateKeyName(c.Request.Context(), tenantID, userID, keyID, *body.Name); err != nil {
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

// Portal auth + key management

const portalPrincipalKey = "portalEndUser"
const portalSessionKey = "portalSessionID"

func (h *Handler) PostPortalLogin(c *gin.Context) {
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Username) == "" || body.Password == "" {
		endUserError(c, enduser.ErrInvalidCredentials)
		return
	}
	result, err := svc.Login(c.Request.Context(), body.Username, body.Password, c.GetHeader("User-Agent"))
	if err != nil {
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) PostPortalRefresh(c *gin.Context) {
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.RefreshToken) == "" {
		endUserError(c, enduser.ErrSessionRevoked)
		return
	}
	result, err := svc.Refresh(c.Request.Context(), body.RefreshToken, c.GetHeader("User-Agent"))
	if err != nil {
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) authenticatePortal(c *gin.Context) (enduser.User, string, bool) {
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return enduser.User{}, "", false
	}
	token := bearerToken(c)
	user, sessionID, err := svc.Authenticate(c.Request.Context(), token)
	if err != nil {
		endUserError(c, err)
		return enduser.User{}, "", false
	}
	c.Set(portalPrincipalKey, user)
	c.Set(portalSessionKey, sessionID)
	return user, sessionID, true
}

// portalKeyAccess requires an active portal session that is allowed to manage keys.
func (h *Handler) portalKeyAccess(c *gin.Context) (enduser.User, bool) {
	user, _, ok := h.authenticatePortal(c)
	if !ok {
		return enduser.User{}, false
	}
	if user.MustChangePassword {
		endUserError(c, enduser.ErrMustChangePassword)
		return enduser.User{}, false
	}
	return user, true
}

func (h *Handler) PostPortalLogout(c *gin.Context) {
	_, sessionID, ok := h.authenticatePortal(c)
	if !ok {
		return
	}
	_ = h.endUserService().Logout(c.Request.Context(), sessionID)
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetPortalMe(c *gin.Context) {
	user, _, ok := h.authenticatePortal(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": user})
}

func (h *Handler) PutPortalPassword(c *gin.Context) {
	user, sessionID, ok := h.authenticatePortal(c)
	if !ok {
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := h.endUserService().ChangePassword(c.Request.Context(), user, sessionID, body.CurrentPassword, body.NewPassword); err != nil {
		endUserError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
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
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&body)
	result, err := h.endUserService().CreateKey(c.Request.Context(), user.TenantID, user.ID, body.Name)
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
		Name *string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	keyID := c.Param("id")
	svc := h.endUserService()
	if body.Name != nil {
		if err := svc.UpdateKeyName(c.Request.Context(), user.TenantID, user.ID, keyID, *body.Name); err != nil {
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
