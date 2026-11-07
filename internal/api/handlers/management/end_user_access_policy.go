package management

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// accessPolicyCapabilities 声明本后端支持的访问策略能力。面板会自动升级到最新版本，
// 可能跑在旧后端上；它只在看到显式 true 时才展示并提交新字段——旧后端会静默丢弃
// 未知字段，保存看起来成功、实际没有生效。
func accessPolicyCapabilities() gin.H {
	return gin.H{
		// quota-limits / model-rules / model-routes 三个字段与按模型用量。
		"access-policy": true,
		// 按模型规则可配置 fallback-model（超额降级）。
		"model-fallback": true,
		// POST /end-users/batch/permission-profile。
		"batch-profile-binding": true,
		// POST /end-users 接受 permission-profile-id。
		"create-with-profile": true,
		// GET /end-users/:id/api-keys 返回已暂停的 Key，PATCH 接受 disabled，
		// GET .../:key_id/secret 按需取明文。
		"owned-key-admin": true,
	}
}

// decorateEndUsers 用模板覆盖后的生效值填充账号，并附上当前用量。按租户批量读取：
// 每个租户一次快照查询（6 条 SQL，与账号数无关）与一次模板读取。
func decorateEndUsers(items []enduser.User) error {
	byTenant := map[string][]int{}
	for i := range items {
		byTenant[items[i].TenantID] = append(byTenant[items[i].TenantID], i)
	}
	now := time.Now()
	for tenantID, indexes := range byTenant {
		ids := make([]string, 0, len(indexes))
		for _, i := range indexes {
			ids = append(ids, items[i].ID)
		}
		snapshots, err := usage.QueryEndUserQuotaUsageSnapshotsAt(tenantID, ids, now)
		if err != nil {
			return err
		}
		resetCounts, err := usage.ListEndUserDailySpendingResetEventCounts(tenantID, ids)
		if err != nil {
			return err
		}
		profiles := usage.ListAPIKeyPermissionProfilesForTenant(tenantID)
		for _, i := range indexes {
			effective := usage.EffectiveEndUserQuotaWithProfiles(items[i].AccountQuota(), profiles)
			enduser.ApplyEffectiveQuota(&items[i], effective)
			snapshot := snapshots[items[i].ID]
			used := snapshot.PeriodSpending()
			items[i].DailySpendingUsed = used.Day
			items[i].LifetimeSpendingUsed = used.Lifetime
			items[i].PeriodSpending = quota.BuildPeriodSpending(items[i].PeriodSpendingLimits, used)
			view := snapshot.UsageView(effective.AccessPolicy.ModelRules)
			// What the account's requests in flight on this node hold; the
			// admission checks count it as used.
			view.InFlight = inflight.Default().Held(inflight.AccountName(items[i].ID)).Usage()
			items[i].QuotaUsage = &view
			items[i].DailySpendingResetCount = resetCounts[items[i].ID]
		}
	}
	return nil
}

func decorateEndUser(user *enduser.User) error {
	items := []enduser.User{*user}
	if err := decorateEndUsers(items); err != nil {
		return err
	}
	*user = items[0]
	return nil
}

// writeAccessPolicyError 把访问策略相关的校验错误映射成结构化 400，返回是否已处理。
// details 里给出出错的位置与数值，面板据此用本地化文案就地提示。
func writeAccessPolicyError(c *gin.Context, err error) bool {
	var policyErr *quota.PolicyValidationError
	var targetErr *enduser.PolicyTargetNotAllowedError
	var cellErr *quota.QuotaLimitExceedsAccountError
	var profileErr *sqlapikey.ProfilePolicyError
	switch {
	case errors.As(err, &profileErr):
		details := gin.H{"profile": profileErr.ProfileID, "index": profileErr.Index}
		if errors.As(profileErr.Err, &policyErr) {
			details["field"] = policyErr.Field
		}
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"code": "access_policy_invalid", "message": err.Error(), "details": details,
		}})
	case errors.As(err, &policyErr):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"code": "access_policy_invalid", "message": policyErr.Error(), "details": gin.H{"field": policyErr.Field},
		}})
	case errors.As(err, &targetErr):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"code": "policy_target_not_allowed", "message": targetErr.Error(),
			"details": gin.H{"field": targetErr.Field, "model": targetErr.Model},
		}})
	case errors.As(err, &cellErr):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"code": "key_quota_limit_exceeds_account", "message": cellErr.Error(),
			"details": gin.H{"metric": cellErr.Metric, "period": cellErr.Window, "key_limit": cellErr.KeyLimit, "account_limit": cellErr.AccountLimit},
		}})
	case errors.Is(err, enduser.ErrQuotaLegacyConflict):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"code": "quota_limits_legacy_conflict", "message": "quota-limits conflicts with a legacy limit field: " + err.Error(),
		}})
	default:
		return false
	}
	return true
}

