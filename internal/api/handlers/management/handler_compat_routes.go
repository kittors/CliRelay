package management

import "github.com/gin-gonic/gin"

// APICall forwards the legacy root handler method to the tenant-aware API tools service.
func (h *Handler) APICall(c *gin.Context) {
	if h == nil {
		c.AbortWithStatusJSON(503, gin.H{"error": "handler unavailable"})
		return
	}
	h.APITools().APICall(c)
}

// GetOAuthCallback keeps the pre-v8 route name available to the server router.
func (h *Handler) GetOAuthCallback(c *gin.Context) {
	h.GetOAuthCallbackV8(c)
}

// ResetQuota keeps the legacy endpoint available; credential-specific quota
// reset logic is implemented by ResetCredentialQuota.
func (h *Handler) ResetQuota(c *gin.Context) {
	h.ResetCredentialQuota(c)
}

// Provider-key methods below preserve the legacy v0 handler surface while the
// implementation lives in the tenant-aware ProviderKeysHandler.
func (h *Handler) GetGeminiKeys(c *gin.Context) {
	h.ProviderKeys().GetGeminiKeys(c)
}

func (h *Handler) PutGeminiKeys(c *gin.Context) {
	h.ProviderKeys().PutGeminiKeys(c)
}

func (h *Handler) PatchGeminiKey(c *gin.Context) {
	h.ProviderKeys().PatchGeminiKey(c)
}

func (h *Handler) DeleteGeminiKey(c *gin.Context) {
	h.ProviderKeys().DeleteGeminiKey(c)
}

func (h *Handler) GetInteractionsKeys(c *gin.Context) {
	h.ProviderKeys().GetInteractionsKeys(c)
}

func (h *Handler) PutInteractionsKeys(c *gin.Context) {
	h.ProviderKeys().PutInteractionsKeys(c)
}

func (h *Handler) PatchInteractionsKey(c *gin.Context) {
	h.ProviderKeys().PatchInteractionsKey(c)
}

func (h *Handler) DeleteInteractionsKey(c *gin.Context) {
	h.ProviderKeys().DeleteInteractionsKey(c)
}

func (h *Handler) GetClaudeKeys(c *gin.Context) {
	h.ProviderKeys().GetClaudeKeys(c)
}

func (h *Handler) PutClaudeKeys(c *gin.Context) {
	h.ProviderKeys().PutClaudeKeys(c)
}

func (h *Handler) PatchClaudeKey(c *gin.Context) {
	h.ProviderKeys().PatchClaudeKey(c)
}

func (h *Handler) DeleteClaudeKey(c *gin.Context) {
	h.ProviderKeys().DeleteClaudeKey(c)
}

func (h *Handler) GetCodexKeys(c *gin.Context) {
	h.ProviderKeys().GetCodexKeys(c)
}

func (h *Handler) PutCodexKeys(c *gin.Context) {
	h.ProviderKeys().PutCodexKeys(c)
}

func (h *Handler) PatchCodexKey(c *gin.Context) {
	h.ProviderKeys().PatchCodexKey(c)
}

func (h *Handler) DeleteCodexKey(c *gin.Context) {
	h.ProviderKeys().DeleteCodexKey(c)
}

func (h *Handler) GetOpenAICompat(c *gin.Context) {
	h.ProviderKeys().GetOpenAICompat(c)
}

func (h *Handler) PutOpenAICompat(c *gin.Context) {
	h.ProviderKeys().PutOpenAICompat(c)
}

func (h *Handler) PatchOpenAICompat(c *gin.Context) {
	h.ProviderKeys().PatchOpenAICompat(c)
}

func (h *Handler) DeleteOpenAICompat(c *gin.Context) {
	h.ProviderKeys().DeleteOpenAICompat(c)
}

func (h *Handler) GetVertexCompatKeys(c *gin.Context) {
	h.ProviderKeys().GetVertexCompatKeys(c)
}

func (h *Handler) PutVertexCompatKeys(c *gin.Context) {
	h.ProviderKeys().PutVertexCompatKeys(c)
}

func (h *Handler) PatchVertexCompatKey(c *gin.Context) {
	h.ProviderKeys().PatchVertexCompatKey(c)
}

func (h *Handler) DeleteVertexCompatKey(c *gin.Context) {
	h.ProviderKeys().DeleteVertexCompatKey(c)
}

func (h *Handler) GetOAuthExcludedModels(c *gin.Context) {
	h.ProviderKeys().GetOAuthExcludedModels(c)
}

func (h *Handler) PutOAuthExcludedModels(c *gin.Context) {
	h.ProviderKeys().PutOAuthExcludedModels(c)
}

func (h *Handler) PatchOAuthExcludedModels(c *gin.Context) {
	h.ProviderKeys().PatchOAuthExcludedModels(c)
}

func (h *Handler) DeleteOAuthExcludedModels(c *gin.Context) {
	h.ProviderKeys().DeleteOAuthExcludedModels(c)
}

func (h *Handler) GetOAuthModelAlias(c *gin.Context) {
	h.ProviderKeys().GetOAuthModelAlias(c)
}

func (h *Handler) PutOAuthModelAlias(c *gin.Context) {
	h.ProviderKeys().PutOAuthModelAlias(c)
}

func (h *Handler) PatchOAuthModelAlias(c *gin.Context) {
	h.ProviderKeys().PatchOAuthModelAlias(c)
}

func (h *Handler) DeleteOAuthModelAlias(c *gin.Context) {
	h.ProviderKeys().DeleteOAuthModelAlias(c)
}
