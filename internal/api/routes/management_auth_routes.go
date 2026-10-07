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
	group.GET("/video-generation/models", h.ListVideoGenerationModels)
	group.POST("/video-generation/test", h.PostVideoGenerationTest)
	group.GET("/video-generation/test/:task_id", h.GetVideoGenerationTestTask)
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
	group.GET("/xai-auth-url", h.RequestXAIToken)
	group.GET("/iflow-auth-url", h.RequestIFlowToken)
	group.POST("/iflow-auth-url", h.RequestIFlowCookieToken)
	group.POST("/oauth-callback", h.PostOAuthCallback)
	group.GET("/get-auth-status", h.GetAuthStatus)

	// Adding an account from a credential the operator already holds, rather
	// than by signing in. "oauth-import" keeps these on the auth_files.oauth
	// permission the route mapping already grants anything containing "oauth".
	group.POST("/oauth-import/anthropic-session", h.ImportAnthropicSession)
	group.POST("/oauth-import/codex-refresh-token", h.ImportCodexRefreshToken)
	group.POST("/oauth-import/antigravity-refresh-token", h.ImportAntigravityRefreshToken)
	group.POST("/oauth-import/xai-sso", h.ImportXAISSO)

	group.GET("/auth-files/warmup/targets", h.GetWarmupAccountTargets)
	group.POST("/auth-files/warmup/run", h.PostWarmupAccount)
	group.POST("/auth-files/warmup/batch", h.PostWarmupBatch)
	group.GET("/auth-files/warmup/policies", h.GetWarmupPolicies)
	group.POST("/auth-files/warmup/policies", h.PostWarmupPolicy)
}