// keyMutationBody 是账号下 Key 的创建/修改请求体（管理端与门户共用）。
type keyMutationBody struct {
	Name                 *string                          `json:"name"`
	Disabled             *bool                            `json:"disabled"`
	DailySpendingLimit   *float64                         `json:"daily-spending-limit"`
	PeriodSpendingLimits *quota.PeriodSpendingLimitsPatch `json:"period-spending-limits"`
	QuotaLimits          *quota.MatrixPatch               `json:"quota-limits"`
	ModelRules           *[]quota.ModelRule               `json:"model-rules"`
}

func (b keyMutationBody) isEmpty() bool {
	return b.Name == nil && b.Disabled == nil && b.DailySpendingLimit == nil && b.PeriodSpendingLimits == nil &&
		b.QuotaLimits == nil && b.ModelRules == nil
}

// bindKeyMutation 解析请求体并把旧的日额度字段并入周期补丁；失败时已写好响应。
func bindKeyMutation(c *gin.Context) (keyMutationBody, enduser.KeyPatch, bool) {
	var body keyMutationBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return body, enduser.KeyPatch{}, false
	}
	periodPatch, err := quota.ResolveLegacyDay(body.DailySpendingLimit, body.PeriodSpendingLimits)
	if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
		endUserError(c, enduser.ErrPeriodDayLegacyConflict)
		return body, enduser.KeyPatch{}, false
	}
	if err != nil {
		endUserError(c, fmt.Errorf("%w: %v", enduser.ErrValidation, err))
		return body, enduser.KeyPatch{}, false
	}
	return body, enduser.KeyPatch{
		Name: body.Name, Disabled: body.Disabled, Period: periodPatch,
		QuotaLimits: body.QuotaLimits, ModelRules: body.ModelRules,
	}, true
}

// GetEndUserAPIKeySecret 按需返回账号下一把 Key 的明文，供面板「复制」使用。
// 以前面板为了复制要拉整个租户的 /api-key-entries（全部明文），现在只取一把。
func (h *Handler) GetEndUserAPIKeySecret(c *gin.Context) {
	principal, _ := principalFromContext(c)
	if !principal.Has("api_keys.read") && !principal.Has("end_users.read") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	secret, err := svc.ResolveOwnedKeySecret(c.Request.Context(), effectiveTenantID(c), c.Param("id"), c.Param("key_id"))
	if err != nil {
		endUserError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"id": c.Param("key_id"), "key": secret})
}

// PostEndUsersBatchPermissionProfile 把一批账号绑定到同一个权限模板。逐个账号设置
// 很繁琐，模板才是成批管理的入口；整批在一个事务里完成，鉴权快照只刷新一次。
func (h *Handler) PostEndUsersBatchPermissionProfile(c *gin.Context) {
	principal, _ := principalFromContext(c)
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		IDs                 []string `json:"ids"`
		PermissionProfileID string   `json:"permission-profile-id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	result, err := svc.BindPermissionProfile(c.Request.Context(), principal, effectiveTenantID(c), body.IDs, strings.TrimSpace(body.PermissionProfileID))
	if err != nil {
		endUserError(c, err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "cache_refresh_failed", "message": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, result)
}
