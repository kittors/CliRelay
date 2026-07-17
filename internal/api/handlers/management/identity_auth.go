package management

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

const managementPrincipalKey = "managementPrincipal"

func (h *Handler) identity() *identity.Service {
	if h == nil {
		return nil
	}
	if h.identityService != nil {
		return h.identityService
	}
	return identity.Default()
}

func bearerToken(c *gin.Context) string {
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	parts := strings.SplitN(header, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func principalFromContext(c *gin.Context) (identity.Principal, bool) {
	value, ok := c.Get(managementPrincipalKey)
	if !ok {
		return identity.Principal{}, false
	}
	principal, ok := value.(identity.Principal)
	return principal, ok
}

func (h *Handler) nextWithManagementAudit(c *gin.Context) {
	if c != nil && c.Request != nil && isTenantGovernancePath(c.Request.URL.Path) {
		if principal, ok := principalFromContext(c); ok && principal.Kind == "service_credential" {
			h.recordManagementAudit(c, principal, "denied")
			identityError(c, identity.ErrPermissionDenied)
			return
		}
		if h.identity() == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "identity_unavailable", "message": "identity service unavailable"}})
			return
		}
	}
	c.Next()
	principal, ok := principalFromContext(c)
	if ok {
		h.recordManagementAudit(c, principal, "")
	}
}

func (h *Handler) recordManagementAudit(c *gin.Context, principal identity.Principal, forcedResult string) {
	service := h.identity()
	if service == nil || c == nil || c.Request == nil {
		return
	}
	relative := strings.TrimPrefix(c.Request.URL.Path, "/v0/management/")
	parts := strings.Split(strings.Trim(relative, "/"), "/")
	resourceType := "management"
	resourceID := ""
	if len(parts) > 0 && parts[0] != "" {
		resourceType = parts[0]
	}
	if len(parts) > 1 {
		resourceID = strings.Join(parts[1:], "/")
	}
	write := c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead
	sensitiveRead := strings.Contains(relative, "/content") || strings.Contains(relative, "/egress") ||
		strings.Contains(relative, "/export") || strings.Contains(relative, "/download")
	result := forcedResult
	if result == "" {
		switch status := c.Writer.Status(); {
		case status < http.StatusBadRequest:
			result = "success"
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			result = "denied"
		default:
			result = "failed"
		}
	}
	if result == "success" && isTenantGovernancePath(c.Request.URL.Path) {
		return
	}
	if result == "success" && !write && !sensitiveRead {
		return
	}
	// Prefer middleware-assigned ID; fall back to generating one so audit rows stay correlatable
	// even if request-id middleware skipped this path (e.g. older deployments / test routers).
	requestID := logging.GetGinRequestID(c)
	if requestID == "" {
		requestID = logging.GetRequestID(c.Request.Context())
	}
	if requestID == "" {
		requestID = logging.GenerateRequestID()
		logging.SetGinRequestID(c, requestID)
		if c.Request != nil {
			c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), requestID))
		}
	}
	routePath := strings.TrimPrefix(c.Request.URL.Path, "/v0/management")
	handlerName := c.Request.Method + " " + routePath
	permission := permissionForManagementRequest(c.Request.Method, c.Request.URL.Path)
	service.RecordAudit(c.Request.Context(), identity.AuditEvent{
		TenantID:       principal.EffectiveTenant.ID,
		ActorKind:      principal.Kind,
		ActorUserID:    principal.User.ID,
		ActorSessionID: principal.SessionID,
		Action:         "management." + strings.ToLower(c.Request.Method),
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		Result:         result,
		RequestID:      requestID,
		Changes: map[string]any{
			"http": map[string]any{
				"method": c.Request.Method,
				"path":   c.Request.URL.Path,
				"status": c.Writer.Status(),
			},
			"permission": permission,
			// call_chain reconstructs the management request path for audit detail UI.
			"call_chain": []map[string]any{
				{"step": 1, "layer": "http", "name": c.Request.Method + " " + c.Request.URL.Path, "detail": "client request"},
				{"step": 2, "layer": "middleware", "name": "management.auth", "detail": "session + RBAC"},
				{"step": 3, "layer": "handler", "name": handlerName, "package": "internal/api/handlers/management"},
				{"step": 4, "layer": "service", "name": "identity/management service", "resource": resourceType, "resource_id": resourceID},
			},
			"project_method": map[string]any{
				"package":  "internal/api/handlers/management",
				"handler":  handlerName,
				"resource": resourceType,
				"route":    routePath,
			},
		},
	})
}

