package middleware

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// ─── Access policy admission ────────────────────────────────────────────────
// 访问策略（internal/accesspolicy）在原有额度之上增加两类检查：
//
//   - 总额度矩阵中固定列以外的格子：按窗口的请求次数与 Token，以及账号下 Key 的
//     其余子额度。它们不依赖请求的模型，和原有额度一起在 checkBudgets 里检查；
//   - 按模型规则：要知道请求的模型才能判定，由 ModelPolicyMiddleware 在读到模型后
//     检查（见 model_policy.go）。
//
// 两类检查读同一份用量快照（每个主体每次准入最多一次库查询），快照读失败时与原有
// 检查一样先用不超过上限时长的旧读数，再不行就 503（宁可拒绝也不绕过付费额度）。

var (
	queryAccountSnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
		return quota.UsageSnapshot{}, errQuotaUsageUnavailable
	}
	queryKeySnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
		return quota.UsageSnapshot{}, errQuotaUsageUnavailable
	}
)

// InitQuotaSnapshotFuncs 注入按账号 / Key 读取用量快照的实现（参数为 tenantID 与主体 ID）。
func InitQuotaSnapshotFuncs(account, key func(tenantID, id string) (quota.UsageSnapshot, error)) {
	if account != nil {
		queryAccountSnapshotFunc = account
	}
	if key != nil {
		queryKeySnapshotFunc = key
	}
}

const (
	quotaWindowSnapshotAccount = "snapshot_account"
	quotaWindowSnapshotKey     = "snapshot_key"

	// policyUsageContextKey 让同一个 HTTP 请求里的总额度检查与模型策略检查共用快照。
	policyUsageContextKey = "cliproxy.policyUsage"
)

// policyUsage 为一次准入惰性读取并缓存账号与 Key 的用量快照。它只在单个请求
// （或单个 WebSocket 轮次）内使用，不跨请求共享，所以不需要加锁。
type policyUsage struct {
	policy  *accesspolicy.Policy
	account *quota.UsageSnapshot
	key     *quota.UsageSnapshot
}

func newPolicyUsage(policy *accesspolicy.Policy) *policyUsage {
	return &policyUsage{policy: policy}
}

func (u *policyUsage) accountSnapshot() (quota.UsageSnapshot, error) {
	if u.account != nil {
		return *u.account, nil
	}
	p := u.policy
	snapshot, err := readQuotaUsage(quotaWindowSnapshotAccount, "eu:"+p.TenantID+"/"+p.EndUserID, "account "+p.EndUserID, true,
		func() (quota.UsageSnapshot, error) { return queryAccountSnapshotFunc(p.TenantID, p.EndUserID) })
	if err != nil {
		return quota.UsageSnapshot{}, err
	}
	u.account = &snapshot
	return snapshot, nil
}

// fiveHourWindowStart is the 5h window anchor of the snapshot already read for
// scope, zero when none was read or the window is not open.
func (u *policyUsage) fiveHourWindowStart(scope quota.RuleScope) time.Time {
	switch {
	case scope == quota.RuleScopeAccount && u.account != nil:
		return u.account.FiveHourWindowStart
	case scope == quota.RuleScopeKey && u.key != nil:
		return u.key.FiveHourWindowStart
	}
	return time.Time{}
}

func (u *policyUsage) keySnapshot() (quota.UsageSnapshot, error) {
	if u.key != nil {
		return *u.key, nil
	}
	p := u.policy
	snapshot, err := readQuotaUsage(quotaWindowSnapshotKey, "key:"+p.TenantID+"/"+p.APIKeyID, "key "+p.APIKeyID, true,
		func() (quota.UsageSnapshot, error) { return queryKeySnapshotFunc(p.TenantID, p.APIKeyID) })
	if err != nil {
		return quota.UsageSnapshot{}, err
	}
	u.key = &snapshot
	return snapshot, nil
}

// hasPolicyCells 报告这把 Key 是否有需要新准入逻辑检查的总额度格子。
func (p *quotaPolicy) hasPolicyCells() bool {
	return p.access != nil && (len(p.access.AccountPolicyCells()) > 0 || len(p.access.KeyPolicyCells()) > 0)
}

// checkPolicyCells 检查访问策略承载的总额度格子。
func (p *quotaPolicy) checkPolicyCells() *quotaVerdict {
	if p.access == nil {
		return nil
	}
	usage := p.policyUsage()
	if cells := p.access.AccountPolicyCells(); len(cells) > 0 {
		used, err := usage.accountSnapshot()
		if err != nil {
			return quotaUsageUnavailableVerdict("account", "policy", p.access.TenantID, p.access.EndUserID, err)
		}
		if verdict := policyCellVerdict("account", cells, used.Total, used.FiveHourWindowStart); verdict != nil {
			return verdict
		}
	}
	if cells := p.access.KeyPolicyCells(); len(cells) > 0 {
		used, err := usage.keySnapshot()
		if err != nil {
			return quotaUsageUnavailableVerdict("key", "policy", p.access.TenantID, p.access.APIKeyID, err)
		}
		if verdict := policyCellVerdict("key", cells, used.Total, used.FiveHourWindowStart); verdict != nil {
			return verdict
		}
	}
	return nil
}

// policyUsage 返回本次准入的快照缓存；WebSocket 每一轮由 turnGate 重置，读到最新用量。
func (p *quotaPolicy) policyUsage() *policyUsage {
	if p.usage == nil {
		p.usage = newPolicyUsage(p.access)
	}
	return p.usage
}

