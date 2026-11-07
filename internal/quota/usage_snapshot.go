package quota

import "time"

// UsageSnapshot 是一个主体（账号或 Key）在额度矩阵各格上的有效用量（已减去重置基线），
// 并按用量记录里的模型名保留拆分，供按模型规则在内存中按模式归集。准入判定、面板
// 展示与重置都读同一种快照，所以同一格只会有一个数。
type UsageSnapshot struct {
	Total               Matrix
	Models              map[string]Matrix
	FiveHourWindowStart time.Time
}

// MatchedUsage 返回命中任一模式的模型用量之和，即一条按模型规则当前的用量。
func (s UsageSnapshot) MatchedUsage(patterns []string) Matrix {
	out := Matrix{}
	for model, used := range s.Models {
		if MatchesAnyModelPattern(patterns, model) {
			out = out.Add(used)
		}
	}
	return out
}

// PeriodSpending 把快照投影回既有的美元周期用量结构。
func (s UsageSnapshot) PeriodSpending() PeriodSpendingUsage {
	return PeriodSpendingUsage{
		FiveHour:            s.Total.Cost.FiveHour,
		Day:                 s.Total.Cost.Day,
		Week:                s.Total.Cost.Week,
		Month:               s.Total.Cost.Month,
		Lifetime:            s.Total.Cost.Lifetime,
		FiveHourWindowStart: s.FiveHourWindowStart,
	}
}

// UsageView 组装面板用的用量视图：总额度各格的用量，以及每条规则命中模型的用量之和。
func (s UsageSnapshot) UsageView(rules []ModelRule) UsageView {
	items := make([]RuleUsage, 0, len(rules))
	for _, rule := range rules {
		items = append(items, RuleUsage{ID: rule.ID, Used: s.MatchedUsage(rule.Models)})
	}
	return NewUsageView(s.Total, items, s.FiveHourWindowStart)
}
