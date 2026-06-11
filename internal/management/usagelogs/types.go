package usagelogs

import "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"

type ManagementLogQueryInput struct {
	Page            int
	Size            int
	Days            int
	APIKeys         []string
	Models          []string
	Statuses        []string
	Channels        []string
	MatchNoAPIKeys  bool
	MatchNoModels   bool
	MatchNoStatuses bool
	MatchNoChannels bool
}

type PublicLogQueryInput struct {
	APIKey string
	Model  string
	Status string
	Page   int
	Size   int
	Days   int
}

type LogContentResponse struct {
	Status      int
	ContentType string
	Headers     map[string]string
	Payload     any
	Text        string
}

type AuthFileGroupTrendResponse struct {
	Days        int                     `json:"days"`
	Group       string                  `json:"group"`
	Points      []usage.DailyCountPoint `json:"points"`
	QuotaPoints []usage.DailyQuotaPoint `json:"quota_points"`
}

type AuthFileTrendResponse struct {
	AuthIndex         string                      `json:"auth_index"`
	Days              int                         `json:"days"`
	Hours             int                         `json:"hours"`
	RequestTotal      int64                       `json:"request_total"`
	CycleRequestTotal int64                       `json:"cycle_request_total"`
	CycleCostTotal    float64                     `json:"cycle_cost_total"`
	WeeklyQuotaUsed   *float64                    `json:"weekly_quota_used_percent"`
	CycleKnown        bool                        `json:"cycle_known"`
	CycleStart        string                      `json:"cycle_start"`
	DailyUsage        []usage.DailyUsagePoint     `json:"daily_usage"`
	HourlyUsage       []usage.HourlyUsagePoint    `json:"hourly_usage"`
	QuotaSeries       []usage.QuotaSnapshotSeries `json:"quota_series"`
}
