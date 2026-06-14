package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementAuthRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/auth-files", h.ListAuthFiles)
	group.GET("/auth-files/models", h.GetAuthFileModels)
	group.GET("/model-definitions/:channel", h.GetStaticModelDefinitions)
	group.GET("/image-generation/channels", h.ListImageGenerationChannels)
	group.GET("/image-generation/size-presets", h.GetImageGenerationSizePresets)
	group.PUT("/image-generation/size-presets", h.PutImageGenerationSizePresets)
	group.POST("/image-generation/test", h.PostImageGenerationTest)
	group.GET("/image-generation/test/:task_id", h.GetImageGenerationTestTask)
	group.GET("/auth-files/download", h.DownloadAuthFile)
	group.POST("/auth-files", h.UploadAuthFile)
	group.DELETE("/auth-files", h.DeleteAuthFile)
	group.PATCH("/auth-files/status", h.PatchAuthFileStatus)
	group.PATCH("/auth-files/fields", h.PatchAuthFileFields)
	group.POST("/vertex/import", h.ImportVertexCredential)

	group.GET("/anthropic-auth-url", h.RequestAnthropicToken)
	group.GET("/codex-auth-url", h.RequestCodexToken)
	group.GET("/gemini-cli-auth-url", h.RequestGeminiCLIToken)
	group.GET("/antigravity-auth-url", h.RequestAntigravityToken)
	group.GET("/qwen-auth-url", h.RequestQwenToken)
	group.GET("/kimi-auth-url", h.RequestKimiToken)
	group.GET("/iflow-auth-url", h.RequestIFlowToken)
	group.POST("/iflow-auth-url", h.RequestIFlowCookieToken)
	group.POST("/oauth-callback", h.PostOAuthCallback)
	group.GET("/get-auth-status", h.GetAuthStatus)
}
