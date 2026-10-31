package quota

import "time"

// UsageView 是一个主体（账号或 Key）当前的用量视图，面板据此画出总额度矩阵与每条
// 按模型规则的使用进度。用量与准入判定读的是同一份快照，二者不会出现两个数。
type UsageView struct {
	Total Matrix      `json:"total"`
	Rules []RuleUsage `json:"rules,omitempty"`
	// 只有 5h 窗口由服务端锚定（首笔计费请求开窗，5 小时后整窗清零），所以只有它
	// 能给出确定的起止时刻；日历周期的边界由面板自行推导，这里不下发。
	FiveHourWindowStart *time.Time `json:"five-hour-window-start,omitempty"`
	FiveHourResetsAt    *time.Time `json:"five-hour-resets-at,omitempty"`
	// InFlight 是此刻在途、用量还没入账的请求的预留。准入检查把它算作已用，所以它解释了
	// 「已用没到上限却被拒」。只统计本节点；没有在途请求时不下发。
	InFlight *InFlightUsage `json:"in-flight,omitempty"`
}

// InFlightUsage 是在途请求的预留合计：请求数是准确的，token 与费用按该 Key 近期
// 已入账请求的平均值估算。
type InFlightUsage struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

// RuleUsage 是一条按模型规则当前的用量（命中规则的全部模型之和）。
type RuleUsage struct {
	ID   string `json:"id"`
	Used Matrix `json:"used"`
}

// NewUsageView 由总用量、按规则归集的用量与 5h 窗口起点组装视图。
func NewUsageView(total Matrix, rules []RuleUsage, fiveHourWindowStart time.Time) UsageView {
	view := UsageView{Total: total, Rules: rules}
	if !fiveHourWindowStart.IsZero() {
		start := fiveHourWindowStart.UTC()
		resets := start.Add(FiveHourWindowDuration)
		view.FiveHourWindowStart = &start
		view.FiveHourResetsAt = &resets
	}
	return view
}
