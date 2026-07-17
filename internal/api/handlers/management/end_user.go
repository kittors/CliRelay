package management

import (
	"errors"
	"net/http"
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
		Username    *string `json:"username"`
		DisplayName *string `json:"display_name"`
		Password    *string `json:"password"`
		Status      *string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	user, err := svc.UpdateUser(c.Request.Context(), principal, effectiveTenantID(c), c.Param("id"), body.Username, body.DisplayName, body.Password, body.Status)
	if err != nil {
		endUserError(c, err)
		return
	}
	if body.Status != nil {
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

func (h *Handler) GetEndUserAPIKeys(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("end_users.read") && !principal.PlatformAdmin {
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
	if !principal.Has("end_users.write") && !principal.PlatformAdmin {
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
		Name      *string `json:"name"`
		IsDefault *bool   `json:"is_default"`
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
	if body.IsDefault != nil && *body.IsDefault {
		if err := svc.SetDefaultKey(c.Request.Context(), user.TenantID, user.ID, keyID); err != nil {
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
