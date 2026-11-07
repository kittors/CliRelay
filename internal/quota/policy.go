package quota

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 访问策略（AccessPolicy）是账号、Key 与权限模板在固定额度列之外的扩展部分，
// 整体以 JSON 存放在各表的 access_policy 列中：
//
//   - Limits：固定列承载不了的总额度格子（请求次数与 Token 的多窗口等），
//     哪些格子在列里由 ColumnLayout 决定，同一格只有一个真源；
//   - ModelRules：按模型叠加的配额。命中规则的请求除了受总额度约束，还要在规则
//     自己的矩阵之内（「叠加」：两层都检查，任何一层触顶即拒绝）；
//   - ModelRoutes：把请求的模型改写成另一个模型（例如把 Opus 请求改用 Sonnet）。
//
// 规则与路由作为一个整体随所属对象一起读写：编辑时整体替换，和 allowed-models
// 等既有 JSON 列的语义一致，也让集群间的配置同步沿用既有的版本号机制。

const (
	// MaxModelRules / MaxModelRoutes 防止一个对象挂上成百上千条规则，把每次准入的
	// 匹配开销与面板渲染拖垮。真实场景里一个账号的模型规则很少超过十几条。
	MaxModelRules  = 50
	MaxModelRoutes = 50
	// MaxPatternsPerEntry 是一条规则或路由最多可写的模型模式数。
	MaxPatternsPerEntry = 100
	maxPatternLength    = 200
	maxRuleNameLength   = 80
	maxEntryIDLength    = 64
)

var (
	ErrInvalidAccessPolicy = errors.New("invalid access policy")
)

// ModelRule 是按模型叠加的一组限额。
type ModelRule struct {
	ID     string   `json:"id"`
	Name   string   `json:"name,omitempty"`
	Models []string `json:"models"`
	Limits Matrix   `json:"limits"`
	// FallbackModel 非空时，规则任一限额触顶后命中它的请求改用这个模型继续服务，
	// 而不是直接拒绝（例如 Opus 的周额度用完后自动改用 Sonnet）。降级后的模型
	// 重新经过全部规则与模型门控，见 ResolveModelPolicy。
	FallbackModel string `json:"fallback-model,omitempty"`
}

// ModelRoute 把命中 From 任一模式的请求改用模型 To。
type ModelRoute struct {
	ID   string   `json:"id"`
	From []string `json:"from"`
	To   string   `json:"to"`
}

// AccessPolicy 是 access_policy 列的内容。
type AccessPolicy struct {
	Limits      Matrix       `json:"limits"`
	ModelRules  []ModelRule  `json:"model-rules"`
	ModelRoutes []ModelRoute `json:"model-routes"`
}

// IsEmpty 报告策略是否没有任何生效内容。
func (p AccessPolicy) IsEmpty() bool {
	return p.Limits.IsZero() && len(p.ModelRules) == 0 && len(p.ModelRoutes) == 0
}

// Clone 深拷贝策略，避免多个账号共享同一个模板策略时互相改写切片。
func (p AccessPolicy) Clone() AccessPolicy {
	out := AccessPolicy{Limits: p.Limits}
	if len(p.ModelRules) > 0 {
		out.ModelRules = make([]ModelRule, len(p.ModelRules))
		for i, rule := range p.ModelRules {
			rule.Models = append([]string(nil), rule.Models...)
			out.ModelRules[i] = rule
		}
	}
	if len(p.ModelRoutes) > 0 {
		out.ModelRoutes = make([]ModelRoute, len(p.ModelRoutes))
		for i, route := range p.ModelRoutes {
			route.From = append([]string(nil), route.From...)
			out.ModelRoutes[i] = route
		}
	}
	return out
}

// PolicyValidationError 指出策略中出错的位置，管理端据此返回结构化错误。
type PolicyValidationError struct {
	// Field 是出错字段的路径，例如 "model-rules[2].models[0]"。
	Field   string
	Message string
}

