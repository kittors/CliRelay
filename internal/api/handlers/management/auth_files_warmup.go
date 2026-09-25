package management

import (
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
	log "github.com/sirupsen/logrus"
)

type warmupPolicyStoreHolder struct{ store warmup.PolicyStore }

var warmupPolicyStore atomic.Pointer[warmupPolicyStoreHolder]

// SetWarmupPolicyStore persists warmup policies in store for schedulers
// created from now on; nil keeps them in memory, lost on restart.
func SetWarmupPolicyStore(store warmup.PolicyStore) {
	if store == nil {
		warmupPolicyStore.Store(nil)
		return
	}
	warmupPolicyStore.Store(&warmupPolicyStoreHolder{store: store})
}

func sharedWarmupPolicyStore() warmup.PolicyStore {
	if holder := warmupPolicyStore.Load(); holder != nil {
		return holder.store
	}
	return nil
}

// StartWarmupScheduler starts policy scheduling at server startup. Stored
// policies must keep running after a restart, before anyone opens the panel;
// in a cluster only the leader evaluates them.
func (h *Handler) StartWarmupScheduler() {
	if h != nil {
		h.warmupService()
	}
}

func (h *Handler) stopWarmupScheduler() {
	h.mu.Lock()
	svc := h.warmupSvc
	h.mu.Unlock()
	if svc != nil {
		svc.Stop()
	}
}

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

// PostWarmupBatch triggers a staggered warmup for multiple accounts.
func (h *Handler) PostWarmupBatch(c *gin.Context) {
	var req struct {
		AuthIDs []string `json:"auth_ids" binding:"required"`
		PoolID  string   `json:"pool_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	count, err := h.warmupService().WarmupBatchAccounts(c.Request.Context(), req.AuthIDs, req.PoolID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"dispatched": count})
}

// GetWarmupPolicies returns all configured warmup policies.
func (h *Handler) GetWarmupPolicies(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	policies, err := h.warmupService().ListPolicies(c.Request.Context(), tenantID)
	if err != nil {
		log.WithError(err).Error("warmup: failed to list policies")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load warmup policies"})
		return
	}
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

	if err := h.warmupService().SavePolicy(c.Request.Context(), p); err != nil {
		log.WithError(err).Error("warmup: failed to save policy")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save warmup policy"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "policy": p})
}
