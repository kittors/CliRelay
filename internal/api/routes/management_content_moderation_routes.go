package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementContentModerationRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/content-moderation/metrics", h.GetContentModerationMetrics)
	group.GET("/content-moderation/profiles", h.GetContentModerationProfiles)
	group.POST("/content-moderation/profiles", h.PostContentModerationProfile)
	group.GET("/content-moderation/profiles/:id", h.GetContentModerationProfile)
	group.PATCH("/content-moderation/profiles/:id", h.PatchContentModerationProfile)
	group.DELETE("/content-moderation/profiles/:id", h.DeleteContentModerationProfile)
	group.POST("/content-moderation/profiles/:id/test", h.PostContentModerationProfileTest)
	group.GET("/content-moderation/channels", h.GetContentModerationChannels)
	group.PATCH("/content-moderation/channel-bindings", h.PatchContentModerationChannelBindings)

	// Sensitive-word policies: the platform's (readable by every tenant, editable
	// only by a platform administrator in the system tenant), the effective
	// tenant's and one API key's. validate and test change nothing.
	group.GET("/content-moderation/word-policies", h.GetWordPolicies)
	group.GET("/content-moderation/word-policies/platform", h.GetWordPolicyPlatform)
	group.PUT("/content-moderation/word-policies/platform", h.PutWordPolicyPlatform)
	group.DELETE("/content-moderation/word-policies/platform", h.DeleteWordPolicyPlatform)
	group.GET("/content-moderation/word-policies/tenant", h.GetWordPolicyTenant)
	group.PUT("/content-moderation/word-policies/tenant", h.PutWordPolicyTenant)
	group.DELETE("/content-moderation/word-policies/tenant", h.DeleteWordPolicyTenant)
	group.GET("/content-moderation/word-policies/keys/:id", h.GetWordPolicyKey)
	group.PUT("/content-moderation/word-policies/keys/:id", h.PutWordPolicyKey)
	group.DELETE("/content-moderation/word-policies/keys/:id", h.DeleteWordPolicyKey)
	group.POST("/content-moderation/word-policies/validate", h.PostWordPolicyValidate)
	group.POST("/content-moderation/word-policies/test", h.PostWordPolicyTest)
}
