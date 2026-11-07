// Package accesspolicy 保存每把 API Key 在网关上生效的结构化访问策略：按窗口的
// 请求/Token 额度、按模型规则与模型路由。
//
// 鉴权快照（internal/access/config_access）重建 Key 表时一并算出全部策略并整体发布，
// 准入中间件按请求携带的 Key 以 O(1) 查表，不在请求路径上解析 JSON 或查库读配置。
// 没有使用这些能力的 Key 不进表，它们的请求不会因此多做任何事情。
package accesspolicy

import (
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// Policy 是一把 Key 的生效策略。所有字段在发布后只读，可被并发请求共享。
type Policy struct {
	TenantID  string
	APIKeyID  string
	EndUserID string
	// AccountLimits 是账号（模板覆盖后）的总额度矩阵，只对账号下的 Key 有意义。
	AccountLimits quota.Matrix
	// KeyLimits 是 Key 的总额度矩阵：账号下的 Key 是子额度，独立 Key 是自身额度。
	KeyLimits    quota.Matrix
	AccountRules []quota.ModelRule
	KeyRules     []quota.ModelRule
	// Routes 是生效的模型路由（账号/模板层；独立 Key 来自它绑定的模板）。
	Routes []quota.ModelRoute
}

// Owned 报告 Key 是否属于某个门户账号。
func (p *Policy) Owned() bool { return p != nil && p.EndUserID != "" }

// HasModelPolicy 报告是否需要读取请求模型：有路由或有按模型规则。
func (p *Policy) HasModelPolicy() bool {
	return p != nil && (len(p.Routes) > 0 || len(p.AccountRules) > 0 || len(p.KeyRules) > 0)
}

// AccountPolicyCells 是账号矩阵中由访问策略承载、需要由新准入逻辑检查的格子；
// 固定列上的格子由原有的额度检查负责，这里不重复检查。
func (p *Policy) AccountPolicyCells() []quota.Cell {
	if !p.Owned() {
		return nil
	}
	return quota.AccountColumnLayout.StripColumnCells(p.AccountLimits).ConfiguredCells()
}

// KeyPolicyCells 是 Key 矩阵中由访问策略承载的格子。
func (p *Policy) KeyPolicyCells() []quota.Cell {
	if p == nil {
		return nil
	}
	layout := quota.StandaloneKeyColumnLayout
	if p.Owned() {
		layout = quota.OwnedKeyColumnLayout
	}
	return layout.StripColumnCells(p.KeyLimits).ConfiguredCells()
}

// Empty 报告策略是否没有任何需要网关执行的内容。
func (p *Policy) Empty() bool {
	return p == nil || (!p.HasModelPolicy() && len(p.AccountPolicyCells()) == 0 && len(p.KeyPolicyCells()) == 0)
}

type snapshot struct {
	byKey map[string]*Policy
}

var current atomic.Pointer[snapshot]

// Publish 以 Key 明文为索引整体替换策略表。空策略不会进表。
func Publish(byKey map[string]*Policy) {
	table := make(map[string]*Policy, len(byKey))
	for key, policy := range byKey {
		key = strings.TrimSpace(key)
		if key == "" || policy.Empty() {
			continue
		}
		table[key] = policy
	}
	current.Store(&snapshot{byKey: table})
}

// Lookup 返回 apiKey 的生效策略；没有任何策略时返回 nil。
func Lookup(apiKey string) *Policy {
	snap := current.Load()
	if snap == nil {
		return nil
	}
	return snap.byKey[strings.TrimSpace(apiKey)]
}
