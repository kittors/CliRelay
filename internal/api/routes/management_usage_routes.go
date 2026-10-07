package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementUsageRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	usageLogs := h.UsageLogs()
	group.GET("/usage", h.GetUsageStatistics)
	group.GET("/usage/export", h.ExportUsageStatistics)
	group.POST("/usage/import", h.ImportUsageStatistics)
	group.GET("/usage/logs", usageLogs.GetUsageLogs)
	group.DELETE("/usage/logs", usageLogs.DeleteUsageLogs)
	group.GET("/usage/logs/:id/content", usageLogs.GetLogContent)
	group.GET("/usage/logs/:id/egress", h.GetUsageLogEgress)
	group.GET("/usage/auth-file-group-trend", usageLogs.GetAuthFileGroupTrend)
	group.GET("/usage/auth-file-trend", usageLogs.GetAuthFileTrend)
	group.POST("/usage/auth-file-quota-snapshot", h.PostAuthFileQuotaSnapshot)
	group.GET("/usage/chart-data", usageLogs.GetUsageChartData)
	group.GET("/usage/entity-stats", usageLogs.GetEntityUsageStats)
	// Monitor center: the full overview, and the per-minute live strip that
	// the panel polls more often than the overview.
	group.GET("/usage/monitor/overview", usageLogs.GetUsageMonitorOverview)
	group.GET("/usage/monitor/realtime", usageLogs.GetUsageMonitorRealtime)

	// Backend-owned AI account latest status (replaces browser fan-out).
	group.GET("/ai-accounts/status", h.GetAIAccountStatus)
	group.POST("/ai-accounts/status-refresh", h.PostAIAccountStatusRefresh)
	group.GET("/ai-accounts/status-refresh/:job_id", h.GetAIAccountStatusRefreshJob)
}