func (e *PolicyValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func (e *PolicyValidationError) Unwrap() error { return ErrInvalidAccessPolicy }

func policyError(field, format string, args ...any) error {
	return &PolicyValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// ValidateModelPattern 校验规则或路由来源中的一个模型模式。
func ValidateModelPattern(pattern string) error {
	normalized := NormalizeModelPattern(pattern)
	switch {
	case normalized == "":
		return errors.New("model pattern is empty")
	case normalized == "*":
		// 「全部模型」用账号/Key 的总额度表达；单独一个 * 的规则与总额度完全重叠，
		// 却会让面板上出现两处看似独立、实则同源的额度。
		return errors.New(`"*" alone is not a model pattern; use the account-wide limits for all models`)
	case len(normalized) > maxPatternLength:
		return fmt.Errorf("model pattern exceeds %d characters", maxPatternLength)
	}
	return nil
}

// normalizePatterns 去空白、归一化、去重，保留首次出现的顺序。
func normalizePatterns(field string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, policyError(field, "at least one model is required")
	}
	if len(patterns) > MaxPatternsPerEntry {
		return nil, policyError(field, "at most %d models", MaxPatternsPerEntry)
	}
	out := make([]string, 0, len(patterns))
	seen := make(map[string]struct{}, len(patterns))
	for i, raw := range patterns {
		if err := ValidateModelPattern(raw); err != nil {
			return nil, policyError(fmt.Sprintf("%s[%d]", field, i), "%s", err.Error())
		}
		// 存储保留原始大小写，便于面板按管理员的写法回显；匹配时再归一化。
		trimmed := strings.TrimSpace(raw)
		key := NormalizeModelPattern(trimmed)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

func normalizeEntryID(field, id string, seen map[string]struct{}) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = newEntryID()
	}
	if len(id) > maxEntryIDLength {
		return "", policyError(field, "id exceeds %d characters", maxEntryIDLength)
	}
	if _, dup := seen[id]; dup {
		return "", policyError(field, "duplicate id %q", id)
	}
	seen[id] = struct{}{}
	return id, nil
}

func newEntryID() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "r-fallback"
	}
	return "r-" + hex.EncodeToString(raw)
}

// NormalizeAccessPolicy 校验并规范化策略。layout 决定哪些总额度格子由固定列承载，
// 这些格子在策略里被清零，保证同一格只有一个真源。
func NormalizeAccessPolicy(policy AccessPolicy, layout ColumnLayout) (AccessPolicy, error) {
	out := AccessPolicy{}
	limits, err := NormalizeMatrix(policy.Limits)
	if err != nil {
		return AccessPolicy{}, policyError("limits", "%s", err.Error())
	}
	out.Limits = layout.StripColumnCells(limits)

	if len(policy.ModelRules) > MaxModelRules {
		return AccessPolicy{}, policyError("model-rules", "at most %d rules", MaxModelRules)
	}
	ruleIDs := make(map[string]struct{}, len(policy.ModelRules))
	for i, rule := range policy.ModelRules {
		field := fmt.Sprintf("model-rules[%d]", i)
		id, err := normalizeEntryID(field+".id", rule.ID, ruleIDs)
		if err != nil {
			return AccessPolicy{}, err
		}
		name := strings.TrimSpace(rule.Name)
		if len([]rune(name)) > maxRuleNameLength {
			return AccessPolicy{}, policyError(field+".name", "name exceeds %d characters", maxRuleNameLength)
		}
		models, err := normalizePatterns(field+".models", rule.Models)
		if err != nil {
			return AccessPolicy{}, err
		}
		ruleLimits, err := NormalizeMatrix(rule.Limits)
		if err != nil {
			return AccessPolicy{}, policyError(field+".limits", "%s", err.Error())
		}
		if ruleLimits.IsZero() {
			// 没有任何限额的规则什么也不做，却会在面板上显示成一条「规则」，误导管理员
			// 以为该模型已被限制。
			return AccessPolicy{}, policyError(field+".limits", "a model rule needs at least one limit")
		}
		fallback := strings.TrimSpace(rule.FallbackModel)
		switch {
		case fallback == "":
		case strings.Contains(fallback, "*"):
			return AccessPolicy{}, policyError(field+".fallback-model", "fallback model must be a concrete model, not a pattern")
		case len(fallback) > maxPatternLength:
			return AccessPolicy{}, policyError(field+".fallback-model", "fallback model exceeds %d characters", maxPatternLength)
		case MatchesAnyModelPattern(models, fallback):
			// 降级目标仍命中本规则，额度用完后照样触顶，降级永远不会发生。
			return AccessPolicy{}, policyError(field+".fallback-model", "fallback model %q is matched by the rule itself", fallback)
		}
		out.ModelRules = append(out.ModelRules, ModelRule{ID: id, Name: name, Models: models, Limits: ruleLimits, FallbackModel: fallback})
	}

	if len(policy.ModelRoutes) > MaxModelRoutes {
		return AccessPolicy{}, policyError("model-routes", "at most %d routes", MaxModelRoutes)
	}
	routeIDs := make(map[string]struct{}, len(policy.ModelRoutes))
	for i, route := range policy.ModelRoutes {
		field := fmt.Sprintf("model-routes[%d]", i)
		id, err := normalizeEntryID(field+".id", route.ID, routeIDs)
		if err != nil {
			return AccessPolicy{}, err
		}
		from, err := normalizePatterns(field+".from", route.From)
		if err != nil {
			return AccessPolicy{}, err
		}
		to := strings.TrimSpace(route.To)
		switch {
		case to == "":
			return AccessPolicy{}, policyError(field+".to", "target model is required")
		case strings.Contains(to, "*"):
			return AccessPolicy{}, policyError(field+".to", "target model must be a concrete model, not a pattern")
		case len(to) > maxPatternLength:
			return AccessPolicy{}, policyError(field+".to", "target model exceeds %d characters", maxPatternLength)
		case MatchesAnyModelPattern(from, to):
			// 目标命中自己的来源：改写后的请求依然满足条件，规则没有意义，
			// 而且读起来像一个循环。
			return AccessPolicy{}, policyError(field+".to", "target model %q is matched by its own source patterns", to)
		}
		out.ModelRoutes = append(out.ModelRoutes, ModelRoute{ID: id, From: from, To: to})
	}
	return out, nil
}

