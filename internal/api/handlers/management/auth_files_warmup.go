package management

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
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
		writeMissingFieldError(c, "auth_id", "请通过查询参数 auth_id 指定要查看预热目标的账号凭据 ID")
		return
	}

	targets, err := h.warmupService().GetAccountTargets(effectiveTenantID(c), authID)
	if err != nil {
		writeNotFoundError(c, "预热账号凭据", authID)
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
		writeInvalidBodyError(c, err)
		return
	}

	res, err := h.warmupService().WarmupSingleAccount(c.Request.Context(), effectiveTenantID(c), req.AuthID, req.PoolID)
	if err != nil {
		if errors.Is(err, warmup.ErrAuthNotFound) {
			writeNotFoundError(c, "预热账号凭据", req.AuthID)
			return
		}
		writeInternalError(c, "执行单账号预热", err)
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
		writeInvalidBodyError(c, err)
		return
	}

	count, err := h.warmupService().WarmupBatchAccounts(c.Request.Context(), effectiveTenantID(c), req.AuthIDs, req.PoolID)
	if err != nil {
		writeInternalError(c, "批量派发账号预热任务", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"dispatched": count})
}

// GetWarmupPolicies returns all configured warmup policies.
func (h *Handler) GetWarmupPolicies(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	policies, err := h.warmupService().ListPolicies(c.Request.Context(), tenantID)
	if err != nil {
		writeInternalError(c, "读取预热策略列表", err)
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
		writeInvalidBodyError(c, err)
		return
	}
	p.TenantID = effectiveTenantID(c)
	if p.ID == "" {
		writeMissingFieldError(c, "id", "请提供预热策略的唯一标识（id）")
		return
	}

	if err := h.warmupService().SavePolicy(c.Request.Context(), p); err != nil {
		writeInternalError(c, fmt.Sprintf("保存预热策略 %q", p.ID), err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "policy": p})
}