func identityError(c *gin.Context, err error) {
	status := http.StatusUnauthorized
	code := "invalid_credentials"
	switch {
	case errors.Is(err, identity.ErrAccountDisabled):
		code = "account_disabled"
	case errors.Is(err, identity.ErrAccountLocked):
		code = "account_locked"
	case errors.Is(err, identity.ErrTenantSuspended):
		status, code = http.StatusForbidden, "tenant_suspended"
	case errors.Is(err, identity.ErrTenantExpired):
		status, code = http.StatusForbidden, "tenant_expired"
	case errors.Is(err, identity.ErrSessionExpired):
		code = "session_expired"
	case errors.Is(err, identity.ErrSessionRevoked):
		code = "session_revoked"
	case errors.Is(err, identity.ErrPermissionDenied):
		status, code = http.StatusForbidden, "permission_denied"
	case errors.Is(err, identity.ErrTenantScope):
		status, code = http.StatusForbidden, "tenant_scope_forbidden"
	case errors.Is(err, identity.ErrVersionConflict):
		status, code = http.StatusConflict, "version_conflict"
	case errors.Is(err, identity.ErrProtectedResource):
		status, code = http.StatusConflict, "protected_resource"
	case errors.Is(err, identity.ErrValidation):
		status, code = http.StatusBadRequest, "validation_failed"
	case errors.Is(err, sql.ErrNoRows):
		status, code = http.StatusNotFound, "not_found"
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": err.Error()}})
}

func (h *Handler) PostLogin(c *gin.Context) {
	clientIP := c.ClientIP()
	now := time.Now()
	h.attemptsMu.Lock()
	attempt := h.failedAttempts[clientIP]
	if attempt != nil && !attempt.blockedUntil.IsZero() && now.Before(attempt.blockedUntil) {
		remaining := time.Until(attempt.blockedUntil).Round(time.Second)
		h.attemptsMu.Unlock()
		c.Header("Retry-After", retryAfterSecondsHeader(remaining))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "login_rate_limited", "message": "too many login attempts"}})
		return
	}
	h.attemptsMu.Unlock()

	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		RememberMe bool   `json:"remember_me"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Username) == "" || body.Password == "" {
		identityError(c, identity.ErrInvalidCredentials)
		return
	}
	service := h.identity()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "identity_unavailable", "message": "identity service unavailable"}})
		return
	}
	result, err := service.Login(c.Request.Context(), body.Username, body.Password, body.RememberMe, c.GetHeader("User-Agent"))
	if err != nil {
		h.attemptsMu.Lock()
		attempt = h.failedAttempts[clientIP]
		if attempt == nil {
			attempt = &attemptInfo{}
			h.failedAttempts[clientIP] = attempt
		}
		attempt.count++
		attempt.lastActivity = now
		if attempt.count >= 5 {
			attempt.blockedUntil = now.Add(30 * time.Minute)
			attempt.count = 0
		}
		h.attemptsMu.Unlock()
		identityError(c, err)
		return
	}
	h.attemptsMu.Lock()
	delete(h.failedAttempts, clientIP)
	h.attemptsMu.Unlock()
	c.JSON(http.StatusOK, result)
}

func (h *Handler) PostRefresh(c *gin.Context) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.RefreshToken) == "" {
		identityError(c, identity.ErrSessionRevoked)
		return
	}
	service := h.identity()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "identity_unavailable", "message": "identity service unavailable"}})
		return
	}
	result, err := service.RefreshSession(c.Request.Context(), body.RefreshToken)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) authenticateUserRequest(c *gin.Context) (identity.Principal, bool) {
	token := bearerToken(c)
	if !strings.HasPrefix(token, "cps_") {
		identityError(c, identity.ErrSessionRevoked)
		return identity.Principal{}, false
	}
	principal, err := h.identity().Authenticate(c.Request.Context(), token, c.GetHeader("X-Effective-Tenant-ID"))
	if err != nil {
		identityError(c, err)
		return identity.Principal{}, false
	}
	c.Set(managementPrincipalKey, principal)
	return principal, true
}

func (h *Handler) GetMe(c *gin.Context) {
	principal, ok := h.authenticateUserRequest(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"principal": principal})
}

func (h *Handler) PostLogout(c *gin.Context) {
	principal, ok := h.authenticateUserRequest(c)
	if !ok {
		return
	}
	if err := h.identity().Logout(c.Request.Context(), principal.SessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "logout_failed", "message": err.Error()}})
		return
	}
	h.identity().RecordAudit(c.Request.Context(), identity.AuditEvent{
		TenantID:       principal.HomeTenant.ID,
		ActorKind:      principal.Kind,
		ActorUserID:    principal.User.ID,
		ActorSessionID: principal.SessionID,
		Action:         "auth.logout",
		ResourceType:   "session",
		ResourceID:     principal.SessionID,
		Result:         "success",
	})
	c.Status(http.StatusNoContent)
}

func (h *Handler) PutPassword(c *gin.Context) {
	principal, ok := h.authenticateUserRequest(c)
	if !ok {
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_body", "message": "invalid body"}})
		return
	}
	if err := h.identity().ChangePassword(c.Request.Context(), principal, body.CurrentPassword, body.NewPassword); err != nil {
		identityError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetTenants(c *gin.Context) {
	items, err := h.identity().ListTenants(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) PostTenant(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		Name             string    `json:"name"`
		Description      string    `json:"description"`
		ExpiresAt        time.Time `json:"expires_at"`
		AdminUsername    string    `json:"admin_username"`
		AdminDisplayName string    `json:"admin_display_name"`
		AdminPassword    string    `json:"admin_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tenant, admin, err := h.identity().CreateTenant(c.Request.Context(), principal, identity.CreateTenantInput{
		Name: body.Name, Description: body.Description, ExpiresAt: body.ExpiresAt,
		AdminUsername: body.AdminUsername, AdminDisplayName: body.AdminDisplayName, AdminPassword: body.AdminPassword,
	})
	if err != nil {
		identityError(c, err)
		return
	}
	if h.authManager != nil && h.cfg != nil {
		tenantCfg := usage.BuildTenantRuntimeConfig(h.cfg, tenant.ID)
		h.authManager.SetConfigForTenant(tenant.ID, &tenantCfg)
	}
	c.JSON(http.StatusCreated, gin.H{"tenant": tenant, "admin": admin})
}

