package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementAPIKeyRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	keys := h.ProviderKeys()
	group.GET("/api-keys", h.GetAPIKeys)
	group.PUT("/api-keys", h.PutAPIKeys)
	group.PATCH("/api-keys", h.PatchAPIKeys)
	group.DELETE("/api-keys", h.DeleteAPIKeys)

	group.GET("/api-key-permission-profiles", h.GetAPIKeyPermissionProfiles)
	group.PUT("/api-key-permission-profiles", h.PutAPIKeyPermissionProfiles)

	group.GET("/api-key-entries", h.GetAPIKeyEntries)
	group.PUT("/api-key-entries", h.PutAPIKeyEntries)
	group.PATCH("/api-key-entries", h.PatchAPIKeyEntry)
	group.DELETE("/api-key-entries", h.DeleteAPIKeyEntry)

	group.GET("/gemini-api-key", keys.GetGeminiKeys)
	group.PUT("/gemini-api-key", keys.PutGeminiKeys)
	group.PATCH("/gemini-api-key", keys.PatchGeminiKey)
	group.DELETE("/gemini-api-key", keys.DeleteGeminiKey)
}

func registerManagementProviderRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	keys := h.ProviderKeys()
	group.GET("/claude-api-key", keys.GetClaudeKeys)
	group.PUT("/claude-api-key", keys.PutClaudeKeys)
	group.PATCH("/claude-api-key", keys.PatchClaudeKey)
	group.DELETE("/claude-api-key", keys.DeleteClaudeKey)

	group.GET("/bedrock-api-key", keys.GetBedrockKeys)
	group.PUT("/bedrock-api-key", keys.PutBedrockKeys)
	group.PATCH("/bedrock-api-key", keys.PatchBedrockKey)
	group.DELETE("/bedrock-api-key", keys.DeleteBedrockKey)

	group.GET("/opencode-go-api-key", keys.GetOpenCodeGoKeys)
	group.PUT("/opencode-go-api-key", keys.PutOpenCodeGoKeys)
	group.PATCH("/opencode-go-api-key", keys.PatchOpenCodeGoKey)
	group.DELETE("/opencode-go-api-key", keys.DeleteOpenCodeGoKey)
	group.POST("/opencode-go-api-key/usage", h.QueryOpenCodeGoUsage)

	group.GET("/cline-api-key", keys.GetClineKeys)
	group.PUT("/cline-api-key", keys.PutClineKeys)
	group.PATCH("/cline-api-key", keys.PatchClineKey)
	group.DELETE("/cline-api-key", keys.DeleteClineKey)

	group.GET("/codex-api-key", keys.GetCodexKeys)
	group.PUT("/codex-api-key", keys.PutCodexKeys)
	group.PATCH("/codex-api-key", keys.PatchCodexKey)
	group.DELETE("/codex-api-key", keys.DeleteCodexKey)

	group.GET("/openai-compatibility", keys.GetOpenAICompat)
	group.PUT("/openai-compatibility", keys.PutOpenAICompat)
	group.PATCH("/openai-compatibility", keys.PatchOpenAICompat)
	group.DELETE("/openai-compatibility", keys.DeleteOpenAICompat)

	group.GET("/vertex-api-key", keys.GetVertexCompatKeys)
	group.PUT("/vertex-api-key", keys.PutVertexCompatKeys)
	group.PATCH("/vertex-api-key", keys.PatchVertexCompatKey)
	group.DELETE("/vertex-api-key", keys.DeleteVertexCompatKey)

	group.GET("/oauth-excluded-models", keys.GetOAuthExcludedModels)
	group.PUT("/oauth-excluded-models", keys.PutOAuthExcludedModels)
	group.PATCH("/oauth-excluded-models", keys.PatchOAuthExcludedModels)
	group.DELETE("/oauth-excluded-models", keys.DeleteOAuthExcludedModels)

	group.GET("/oauth-model-alias", keys.GetOAuthModelAlias)
	group.PUT("/oauth-model-alias", keys.PutOAuthModelAlias)
	group.PATCH("/oauth-model-alias", keys.PatchOAuthModelAlias)
	group.DELETE("/oauth-model-alias", keys.DeleteOAuthModelAlias)
}