func policyCellVerdict(scope string, cells []quota.Cell, used quota.Matrix, fiveHourStart time.Time) *quotaVerdict {
	for _, cell := range cells {
		current := used.Value(cell.Metric, cell.Window)
		if current < cell.Value {
			continue
		}
		code, message := matrixLimitMessage(scope, cell.Metric, cell.Window, cell.Value, current)
		return matrixQuotaVerdict(matrixVerdictInput{
			rejectedBy: "quota_limits", code: code, message: message,
			scope: scope, metric: cell.Metric, window: cell.Window, limit: cell.Value, current: current,
		}).withRetryAfter(windowResetWait(cell.Window, fiveHourStart, time.Now()))
	}
	return nil
}

var windowAdjectives = map[quota.Period]string{
	quota.PeriodFiveHour: "5-hour",
	quota.PeriodDay:      "daily",
	quota.PeriodWeek:     "weekly",
	quota.PeriodMonth:    "monthly",
	quota.PeriodLifetime: "lifetime",
}

func scopeLabel(scope string) string {
	if scope == "account" {
		return "Account"
	}
	return "Key"
}

func matrixLimitMessage(scope string, metric quota.Metric, window quota.Period, limit, current float64) (code, message string) {
	label, adjective := scopeLabel(scope), windowAdjectives[window]
	switch metric {
	case quota.MetricRequests:
		return "request_limit_exceeded", fmt.Sprintf("%s %s request limit exceeded: %s/%s requests used.",
			label, adjective, formatQuotaNumber(current), formatQuotaNumber(limit))
	case quota.MetricTokens:
		return "token_limit_exceeded", fmt.Sprintf("%s %s token limit exceeded: %s/%s tokens used.",
			label, adjective, formatQuotaNumber(current), formatQuotaNumber(limit))
	default:
		return "spending_limit_exceeded", fmt.Sprintf("%s %s spending limit exceeded: $%.2f of $%.2f used.",
			label, adjective, current, limit)
	}
}

func describeUsage(metric quota.Metric, window quota.Period, limit, current float64) string {
	adjective := windowAdjectives[window]
	switch metric {
	case quota.MetricRequests:
		return fmt.Sprintf("%s requests %s/%s", adjective, formatQuotaNumber(current), formatQuotaNumber(limit))
	case quota.MetricTokens:
		return fmt.Sprintf("%s tokens %s/%s", adjective, formatQuotaNumber(current), formatQuotaNumber(limit))
	default:
		return fmt.Sprintf("%s spending $%.2f of $%.2f", adjective, current, limit)
	}
}

type matrixVerdictInput struct {
	rejectedBy, code, message string
	scope                     string
	metric                    quota.Metric
	window                    quota.Period
	limit, current            float64
	// 按模型规则触顶时附带模型与规则，客户端据此知道是哪个模型、哪条规则拦下的。
	model, ruleID, ruleName string
}

// matrixQuotaVerdict 是总额度矩阵或按模型规则某一格触顶的 429，响应体与诊断头沿用
// 周期额度拒绝的形态，多带 metric（以及模型规则的 model / rule）。
func matrixQuotaVerdict(in matrixVerdictInput) *quotaVerdict {
	const errType = "rate_limit_exceeded"
	body := gin.H{
		"message": in.message, "type": errType, "code": in.code,
		"scope": in.scope, "period": in.window, "metric": in.metric,
	}
	headers := []quotaHeader{
		{"X-CliRelay-Quota-Code", in.code},
		{"X-CliRelay-Quota-Rejected-By", in.rejectedBy},
		{"X-CliRelay-Quota-Scope", in.scope},
		{"X-CliRelay-Quota-Period", string(in.window)},
		{"X-CliRelay-Quota-Metric", string(in.metric)},
		{"X-CliRelay-Quota-Limit", formatQuotaNumber(in.limit)},
		{"X-CliRelay-Quota-Current", formatQuotaNumber(in.current)},
	}
	if in.model != "" {
		body["model"] = in.model
		headers = append(headers, quotaHeader{"X-CliRelay-Quota-Model", in.model})
	}
	if in.ruleID != "" {
		body["rule"] = in.ruleID
		headers = append(headers, quotaHeader{"X-CliRelay-Quota-Rule", in.ruleID})
	}
	if in.ruleName != "" {
		body["rule_name"] = in.ruleName
	}
	return &quotaVerdict{
		status:  http.StatusTooManyRequests,
		body:    gin.H{"error": body},
		headers: headers,
		rejection: &quotaRejectionDiagnostics{
			rejectedBy: in.rejectedBy, limit: in.limit, current: in.current,
			code: in.code, errType: errType, message: in.message,
		},
	}
}

// modelRuleVerdict 渲染按模型规则触顶的拒绝。fiveHourStart 是同一份快照里的 5h 窗口锚点，
// 用来给 5h 窗口的规则算出 Retry-After。
func modelRuleVerdict(exceeded *quota.RuleExceeded, fiveHourStart time.Time) *quotaVerdict {
	scope := string(exceeded.Scope)
	name := strings.TrimSpace(exceeded.Rule.Name)
	ruleLabel := exceeded.Rule.ID
	if name != "" {
		ruleLabel = name
	}
	message := fmt.Sprintf("%s model quota exceeded for %s (rule %q): %s used.",
		scopeLabel(scope), exceeded.Model, ruleLabel, describeUsage(exceeded.Metric, exceeded.Window, exceeded.Limit, exceeded.Current))
	return matrixQuotaVerdict(matrixVerdictInput{
		rejectedBy: "model_quota", code: "model_quota_exceeded", message: message,
		scope: scope, metric: exceeded.Metric, window: exceeded.Window, limit: exceeded.Limit, current: exceeded.Current,
		model: exceeded.Model, ruleID: exceeded.Rule.ID, ruleName: name,
	}).withRetryAfter(windowResetWait(exceeded.Window, fiveHourStart, time.Now()))
}