// DecodeAccessPolicy 解析 access_policy 列。空值与 "{}" 是合法的空策略。
func DecodeAccessPolicy(raw string) (AccessPolicy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return AccessPolicy{}, nil
	}
	var policy AccessPolicy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return AccessPolicy{}, fmt.Errorf("%w: decode: %v", ErrInvalidAccessPolicy, err)
	}
	return policy, nil
}

// EncodeAccessPolicy 序列化策略；空策略存为 "{}"，让「从未配置」与「配置为空」在库里一致。
func EncodeAccessPolicy(policy AccessPolicy) string {
	if policy.IsEmpty() {
		return "{}"
	}
	if policy.ModelRules == nil {
		policy.ModelRules = []ModelRule{}
	}
	if policy.ModelRoutes == nil {
		policy.ModelRoutes = []ModelRoute{}
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// ResolveModelRoute 返回 model 命中的第一条路由的目标模型；没有命中时返回 model 本身。
// 路由只改写一次，不做链式解析：A→B、B→C 时请求 A 得到 B。链式解析会让一条看似
// 无关的路由悄悄改变另一条路由的结果，管理员很难从面板上看出最终会用到哪个模型。
func ResolveModelRoute(routes []ModelRoute, model string) (string, *ModelRoute) {
	if strings.TrimSpace(model) == "" {
		return model, nil
	}
	for i := range routes {
		if MatchesAnyModelPattern(routes[i].From, model) {
			return routes[i].To, &routes[i]
		}
	}
	return model, nil
}

// MatchingRules 返回 model 命中的全部规则（按配置顺序）。一个请求可以同时受多条
// 规则约束，例如「claude-*」与「claude-opus-*」各自的限额都要满足。
func MatchingRules(rules []ModelRule, model string) []ModelRule {
	if strings.TrimSpace(model) == "" || len(rules) == 0 {
		return nil
	}
	out := make([]ModelRule, 0, 1)
	for _, rule := range rules {
		if MatchesAnyModelPattern(rule.Models, model) {
			out = append(out, rule)
		}
	}
	return out
}

// ColumnLayout 描述某类对象的总额度矩阵中哪些格子落在既有的固定列上。
// 固定列早于访问策略存在，被重置、门户、CC Switch 用量卡片等大量路径直接读取；
// 它们继续作为那些格子的唯一真源，策略只承载其余格子。
type ColumnLayout struct {
	cells map[Metric]map[Period]bool
	// forbidden 是这类对象根本不拥有的格子：既不在列里，也不允许写进策略。
	forbidden map[Metric]map[Period]bool
}

func newColumnLayout(cells ...Cell) ColumnLayout {
	layout := ColumnLayout{cells: map[Metric]map[Period]bool{}, forbidden: map[Metric]map[Period]bool{}}
	for _, cell := range cells {
		if layout.cells[cell.Metric] == nil {
			layout.cells[cell.Metric] = map[Period]bool{}
		}
		layout.cells[cell.Metric][cell.Window] = true
	}
	return layout
}

func (l ColumnLayout) withForbidden(cells ...Cell) ColumnLayout {
	for _, cell := range cells {
		if l.forbidden[cell.Metric] == nil {
			l.forbidden[cell.Metric] = map[Period]bool{}
		}
		l.forbidden[cell.Metric][cell.Window] = true
	}
	return l
}

var (
	costPeriodCells = []Cell{
		{Metric: MetricCost, Window: PeriodFiveHour}, {Metric: MetricCost, Window: PeriodDay},
		{Metric: MetricCost, Window: PeriodWeek}, {Metric: MetricCost, Window: PeriodMonth},
	}
	// AccountColumnLayout：end_users 的美元四周期 + 累计消费（spending_limit）+
	// 每日请求（daily_limit）+ 总请求（total_quota）。
	AccountColumnLayout = newColumnLayout(append(append([]Cell(nil), costPeriodCells...),
		Cell{Metric: MetricCost, Window: PeriodLifetime},
		Cell{Metric: MetricRequests, Window: PeriodDay},
		Cell{Metric: MetricRequests, Window: PeriodLifetime})...)
	// ProfileColumnLayout：模板没有 spending_limit 列——累计消费是发给账号的额度，
	// 不是可复用的权限属性，所以模板的 cost×lifetime 恒为 0（见 ProfileOwnsCell）。
	ProfileColumnLayout = newColumnLayout(append(append([]Cell(nil), costPeriodCells...),
		Cell{Metric: MetricRequests, Window: PeriodDay},
		Cell{Metric: MetricRequests, Window: PeriodLifetime})...).
		withForbidden(Cell{Metric: MetricCost, Window: PeriodLifetime})
	// StandaloneKeyColumnLayout：未归属账号的 Key 和账号一样拥有全部固定列。
	StandaloneKeyColumnLayout = AccountColumnLayout
	// OwnedKeyColumnLayout：归属账号的 Key 只有美元四周期子额度列；其余固定列在
	// owned key 上始终为 0（由存储层清零，防止通过通用 Key 接口铸造独立预算），
	// 所以它的其它子额度格子全部放在策略里，经 owner 作用域接口校验后写入。
	OwnedKeyColumnLayout = newColumnLayout(costPeriodCells...)
)

// Has 报告某格是否由固定列承载。
func (l ColumnLayout) Has(metric Metric, window Period) bool {
	return l.cells[metric][window]
}

// Owns 报告这类对象是否拥有某格（列承载或策略承载）。
func (l ColumnLayout) Owns(metric Metric, window Period) bool {
	return !l.forbidden[metric][window]
}

// StripColumnCells 清零由固定列承载的格子与不拥有的格子，得到应写入策略的部分。
func (l ColumnLayout) StripColumnCells(m Matrix) Matrix {
	out := m
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			if l.Has(metric, window) || !l.Owns(metric, window) {
				out.Set(metric, window, 0)
			}
		}
	}
	return out
}

