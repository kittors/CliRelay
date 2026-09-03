package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
)

// GetWarmupAccountTargets returns quota pools available for warmup on a specific auth file.
func (h *Handler) GetWarmupAccountTargets(c *gin.Context) {
	authID := strings.TrimSpace(c.Query("auth_id"))
	if authID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id is required"})
		return
	}

	targets, err := h.warmupService().GetAccountTargets(authID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"targets": targets})
}

// PostWarmupAccount triggers a single-account warmup.
func (h *Handler) PostWarmupAccount(c *gin.Context) {
	var req struct {
		AuthID string `json:"auth_id" binding:"required"`
		PoolID string `json:"pool_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	res, err := h.warmupService().WarmupSingleAccount(c.Request.Context(), req.AuthID, req.PoolID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"result": res})
}

// GetWarmupPolicies returns all configured warmup policies.
func (h *Handler) GetWarmupPolicies(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	policies := h.warmupService().GetPolicies(tenantID)
	c.JSON(http.StatusOK, gin.H{
		"policies": policies,
		"metrics":  h.warmupService().GetMetrics(),
	})
}

// PostWarmupPolicy creates or updates a warmup policy.
func (h *Handler) PostWarmupPolicy(c *gin.Context) {
	var p warmup.Policy
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy payload"})
		return
	}
	p.TenantID = effectiveTenantID(c)
	if p.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "policy ID is required"})
		return
	}

	h.warmupService().AddPolicy(p)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "policy": p})
}
