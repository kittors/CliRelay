package usage

// Response model of the management monitor center
// (GET /usage/monitor/overview and /usage/monitor/realtime).
//
// Totals, series and breakdowns come from usage_rollup_buckets so they cover
// the whole history; latency percentiles and recent failures need per-request
// rows and are limited to the request_logs retention window, which the
// response reports explicitly instead of letting the two silently disagree.

// MonitorTotals is one aggregate cell: a whole window, a series bucket or a
// breakdown row. Derived rates and averages are computed server-side so every
// consumer reads them the same way.
type MonitorTotals struct {
	Requests        int64   `json:"requests"`
	Success         int64   `json:"success"`
	Failed          int64   `json:"failed"`
	Streaming       int64   `json:"streaming"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	Cost            float64 `json:"cost"`
	// SuccessRate and CacheRate are percentages (0-100). SuccessRate is 0 when
	// there was no traffic; callers tell "idle" from "all failed" by Requests.
	SuccessRate float64 `json:"success_rate"`
	CacheRate   float64 `json:"cache_rate"`
	// Averages only count requests that reported the measurement: latency 0
	// means "not recorded", and first-token time only exists for streams.
	LatencyAvgMs    float64 `json:"latency_avg_ms"`
	FirstTokenAvgMs float64 `json:"first_token_avg_ms"`
	// OutputTokensPerSecond is generated (output + reasoning) tokens over the
	// total time of the requests that reported latency, first-token wait
	// included; it compares models, it is not a decode-speed benchmark.
	OutputTokensPerSecond float64 `json:"output_tokens_per_second"`
}

// MonitorSeriesPoint is one step of a continuous, zero-filled time series.
type MonitorSeriesPoint struct {
	// Start is the bucket start in the usage timezone, RFC 3339 with offset.
	Start string `json:"start"`
	MonitorTotals
}

// MonitorRange describes the resolved window so the panel never has to
// re-derive bucket edges or guess the server timezone.
type MonitorRange struct {
	Key           string `json:"key"`
	Start         string `json:"start"`
	End           string `json:"end"`
	PreviousStart string `json:"previous_start"`
	PreviousEnd   string `json:"previous_end"`
	StepSeconds   int64  `json:"step_seconds"`
	Timezone      string `json:"timezone"`
}

// MonitorBreakdownRow is one entry of the model / channel / consumer rankings.
type MonitorBreakdownRow struct {
	// Key is the filter value for this row (model name, channel name, or a
	// consumer selector such as "eu:<id>" / "key:<id>").
	Key   string `json:"key"`
	Label string `json:"label"`
	// Channel rows: provider and auth type for the provider badge.
	Provider string `json:"provider,omitempty"`
	AuthType string `json:"auth_type,omitempty"`
	// AuthSubjectID is a representative AI-account subject for a channel row;
	// the management layer uses it to resolve the live provider metadata.
	AuthSubjectID string `json:"-"`
	// Consumer rows: "end_user" or "api_key", plus a masked key hint for
	// keys without a name. Raw secrets are never returned.
	Kind    string `json:"kind,omitempty"`
	KeyHint string `json:"key_hint,omitempty"`
	MonitorTotals
	// Trend / TrendFailed are request counts aligned with the overview series;
	// only the top rows carry them.
	Trend       []int64 `json:"trend,omitempty"`
	TrendFailed []int64 `json:"trend_failed,omitempty"`
}

// MonitorBreakdown is a ranking plus how many distinct values exist in total,
// so the panel can say "showing 50 of 132".
type MonitorBreakdown struct {
	Rows  []MonitorBreakdownRow `json:"rows"`
	Total int                   `json:"total"`
}

// MonitorLatencyStats summarises one latency measurement over the window.
type MonitorLatencyStats struct {
	Samples int64   `json:"samples"`
	AvgMs   float64 `json:"avg_ms"`
	MaxMs   int64   `json:"max_ms"`
	P50Ms   float64 `json:"p50_ms"`
	P90Ms   float64 `json:"p90_ms"`
	P95Ms   float64 `json:"p95_ms"`
	P99Ms   float64 `json:"p99_ms"`
	// Histogram has len(MonitorLatency.BoundsMs)+1 cells: cell i counts values
	// below BoundsMs[i] (and at or above the previous bound); the last cell
	// counts everything at or above the last bound.
	Histogram []int64 `json:"histogram"`
}

// MonitorLatency carries total-duration and time-to-first-token distributions
// of successful requests.
type MonitorLatency struct {
	// CoverageStart is the oldest request_logs row the stats saw. Detail rows
	// expire before rollups do, so on long ranges this is later than the range
	// start and the panel says so.
	CoverageStart string              `json:"coverage_start,omitempty"`
	BoundsMs      []int64             `json:"bounds_ms"`
	Total         MonitorLatencyStats `json:"total"`
	FirstToken    MonitorLatencyStats `json:"first_token"`
}

// MonitorHeatmapCell counts requests for one weekday/hour slot.
type MonitorHeatmapCell struct {
	// Weekday follows time.Weekday: 0 = Sunday.
	Weekday  int   `json:"weekday"`
	Hour     int   `json:"hour"`
	Requests int64 `json:"requests"`
	Failed   int64 `json:"failed"`
}

// MonitorHeatmap is the weekday x hour activity matrix (non-zero cells only).
type MonitorHeatmap struct {
	Start string               `json:"start"`
	Days  int                  `json:"days"`
	Cells []MonitorHeatmapCell `json:"cells"`
}

// MonitorFlowNode / MonitorFlowLink form the consumer -> model -> channel
// Sankey graph. Low-volume values fold into one "other" node per layer.
type MonitorFlowNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Layer string `json:"layer"`
}

type MonitorFlowLink struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

type MonitorFlowGraph struct {
	Nodes []MonitorFlowNode `json:"nodes"`
	Links []MonitorFlowLink `json:"links"`
}

// MonitorFailure is one recent failed request, enough to open its error detail.
type MonitorFailure struct {
	ID        int64  `json:"id"`
	Timestamp string `json:"timestamp"`
	Model     string `json:"model"`
	Channel   string `json:"channel"`
	Consumer  string `json:"consumer"`
	LatencyMs int64  `json:"latency_ms"`
	Streaming bool   `json:"streaming"`
}

// MonitorFilterOption is one selectable filter value seen in the window.
type MonitorFilterOption struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Requests int64  `json:"requests"`
	Provider string `json:"provider,omitempty"`
	Kind     string `json:"kind,omitempty"`
	// AuthSubjectID mirrors MonitorBreakdownRow.AuthSubjectID for channels.
	AuthSubjectID string `json:"-"`
}

// MonitorFilterOptions lists filter values regardless of the active filters,
// so narrowing one dimension never hides the way back.
type MonitorFilterOptions struct {
	Models    []MonitorFilterOption `json:"models"`
	Channels  []MonitorFilterOption `json:"channels"`
	Consumers []MonitorFilterOption `json:"consumers"`
}

// MonitorSummary compares the window with the equally long window before it.
type MonitorSummary struct {
	Current  MonitorTotals `json:"current"`
	Previous MonitorTotals `json:"previous"`
}

// MonitorOverview is the full monitor center payload.
type MonitorOverview struct {
	GeneratedAt    string               `json:"generated_at"`
	Range          MonitorRange         `json:"range"`
	Summary        MonitorSummary       `json:"summary"`
	Series         []MonitorSeriesPoint `json:"series"`
	Latency        MonitorLatency       `json:"latency"`
	Models         MonitorBreakdown     `json:"models"`
	Channels       MonitorBreakdown     `json:"channels"`
	Consumers      MonitorBreakdown     `json:"consumers"`
	Heatmap        MonitorHeatmap       `json:"heatmap"`
	Flows          MonitorFlowGraph     `json:"flows"`
	RecentFailures []MonitorFailure     `json:"recent_failures"`
	Filters        MonitorFilterOptions `json:"filters"`
}

// MonitorRealtime is the last hour at one-minute resolution for the live strip.
type MonitorRealtime struct {
	GeneratedAt string               `json:"generated_at"`
	Timezone    string               `json:"timezone"`
	Points      []MonitorSeriesPoint `json:"points"`
	// CurrentRPM / CurrentTPM read the last completed minute; the minute in
	// progress is still in Points but would under-report a rate.
	CurrentRPM int64   `json:"current_rpm"`
	CurrentTPM int64   `json:"current_tpm"`
	PeakRPM    int64   `json:"peak_rpm"`
	PeakTPM    int64   `json:"peak_tpm"`
	AvgRPM     float64 `json:"avg_rpm"`
	AvgTPM     float64 `json:"avg_tpm"`
	// Last5m covers the last five minutes including the one in progress.
	Last5m MonitorTotals `json:"last_5m"`
}
