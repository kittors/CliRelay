package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type authModelCooldownClearRequest struct {
	AuthIndexSnake  *string `json:"auth_index"`
	AuthIndexCamel  *string `json:"authIndex"`
	AuthIndexPascal *string `json:"AuthIndex"`
	Model           string  `json:"model"`
}

// PostAuthFileModelCooldownClear lifts the cooldown of one model on one account,
// so the next request may try it at once ("retry this model now"). Unlike
// POST /quota/clear-status it leaves the account's other models and its
// credential state alone.
//
// The account is looked up among the caller's tenant, so another tenant's
// account is indistinguishable from one that does not exist (404). A model with
// nothing to clear answers 200 with changed=false: the goal state already holds.
func (h *Handler) PostAuthFileModelCooldownClear(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}

	var body authModelCooldownClearRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := h.authByIndexForTenant(effectiveTenantID(c), authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	changed, err := h.authManager.ClearModelCooldown(c.Request.Context(), auth.ID, model)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "changed": changed})
}