// OwnedCellsOnly 清零这类对象不拥有的格子。
func (l ColumnLayout) OwnedCellsOnly(m Matrix) Matrix {
	out := m
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			if !l.Owns(metric, window) {
				out.Set(metric, window, 0)
			}
		}
	}
	return out
}

// ColumnCellsOnly 只保留由固定列承载的格子。
func (l ColumnLayout) ColumnCellsOnly(m Matrix) Matrix {
	out := Matrix{}
	for _, metric := range OrderedMetrics {
		for _, window := range OrderedWindows {
			if l.Has(metric, window) {
				out.Set(metric, window, m.Value(metric, window))
			}
		}
	}
	return out
}

// Merge 把固定列的格子与策略里的格子合成完整矩阵。
func (l ColumnLayout) Merge(columns, policy Matrix) Matrix {
	return l.ColumnCellsOnly(columns).Add(l.StripColumnCells(policy))
}

// ProfileOwnsCell 报告模板是否拥有某格。模板不拥有累计消费：账号绑定模板后，
// 累计消费仍由账号自己的 spending_limit 决定。
func ProfileOwnsCell(metric Metric, window Period) bool {
	return ProfileColumnLayout.Owns(metric, window)
}

// WindowHasLimit 报告某个窗口是否配置了任何限额（总额度矩阵任一度量，或任一按模型
// 规则）。重置一个窗口会同时清零总额度与按模型规则的用量，只要其中有一个限额，
// 重置就有意义。
func WindowHasLimit(limits Matrix, rules []ModelRule, window Period) bool {
	for _, metric := range OrderedMetrics {
		if limits.Value(metric, window) > 0 {
			return true
		}
		for _, rule := range rules {
			if rule.Limits.Value(metric, window) > 0 {
				return true
			}
		}
	}
	return false
}
