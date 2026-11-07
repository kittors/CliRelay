package quota

import "strings"

// 一次请求的模型策略决议：先按路由改写模型，再检查命中的按模型规则，规则触顶
// 且配置了降级模型时改用降级模型并重新检查。全部是纯逻辑，用量通过回调惰性读取，
// 网关（HTTP 与 WebSocket 每一轮）和测试共用同一份实现。

// RuleScope 标识规则来自账号（全部 Key 合计）还是单个 Key。
type RuleScope string

const (
	RuleScopeAccount RuleScope = "account"
	RuleScopeKey     RuleScope = "key"
)

// ScopedRules 是一层规则以及读取该层用量的方式。
type ScopedRules struct {
	Scope RuleScope
	Rules []ModelRule
	// Usage 返回该层命中 patterns 的模型在各窗口的用量。只有请求确实命中规则时
	// 才会被调用，没有命中规则的请求不产生任何用量查询。
	Usage func(patterns []string) (Matrix, error)
}

// RuleExceeded 描述一条触顶的规则。
type RuleExceeded struct {
	Scope   RuleScope
	Rule    ModelRule
	Model   string
	Metric  Metric
	Window  Period
	Limit   float64
	Current float64
}

// FallbackStep 记录一次超额降级。
type FallbackStep struct {
	From     string
	To       string
	Exceeded RuleExceeded
}

// ModelPolicyDecision 是决议结果。Exceeded 非 nil 表示请求应被拒绝。
type ModelPolicyDecision struct {
	RequestedModel string
	RoutedModel    string
	FinalModel     string
	Route          *ModelRoute
	Fallbacks      []FallbackStep
	Exceeded       *RuleExceeded
}

// Rewritten 报告最终模型是否不同于请求模型，即网关是否需要改写请求。
func (d ModelPolicyDecision) Rewritten() bool {
	return d.FinalModel != "" && d.FinalModel != d.RequestedModel
}

// MaxFallbackHops 限制连续降级的次数。A→B→C 已足够表达常见的分级（Opus→Sonnet→Haiku），
// 更长的链多半是配置错误，继续跟下去只会让请求在几个模型之间来回跳。
const MaxFallbackHops = 3

// ResolveModelPolicy 决议一次请求最终使用的模型。layers 的顺序就是检查顺序，
// 网关按「Key 在前、账号在后」传入：Key 规则更具体，触顶时优先报告它。
func ResolveModelPolicy(requested string, routes []ModelRoute, layers []ScopedRules) (ModelPolicyDecision, error) {
	decision := ModelPolicyDecision{RequestedModel: requested}
	routed, route := ResolveModelRoute(routes, requested)
	decision.RoutedModel = routed
	decision.Route = route
	current := routed
	visited := map[string]struct{}{NormalizeModelName(current): {}}
	for hop := 0; ; hop++ {
		exceeded, fallback, err := firstExceededRule(current, layers)
		if err != nil {
			decision.FinalModel = current
			return decision, err
		}
		if exceeded == nil {
			decision.FinalModel = current
			return decision, nil
		}
		next := ""
		if fallback != nil {
			next = strings.TrimSpace(fallback.Rule.FallbackModel)
		}
		_, seen := visited[NormalizeModelName(next)]
		if next == "" || seen || hop >= MaxFallbackHops {
			decision.FinalModel = current
			decision.Exceeded = exceeded
			return decision, nil
		}
		decision.Fallbacks = append(decision.Fallbacks, FallbackStep{From: current, To: next, Exceeded: *fallback})
		visited[NormalizeModelName(next)] = struct{}{}
		current = next
	}
}

// firstExceededRule 返回 model 命中的第一条触顶规则，以及第一条触顶且配置了降级的规则。
// 两者可能不同：若另一条无降级的规则也触顶，降级后的模型会在下一轮被它再次拦下。
func firstExceededRule(model string, layers []ScopedRules) (*RuleExceeded, *RuleExceeded, error) {
	var first, fallback *RuleExceeded
	for _, layer := range layers {
		matching := MatchingRules(layer.Rules, model)
		if len(matching) == 0 {
			continue
		}
		for _, rule := range matching {
			if layer.Usage == nil {
				continue
			}
			used, err := layer.Usage(rule.Models)
			if err != nil {
				return nil, nil, err
			}
			cell, ok := firstExceededCell(rule.Limits, used)
			if !ok {
				continue
			}
			exceeded := &RuleExceeded{
				Scope: layer.Scope, Rule: rule, Model: model,
				Metric: cell.Metric, Window: cell.Window, Limit: cell.Value, Current: used.Value(cell.Metric, cell.Window),
			}
			if first == nil {
				first = exceeded
			}
			if fallback == nil && strings.TrimSpace(rule.FallbackModel) != "" {
				fallback = exceeded
			}
		}
	}
	return first, fallback, nil
}

// FirstExceededCell 返回 used 在 limits 中第一个触顶（used ≥ limit）的格子。
func FirstExceededCell(limits, used Matrix) (Cell, bool) {
	return firstExceededCell(limits, used)
}

func firstExceededCell(limits, used Matrix) (Cell, bool) {
	for _, cell := range limits.ConfiguredCells() {
		if used.Value(cell.Metric, cell.Window) >= cell.Value {
			return cell, true
		}
	}
	return Cell{}, false
}
