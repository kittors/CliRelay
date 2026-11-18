package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
)

func registerPublicManagementRoutes(engine *gin.Engine, h *managementhandlers.Handler, opts ManagementOptions) {
	publicMiddlewares := make([]gin.HandlerFunc, 0, 3)
	if opts.Availability != nil {
		publicMiddlewares = append(publicMiddlewares, opts.Availability)
	}
	if opts.PublicNoStore != nil {
		publicMiddlewares = append(publicMiddlewares, opts.PublicNoStore)
	}
	if opts.PublicRateLimit != nil {
		publicMiddlewares = append(publicMiddlewares, opts.PublicRateLimit)
	}

	pub := engine.Group("/v0/management/public")
	pub.Use(publicMiddlewares...)
	usageLogs := h.UsageLogs()
	{
		pub.GET("/ping", h.GetPublicPing)
		pub.GET("/usage", h.GetPublicUsageByAPIKey)
		pub.POST("/usage", h.GetPublicUsageByAPIKey)
		pub.GET("/ccswitch-import-configs", h.GetPublicCcSwitchImportConfigs)
		pub.POST("/ccswitch-import-configs", h.GetPublicCcSwitchImportConfigs)
		pub.GET("/usage/logs", usageLogs.GetPublicUsageLogs)
		pub.POST("/usage/logs", usageLogs.GetPublicUsageLogs)
		pub.GET("/usage/logs/:id/content", usageLogs.GetPublicLogContent)
		pub.POST("/usage/logs/:id/content", usageLogs.GetPublicLogContent)
		pub.GET("/usage/chart-data", usageLogs.GetPublicUsageChartData)
		pub.POST("/usage/chart-data", usageLogs.GetPublicUsageChartData)
		pub.POST("/usage/summary", h.GetPublicUsageSummary)
	}
}
