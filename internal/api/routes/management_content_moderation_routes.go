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

	// Sensitive-word policies: the platform's (its settings are readable by every
	// tenant, its word list and the right to edit it belong to a platform
	// administrator in the system tenant), the effective tenant's and one API
	// key's. validate and test change nothing.
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
	// Portal users' own words: who of the tenant's portal users may keep them,
	// and the words of one of them.
	group.GET("/content-moderation/word-policies/portal", h.GetWordPortal)
	group.PUT("/content-moderation/word-policies/portal", h.PutWordPortal)
	group.PUT("/content-moderation/word-policies/portal/users/:id", h.PutWordPortalUser)
	group.GET("/content-moderation/word-policies/end-users/:id", h.GetWordPolicyEndUser)
	group.PUT("/content-moderation/word-policies/end-users/:id", h.PutWordPolicyEndUser)
	group.DELETE("/content-moderation/word-policies/end-users/:id", h.DeleteWordPolicyEndUser)
	group.POST("/content-moderation/word-policies/validate", h.PostWordPolicyValidate)
	group.POST("/content-moderation/word-policies/test", h.PostWordPolicyTest)

	// Outbound secret masking: the tenant's switch.
	group.GET("/content-moderation/secret-masking", h.GetSecretMasking)
	group.PUT("/content-moderation/secret-masking", h.PutSecretMasking)

	// Privacy masking: settings, the portal users' permission, the records.
	group.GET("/privacy-masking/settings", h.GetPrivacyMaskingSettings)
	group.PUT("/privacy-masking/settings", h.PutPrivacyMaskingSettings)
	group.POST("/privacy-masking/preview", h.PostPrivacyMaskingPreview)
	group.GET("/privacy-masking/portal", h.GetPrivacyMaskingPortal)
	group.PUT("/privacy-masking/portal/users/:id", h.PutPrivacyMaskingPortalUser)
	group.GET("/privacy-masking/summary", h.GetPrivacyMaskingSummary)
	group.GET("/privacy-masking/records", h.GetPrivacyMaskingRecords)
	group.GET("/privacy-masking/records/:key", h.GetPrivacyMaskingRecord)
	group.DELETE("/privacy-masking/records", h.DeletePrivacyMaskingRecords)
}
