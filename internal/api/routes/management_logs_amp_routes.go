package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
)

func registerManagementLogRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/logs", h.GetLogs)
	group.DELETE("/logs", h.DeleteLogs)
	group.GET("/request-error-logs", h.GetRequestErrorLogs)
	group.GET("/request-error-logs/:name", h.DownloadRequestErrorLog)
	group.GET("/request-log-by-id/:id", h.GetRequestLogByID)
	group.GET("/request-log", h.GetRequestLog)
	group.PUT("/request-log", h.PutRequestLog)
	group.PATCH("/request-log", h.PutRequestLog)
	group.GET("/ws-auth", h.GetWebsocketAuth)
	group.PUT("/ws-auth", h.PutWebsocketAuth)
	group.PATCH("/ws-auth", h.PutWebsocketAuth)
}

func registerManagementAmpRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/ampcode", h.GetAmpCode)
	group.GET("/ampcode/upstream-url", h.GetAmpUpstreamURL)
	group.PUT("/ampcode/upstream-url", h.PutAmpUpstreamURL)
	group.PATCH("/ampcode/upstream-url", h.PutAmpUpstreamURL)
	group.DELETE("/ampcode/upstream-url", h.DeleteAmpUpstreamURL)
	group.GET("/ampcode/upstream-api-key", h.GetAmpUpstreamAPIKey)
	group.PUT("/ampcode/upstream-api-key", h.PutAmpUpstreamAPIKey)
	group.PATCH("/ampcode/upstream-api-key", h.PutAmpUpstreamAPIKey)
	group.DELETE("/ampcode/upstream-api-key", h.DeleteAmpUpstreamAPIKey)
	group.GET("/ampcode/restrict-management-to-localhost", h.GetAmpRestrictManagementToLocalhost)
	group.PUT("/ampcode/restrict-management-to-localhost", h.PutAmpRestrictManagementToLocalhost)
	group.PATCH("/ampcode/restrict-management-to-localhost", h.PutAmpRestrictManagementToLocalhost)
	group.GET("/ampcode/model-mappings", h.GetAmpModelMappings)
	group.PUT("/ampcode/model-mappings", h.PutAmpModelMappings)
	group.PATCH("/ampcode/model-mappings", h.PatchAmpModelMappings)
	group.DELETE("/ampcode/model-mappings", h.DeleteAmpModelMappings)
	group.GET("/ampcode/force-model-mappings", h.GetAmpForceModelMappings)
	group.PUT("/ampcode/force-model-mappings", h.PutAmpForceModelMappings)
	group.PATCH("/ampcode/force-model-mappings", h.PutAmpForceModelMappings)
	group.GET("/ampcode/upstream-api-keys", h.GetAmpUpstreamAPIKeys)
	group.PUT("/ampcode/upstream-api-keys", h.PutAmpUpstreamAPIKeys)
	group.PATCH("/ampcode/upstream-api-keys", h.PatchAmpUpstreamAPIKeys)
	group.DELETE("/ampcode/upstream-api-keys", h.DeleteAmpUpstreamAPIKeys)
}
