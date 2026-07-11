package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementSettingsRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/config", h.GetConfig)
	group.GET("/config.yaml", h.GetConfigYAML)
	group.PUT("/config.yaml", h.PutConfigYAML)
	group.GET("/latest-version", h.GetLatestVersion)
	group.GET("/update/check", h.CheckUpdate)
	group.GET("/update/current", h.GetCurrentUpdateState)
	group.GET("/update/progress", h.GetUpdateProgress)
	group.GET("/update/events", h.StreamUpdateProgress)
	group.POST("/update/apply", h.ApplyUpdate)
	group.GET("/auto-update/enabled", h.GetAutoUpdateEnabled)
	group.PUT("/auto-update/enabled", h.PutAutoUpdateEnabled)
	group.PATCH("/auto-update/enabled", h.PutAutoUpdateEnabled)
	group.GET("/auto-update/channel", h.GetAutoUpdateChannel)
	group.PUT("/auto-update/channel", h.PutAutoUpdateChannel)
	group.PATCH("/auto-update/channel", h.PutAutoUpdateChannel)

	group.GET("/debug", h.GetDebug)
	group.PUT("/debug", h.PutDebug)
	group.PATCH("/debug", h.PutDebug)

	group.GET("/logging-to-file", h.GetLoggingToFile)
	group.PUT("/logging-to-file", h.PutLoggingToFile)
	group.PATCH("/logging-to-file", h.PutLoggingToFile)

	group.GET("/logs-max-total-size-mb", h.GetLogsMaxTotalSizeMB)
	group.PUT("/logs-max-total-size-mb", h.PutLogsMaxTotalSizeMB)
	group.PATCH("/logs-max-total-size-mb", h.PutLogsMaxTotalSizeMB)

	group.GET("/error-logs-max-files", h.GetErrorLogsMaxFiles)
	group.PUT("/error-logs-max-files", h.PutErrorLogsMaxFiles)
	group.PATCH("/error-logs-max-files", h.PutErrorLogsMaxFiles)

	group.GET("/usage-statistics-enabled", h.GetUsageStatisticsEnabled)
	group.PUT("/usage-statistics-enabled", h.PutUsageStatisticsEnabled)
	group.PATCH("/usage-statistics-enabled", h.PutUsageStatisticsEnabled)

	group.GET("/request-log-storage/store-content", h.GetRequestLogBodyStorage)
	group.PUT("/request-log-storage/store-content", h.PutRequestLogBodyStorage)
	group.PATCH("/request-log-storage/store-content", h.PutRequestLogBodyStorage)

	group.GET("/proxy-url", h.GetProxyURL)
	group.PUT("/proxy-url", h.PutProxyURL)
	group.PATCH("/proxy-url", h.PutProxyURL)
	group.DELETE("/proxy-url", h.DeleteProxyURL)
	group.GET("/proxy-pool", h.GetProxyPool)
	group.PUT("/proxy-pool", h.PutProxyPool)
	group.PATCH("/proxy-pool/:id", h.PatchProxyPoolEntry)
	group.POST("/proxy-pool/check", h.PostProxyPoolCheck)

	group.POST("/api-call", h.APITools().APICall)

	group.GET("/quota-exceeded/switch-project", h.GetSwitchProject)
	group.PUT("/quota-exceeded/switch-project", h.PutSwitchProject)
	group.PATCH("/quota-exceeded/switch-project", h.PutSwitchProject)

	group.GET("/quota-exceeded/switch-preview-model", h.GetSwitchPreviewModel)
	group.PUT("/quota-exceeded/switch-preview-model", h.PutSwitchPreviewModel)
	group.PATCH("/quota-exceeded/switch-preview-model", h.PutSwitchPreviewModel)
	group.POST("/quota/reconcile", h.PostQuotaReconcile)
	group.POST("/quota/clear-status", h.PostQuotaClearStatus)
}

func registerManagementRuntimeTuningRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/request-retry", h.GetRequestRetry)
	group.PUT("/request-retry", h.PutRequestRetry)
	group.PATCH("/request-retry", h.PutRequestRetry)
	group.GET("/max-retry-interval", h.GetMaxRetryInterval)
	group.PUT("/max-retry-interval", h.PutMaxRetryInterval)
	group.PATCH("/max-retry-interval", h.PutMaxRetryInterval)

	group.GET("/force-model-prefix", h.GetForceModelPrefix)
	group.PUT("/force-model-prefix", h.PutForceModelPrefix)
	group.PATCH("/force-model-prefix", h.PutForceModelPrefix)

	group.GET("/routing/strategy", h.GetRoutingStrategy)
	group.PUT("/routing/strategy", h.PutRoutingStrategy)
	group.PATCH("/routing/strategy", h.PutRoutingStrategy)
}
