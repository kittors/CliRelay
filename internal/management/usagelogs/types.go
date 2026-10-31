package usagelogs

import "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"

type ManagementLogQueryInput struct {
	Page int
	Size int
	Days int
	// APIKeyIDs selects keys by stable id as the key facet offers them (an
	// account's representative id expands to the whole account; "__system__"
	// selects system calls).
	APIKeyIDs []string
	// APIKeys are selectors the caller already holds (secrets). They are resolved
	// to key ids and never compared with the stored column.
	APIKeys []string
	// EndUserID 按门户账号过滤（账号名下全部 Key 的稳定 ID，含已删除 Key 的历史日志）。
	// 面板查看某个账号的用量时不必再拉取这些 Key 的明文、把它们放进查询串。
	EndUserID string
	Models    []string
	Statuses  []string
	Channels  []string
	// GuardKinds selects the requests loop protection ended ("reply_loop",
	// "tool_loop"); empty selects all.
	GuardKinds       []string
	MatchNoAPIKeys   bool
	MatchNoAPIKeyIDs bool
	MatchNoModels    bool
	MatchNoStatuses  bool
	MatchNoChannels  bool
}

type PublicLogQueryInput struct {
	APIKey           string
	EndUserID        string
	APIKeyIDs        []string
	Models           []string
	Channels         []string
	Statuses         []string
	MatchNoAPIKeyIDs bool
	MatchNoModels    bool
	MatchNoChannels  bool
	MatchNoStatuses  bool
	Page             int
	Size             int
	Days             int
}

type LogContentResponse struct {
	Status      int
	ContentType string
	Headers     map[string]string
	Payload     any
	Text        string
}

type AuthFileGroupTrendResponse struct {
	Days        int                      `json:"days"`
	Group       string                   `json:"group"`
	Points      []usage.DailyCountPoint  `json:"points"`
	QuotaPoints []usage.DailyQuotaPoint  `json:"quota_points"`
	QuotaSeries []usage.DailyQuotaSeries `json:"quota_series"`
}

type AuthFileTrendResponse struct {
	AuthIndex         string  `json:"auth_index"`
	Days              int     `json:"days"`
	Hours             int     `json:"hours"`
	RequestTotal      int64   `json:"request_total"`
	CycleRequestTotal int64   `json:"cycle_request_total"`
	CycleCostTotal    float64 `json:"cycle_cost_total"`
	CycleTotalTokens  int64   `json:"cycle_total_tokens"`
	// WeeklyQuotaUsed is the pool-wide share consumed this cycle — what the
	// account has spent in total, including surfaces the proxy does not serve.
	WeeklyQuotaUsed *float64 `json:"weekly_quota_used_percent"`
	// ProjectionQuotaUsed is the share consumed by traffic this proxy forwards,
	// and the only correct divisor for the weekly-budget projection: the cost
	// it is divided into covers those requests and no others. Equal to
	// WeeklyQuotaUsed for providers that bill one pool per surface; smaller for
	// xAI, whose weekly pool is shared with Grok Chat and friends. Nil when
	// upstream reported no attributable window, and callers fall back to
	// WeeklyQuotaUsed rather than dropping the projection.
	ProjectionQuotaUsed *float64 `json:"projection_quota_used_percent"`
	// ProjectionQuotaAttributable tells the panel the two percentages above can
	// legitimately differ, so it can say so instead of looking self-inconsistent.
	ProjectionQuotaAttributable bool                        `json:"projection_quota_attributable"`
	CycleKnown                  bool                        `json:"cycle_known"`
	CycleStart                  string                      `json:"cycle_start"`
	DailyUsage                  []usage.DailyUsagePoint     `json:"daily_usage"`
	HourlyUsage                 []usage.HourlyUsagePoint    `json:"hourly_usage"`
	QuotaSeries                 []usage.QuotaSnapshotSeries `json:"quota_series"`
}