func (h *Handler) PatchTenant(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		Name                   *string    `json:"name"`
		Description            *string    `json:"description"`
		Status                 string     `json:"status"`
		ExpiresAt              *time.Time `json:"expires_at"`
		AccessTokenTTLSeconds  *int       `json:"access_token_ttl_seconds"`
		RefreshTokenTTLSeconds *int       `json:"refresh_token_ttl_seconds"`
		Version                int64      `json:"version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tenant, err := h.identity().UpdateTenantDetails(c.Request.Context(), principal, c.Param("id"), body.Name, body.Description, body.Status, body.ExpiresAt, body.AccessTokenTTLSeconds, body.RefreshTokenTTLSeconds, body.Version)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, tenant)
}

func (h *Handler) GetUsers(c *gin.Context) {
	principal, _ := principalFromContext(c)
	items, err := h.identity().ListUsers(c.Request.Context(), principal.EffectiveTenant.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) PostUser(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		Username, DisplayName, Password string
		RoleIDs                         []string `json:"role_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	user, err := h.identity().CreateUser(c.Request.Context(), principal, principal.EffectiveTenant.ID, body.Username, body.DisplayName, body.Password, body.RoleIDs)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusCreated, user)
}

func (h *Handler) PostUserResetPassword(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.identity().ResetPassword(c.Request.Context(), principal, principal.EffectiveTenant.ID, c.Param("id"), body.Password); err != nil {
		identityError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetRoles(c *gin.Context) {
	principal, _ := principalFromContext(c)
	items, err := h.identity().ListRoles(c.Request.Context(), principal.EffectiveTenant.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) GetMenus(c *gin.Context) {
	items, err := h.identity().ListMenus(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) PostMenu(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body identity.MenuInput
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	menu, err := h.identity().CreateMenu(c.Request.Context(), principal, body)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, menu)
}

func (h *Handler) PatchMenu(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body identity.MenuInput
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	menu, err := h.identity().UpdateMenu(c.Request.Context(), principal, c.Param("code"), body)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, menu)
}

func (h *Handler) DeleteMenu(c *gin.Context) {
	principal, _ := principalFromContext(c)
	version, err := strconv.ParseInt(strings.TrimSpace(c.Query("version")), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "version required"})
		return
	}
	if err := h.identity().DeleteMenu(c.Request.Context(), principal, c.Param("code"), version); err != nil {
		identityError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetPermissions(c *gin.Context) {
	items, err := h.identity().ListPermissions(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) PostRole(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Permissions []string `json:"permissions"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	role, err := h.identity().CreateRole(c.Request.Context(), principal, principal.EffectiveTenant.ID, body.Name, body.Description, body.Permissions)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusCreated, role)
}

func (h *Handler) PutRolePermissions(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		Permissions []string `json:"permissions"`
		Version     int64    `json:"version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	role, err := h.identity().ReplaceRolePermissions(c.Request.Context(), principal, principal.EffectiveTenant.ID, c.Param("id"), body.Permissions, body.Version)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, role)
}

func (h *Handler) setServicePrincipal(c *gin.Context) {
	systemTenant := identity.Tenant{ID: identity.SystemTenantID, Name: "System", Slug: "system", Status: "active"}
	principal := identity.Principal{
		Kind:            "service_credential",
		PlatformAdmin:   true,
		Permissions:     map[string]bool{},
		PermissionList:  []string{"*"},
		User:            identity.User{ID: identity.SystemUserID, TenantID: identity.SystemTenantID, Username: "admin", DisplayName: "Administrator", Status: "active"},
		HomeTenant:      systemTenant,
		EffectiveTenant: systemTenant,
	}
	if service := h.identity(); service != nil {
		if tenant, err := service.GetTenant(c.Request.Context(), identity.SystemTenantID); err == nil {
			principal.HomeTenant = tenant
			principal.EffectiveTenant = tenant
		}
	}
	c.Set(managementPrincipalKey, principal)
}

func isTenantGovernancePath(path string) bool {
	relative := strings.TrimPrefix(path, "/v0/management")
	return relative == "/tenants" || strings.HasPrefix(relative, "/tenants/") ||
		relative == "/users" || strings.HasPrefix(relative, "/users/") ||
		relative == "/roles" || strings.HasPrefix(relative, "/roles/") ||
		relative == "/permissions" || relative == "/menus" || strings.HasPrefix(relative, "/menus/") ||
		relative == "/audit-logs" || strings.HasPrefix(relative, "/audit-logs/")
}

// deniesTenantResourceScope blocks process-global management routes when the
// session is acting as a non-system tenant. Platform super-admins are exempt:
// they may switch into a business tenant for ops without losing access to host
// logs, global config, and other process-scoped surfaces.
func deniesTenantResourceScope(principal identity.Principal, path string) bool {
	if principal.PlatformAdmin || principal.EffectiveTenant.ID == identity.SystemTenantID {
		return false
	}
	return !isTenantScopedManagementPath(path)
}

func isTenantScopedManagementPath(path string) bool {
	if isTenantGovernancePath(path) {
		return true
	}
	relative := strings.TrimPrefix(path, "/v0/management")
	switch {
	case relative == "/dashboard-summary", relative == "/config":
		return true
	case strings.HasPrefix(relative, "/auth-files"),
		strings.HasPrefix(relative, "/identity-fingerprint"),
		strings.HasPrefix(relative, "/model-definitions/"),
		strings.HasPrefix(relative, "/image-generation"),
		relative == "/vertex/import",
		strings.HasSuffix(relative, "-auth-url"),
		relative == "/oauth-callback",
		relative == "/get-auth-status":
		return true
	case strings.HasPrefix(relative, "/api-keys"),
		strings.HasPrefix(relative, "/api-key-entries"),
		strings.HasPrefix(relative, "/api-key-permission-profiles"):
		return true
	case strings.HasPrefix(relative, "/gemini-api-key"),
		strings.HasPrefix(relative, "/claude-api-key"),
		strings.HasPrefix(relative, "/bedrock-api-key"),
		strings.HasPrefix(relative, "/opencode-go-api-key"),
		strings.HasPrefix(relative, "/cline-api-key"),
		strings.HasPrefix(relative, "/ollama-cloud-api-key"),
		strings.HasPrefix(relative, "/codex-api-key"),
		strings.HasPrefix(relative, "/vertex-api-key"),
		strings.HasPrefix(relative, "/openai-compatibility"),
		relative == "/oauth-model-alias",
		relative == "/codex-oauth-admission":
		return true
	case relative == "/models",
		relative == "/models/configured-availability",
		relative == "/model-path-availability",
		strings.HasPrefix(relative, "/model-configs"),
		strings.HasPrefix(relative, "/model-openrouter-sync"),
		relative == "/model-owner-presets",
		relative == "/auth-group-model-owner-mappings",
		relative == "/model-pricing":
		return true
	case relative == "/channel-groups",
		relative == "/ccswitch-import-configs",
		relative == "/routing-config",
		relative == "/proxy-pool",
		strings.HasPrefix(relative, "/proxy-pool/"):
		return true
	// Tenant auth-file quota/connectivity probes use /api-call with a
	// tenant-scoped auth_index (AuthByIndex + ListForTenant). Blocking this
	// path leaves imported OAuth cards stuck on "错误 / --" even when the
	// credential file itself is valid.
	case relative == "/api-call":
		return true
	case relative == "/usage/logs",
		(strings.HasSuffix(relative, "/content") || strings.HasSuffix(relative, "/egress")) && strings.HasPrefix(relative, "/usage/logs/"),
		relative == "/usage/chart-data",
		relative == "/usage/entity-stats",
		relative == "/usage/auth-file-group-trend",
		relative == "/usage/auth-file-trend",
		relative == "/usage/auth-file-quota-snapshot",
		relative == "/quota/reconcile",
		relative == "/quota/clear-status",
		relative == "/ai-accounts/status",
		relative == "/ai-accounts/status-refresh",
		strings.HasPrefix(relative, "/ai-accounts/status-refresh/"):
		return true
	default:
		return false
	}
}

func (h *Handler) PatchUser(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	user, err := h.identity().UpdateUserStatus(c.Request.Context(), principal, principal.EffectiveTenant.ID, c.Param("id"), body.Status, body.Version)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, user)
}

func (h *Handler) PutUserRoles(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		RoleIDs []string `json:"role_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.identity().AssignUserRoles(c.Request.Context(), principal, principal.EffectiveTenant.ID, c.Param("id"), body.RoleIDs); err != nil {
		identityError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) PutRoleUsers(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		UserIDs []string `json:"user_ids"`
		Version int64    `json:"version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.identity().ReplaceRoleUsers(c.Request.Context(), principal, principal.EffectiveTenant.ID, c.Param("id"), body.UserIDs, body.Version); err != nil {
		identityError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) DeleteUser(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if err := h.identity().DeleteUser(c.Request.Context(), principal, principal.EffectiveTenant.ID, c.Param("id")); err != nil {
		identityError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) DeleteRole(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if err := h.identity().DeleteRole(c.Request.Context(), principal, principal.EffectiveTenant.ID, c.Param("id")); err != nil {
		identityError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) DeleteTenant(c *gin.Context) {
	principal, _ := principalFromContext(c)
	var body struct {
		Version int64 `json:"version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	tenant, err := h.identity().DeleteTenant(c.Request.Context(), principal, c.Param("id"), body.Version)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, tenant)
}

func (h *Handler) GetAuditLogs(c *gin.Context) {
	principal, _ := principalFromContext(c)
	platform := principal.Has("platform.audit.read")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "50"))
	result, err := h.identity().ListAuditLogs(c.Request.Context(), principal.EffectiveTenant.ID, platform, page, size)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetAuditLog(c *gin.Context) {
	principal, _ := principalFromContext(c)
	platform := principal.Has("platform.audit.read")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	item, err := h.identity().GetAuditLog(c.Request.Context(), principal.EffectiveTenant.ID, platform, id)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, item)
}

func (h *Handler) DeleteAuditLog(c *gin.Context) {
	principal, _ := principalFromContext(c)
	platform := principal.Has("platform.audit.read")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if err := h.identity().DeleteAuditLog(c.Request.Context(), principal.EffectiveTenant.ID, platform, id); err != nil {
		identityError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
