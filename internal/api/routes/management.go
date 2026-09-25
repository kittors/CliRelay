package routes

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

type ManagementOptions struct {
	Availability       gin.HandlerFunc
	PublicNoStore      gin.HandlerFunc
	PublicRateLimit    gin.HandlerFunc
	ClearWriteDeadline func(*gin.Context)
}

func RegisterManagement(engine *gin.Engine, h *managementhandlers.Handler, opts ManagementOptions) {
	if engine == nil || h == nil {
		return
	}

	registerIdentityAuthRoutes(engine, h, opts.Availability)

	clearWriteDeadline := opts.ClearWriteDeadline
	if clearWriteDeadline == nil {
		clearWriteDeadline = func(*gin.Context) {}
	}

	mgmtMiddlewares := make([]gin.HandlerFunc, 0, 5)
	if opts.Availability != nil {
		mgmtMiddlewares = append(mgmtMiddlewares, opts.Availability)
	}
	mgmtMiddlewares = append(
		mgmtMiddlewares,
		managementSecurityHeaders(),
		h.Middleware(),
		bodyutil.LimitBodyMiddleware(bodyutil.ManagementBodyLimit),
		// After the size limit: it buffers the body to read the optional
		// "version" a write was based on.
		managementhandlers.CaptureConfigVersion(),
	)

	mgmt := engine.Group("/v0/management")
	mgmt.Use(mgmtMiddlewares...)

	registerManagementIdentityRoutes(mgmt, h)
	registerManagementCoreRoutes(mgmt, h, clearWriteDeadline)
	registerManagementModelRoutes(mgmt, h)
	registerManagementUsageRoutes(mgmt, h)
	registerManagementSettingsRoutes(mgmt, h)
	registerManagementContentModerationRoutes(mgmt, h)
	registerManagementAPIKeyRoutes(mgmt, h)
	registerManagementLogRoutes(mgmt, h)
	registerManagementAmpRoutes(mgmt, h)
	registerManagementRuntimeTuningRoutes(mgmt, h)
	registerManagementProviderRoutes(mgmt, h)
	registerManagementAuthRoutes(mgmt, h)
	registerPublicManagementRoutes(engine, h, opts)
}
