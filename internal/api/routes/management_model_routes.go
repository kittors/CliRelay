package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementModelRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	models := h.Models()
	group.GET("/models", models.GetModels)
	group.GET("/models/configured-availability", models.GetConfiguredModelAvailability)
	group.GET("/model-path-availability", models.GetModelPathAvailability)
	group.GET("/model-configs", models.GetModelConfigs)
	group.POST("/model-configs", models.PostModelConfig)
	group.PUT("/model-configs/*id", models.PutModelConfig)
	group.DELETE("/model-configs/*id", models.DeleteModelConfig)
	group.GET("/model-owner-presets", models.GetModelOwnerPresets)
	group.PUT("/model-owner-presets", models.PutModelOwnerPresets)
	group.GET("/auth-group-model-owner-mappings", models.GetAuthGroupModelOwnerMappings)
	group.PATCH("/auth-group-model-owner-mappings", models.PatchAuthGroupModelOwnerMapping)
	group.GET("/model-openrouter-sync", models.GetOpenRouterModelSync)
	group.PUT("/model-openrouter-sync", models.PutOpenRouterModelSync)
	group.POST("/model-openrouter-sync/run", models.PostOpenRouterModelSyncRun)
	group.GET("/channel-groups", h.GetChannelGroups)
	group.GET("/ccswitch-import-configs", h.GetCcSwitchImportConfigs)
	group.PUT("/ccswitch-import-configs", h.PutCcSwitchImportConfigs)
	group.GET("/routing-config", h.GetRoutingConfig)
	group.PUT("/routing-config", h.PutRoutingConfig)
	group.GET("/identity-fingerprint", h.GetIdentityFingerprint)
	group.GET("/identity-fingerprint/account", h.GetIdentityFingerprintAccount)
	group.PUT("/identity-fingerprint/account/policy", h.PutIdentityFingerprintAccountPolicy)
	group.DELETE("/identity-fingerprint/account/profile", h.DeleteIdentityFingerprintAccountProfile)
	group.GET("/identity-fingerprint/codex/recommendations", h.GetCodexFingerprintRecommendations)
	group.PUT("/identity-fingerprint", h.PutIdentityFingerprint)
	group.DELETE("/identity-fingerprint/learned", h.DeleteIdentityFingerprintLearned)
	group.GET("/codex-oauth-admission", h.GetCodexOAuthAdmission)
	group.PUT("/codex-oauth-admission", h.PutCodexOAuthAdmission)
	group.GET("/model-pricing", models.GetModelPricing)
	group.PUT("/model-pricing", models.PutModelPricing)
}
