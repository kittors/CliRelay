package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementCoreRoutes(group *gin.RouterGroup, h *managementhandlers.Handler, clearWriteDeadline func(*gin.Context)) {
	group.GET("/dashboard-summary", h.GetDashboardSummary)
	group.GET("/system-stats", h.GetSystemStats)
	group.GET("/cluster", h.GetClusterStatus)
	group.GET("/system-stats/ws", func(c *gin.Context) {
		clearWriteDeadline(c)
		h.SystemStatsWebSocket(c)
	})
}
