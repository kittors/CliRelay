package routes

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerIdentityAuthRoutes(engine *gin.Engine, h *managementhandlers.Handler, availability gin.HandlerFunc) {
	middlewares := []gin.HandlerFunc{managementSecurityHeaders()}
	if availability != nil {
		middlewares = append([]gin.HandlerFunc{availability}, middlewares...)
	}
	middlewares = append(middlewares, bodyutil.LimitBodyMiddleware(bodyutil.ManagementBodyLimit))
	auth := engine.Group("/v0/auth")
	auth.Use(middlewares...)
	auth.POST("/login", h.PostLogin)
	auth.POST("/refresh", h.PostRefresh)
	auth.POST("/logout", h.PostLogout)
	auth.GET("/me", h.GetMe)
	auth.PUT("/password", h.PutPassword)

	portal := engine.Group("/v0/portal")
	portal.Use(middlewares...)
	// Only the two credential-bearing portal routes are throttled, and per-route
	// rather than on the group: the rest already require a portal session, so a
	// shared bucket would let one client's failed logins rate-limit everyone
	// else's key management. Attaching it here (not in the handler) keeps the
	// route table unchanged.
	portalAuthThrottle := h.PortalAuthThrottleMiddleware()
	portal.POST("/auth/login", portalAuthThrottle, h.PostPortalLogin)
	portal.POST("/auth/refresh", portalAuthThrottle, h.PostPortalRefresh)
	portal.POST("/auth/logout", h.PostPortalLogout)
	portal.GET("/auth/me", h.GetPortalMe)
	portal.PUT("/auth/password", h.PutPortalPassword)
	portal.GET("/api-keys", h.GetPortalAPIKeys)
	portal.POST("/api-keys", h.PostPortalAPIKey)
	portal.GET("/api-keys/:id/secret", h.GetPortalAPIKeySecret)
	portal.PATCH("/api-keys/:id", h.PatchPortalAPIKey)
	portal.POST("/api-keys/:id/rotate", h.PostPortalAPIKeyRotate)
	portal.POST("/api-keys/:id/period-spending/reset", h.PostPortalAPIKeyPeriodSpendingReset)
	portal.DELETE("/api-keys/:id", h.DeletePortalAPIKey)
}

func registerManagementIdentityRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/tenants", h.GetTenants)
	group.POST("/tenants", h.PostTenant)
	group.PATCH("/tenants/:id", h.PatchTenant)
	group.DELETE("/tenants/:id", h.DeleteTenant)

	group.GET("/users", h.GetUsers)
	group.POST("/users", h.PostUser)
	group.PATCH("/users/:id", h.PatchUser)
	group.DELETE("/users/:id", h.DeleteUser)
	group.PUT("/users/:id/roles", h.PutUserRoles)
	group.POST("/users/:id/reset-password", h.PostUserResetPassword)

	group.GET("/end-users", h.GetEndUsers)
	group.POST("/end-users", h.PostEndUser)
	group.PATCH("/end-users/:id", h.PatchEndUser)
	group.DELETE("/end-users/:id", h.DeleteEndUser)
	group.POST("/end-users/:id/reset-password", h.PostEndUserResetPassword)
	group.POST("/end-users/:id/daily-spending/reset", h.PostEndUserDailySpendingReset)
	group.POST("/end-users/:id/period-spending/reset", h.PostEndUserPeriodSpendingReset)
	group.GET("/end-users/:id/daily-spending/reset-history", h.GetEndUserDailySpendingResetHistory)
	group.GET("/end-users/:id/api-keys", h.GetEndUserAPIKeys)
	group.POST("/end-users/:id/api-keys", h.PostEndUserAPIKey)
	group.PATCH("/end-users/:id/api-keys/:key_id", h.PatchEndUserAPIKey)
	group.POST("/end-users/:id/api-keys/:key_id/rotate", h.PostEndUserAPIKeyRotate)
	group.POST("/end-users/:id/api-keys/:key_id/daily-spending/reset", h.PostEndUserAPIKeyDailySpendingReset)
	group.POST("/end-users/:id/api-keys/:key_id/period-spending/reset", h.PostEndUserAPIKeyPeriodSpendingReset)
	group.GET("/end-users/:id/api-keys/:key_id/daily-spending/reset-history", h.GetEndUserAPIKeyDailySpendingResetHistory)
	group.DELETE("/end-users/:id/api-keys/:key_id", h.DeleteEndUserAPIKey)
	group.POST("/end-users/:id/api-keys/:key_id/default", h.PostEndUserAPIKeyDefault)

	group.GET("/menus", h.GetMenus)
	group.POST("/menus", h.PostMenu)
	group.PATCH("/menus/:code", h.PatchMenu)
	group.DELETE("/menus/:code", h.DeleteMenu)
	group.GET("/permissions", h.GetPermissions)
	group.GET("/roles", h.GetRoles)
	group.POST("/roles", h.PostRole)
	group.PUT("/roles/:id/permissions", h.PutRolePermissions)
	group.PUT("/roles/:id/users", h.PutRoleUsers)
	group.DELETE("/roles/:id", h.DeleteRole)
	group.GET("/audit-logs", h.GetAuditLogs)
	group.DELETE("/audit-logs", h.ClearAuditLogs)
	group.GET("/audit-logs/:id", h.GetAuditLog)
	group.DELETE("/audit-logs/:id", h.DeleteAuditLog)
}
