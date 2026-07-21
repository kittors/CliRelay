package usage

import "time"

// LogRow represents a single request log entry returned by QueryLogs.
type LogRow struct {
	ID                  int64     `json:"id"`
	Timestamp           time.Time `json:"timestamp"`
	APIKey              string    `json:"api_key"`
	APIKeyID            string    `json:"api_key_id,omitempty"`
	APIKeyMasked        string    `json:"api_key_masked,omitempty"`
	APIKeyName          string    `json:"api_key_name"`
	APIKeyOwnName       string    `json:"api_key_own_name,omitempty"`
	EndUserDisplayName  string    `json:"end_user_display_name,omitempty"`
	Model               string    `json:"model"`
	UpstreamModel       string    `json:"upstream_model,omitempty"`
	VisionFallbackModel string    `json:"vision_fallback_model,omitempty"`
	Source              string    `json:"source"`
	ChannelName         string    `json:"channel_name"`
	Provider            string    `json:"provider,omitempty"`
	AuthType            string    `json:"auth_type,omitempty"` // "oauth" | "api"
	AuthIndex           string    `json:"auth_index"`
	AuthSubjectID       string    `json:"auth_subject_id,omitempty"`
	Failed              bool      `json:"failed"`
	Streaming           bool      `json:"streaming"`
	LatencyMs           int64     `json:"latency_ms"`
	FirstTokenMs        int64     `json:"first_token_ms"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	ReasoningTokens     int64     `json:"reasoning_tokens"`
	CachedTokens        int64     `json:"cached_tokens"`
	TotalTokens         int64     `json:"total_tokens"`
	Cost                float64   `json:"cost"`
	HasContent          bool      `json:"has_content"`
}

// LogQueryParams holds filter/pagination parameters for QueryLogs.
type LogQueryParams struct {
	TenantID         string
	EndUserID        string   // stable account owner; matches all current key ids plus legacy raw-secret rows
	Page             int      // 1-based
	Size             int      // rows per page
	Days             int      // time range in days
	APIKey           string   // exact match filter (deprecated, use APIKeys)
	Model            string   // exact match filter (deprecated, use Models)
	Status           string   // "success", "failed", or "" (all) (deprecated, use Statuses)
	APIKeys          []string // multi-value API key filter
	APIKeyIDs        []string // multi-value stable key id filter (public key facet)
	Models           []string // multi-value model filter
	Statuses         []string // multi-value status filter
	MatchNoAPIKeys   bool     // explicit empty API key filter
	MatchNoAPIKeyIDs bool     // explicit empty stable key id filter
	MatchNoModels    bool     // explicit empty model filter
	MatchNoStatuses  bool     // explicit empty status filter
	MatchNoChannels  bool     // explicit empty channel filter
	AuthSubjectIDs   []string // optional auth_subject_id IN (...) filter (account-level)
	AuthIndexes      []string // optional auth_index IN (...) filter (credential instance)
	ChannelNames     []string // optional channel_name IN (...) filter
	// Optional precise legacy matches for renamed auth channels whose stored
	// channel_name was a shared provider/source value.
	AuthIndexChannelNames map[string][]string
}

// LogQueryResult holds the paginated query result.
type LogQueryResult struct {
	Items []LogRow `json:"items"`
	Total int64    `json:"total"`
	Page  int      `json:"page"`
	Size  int      `json:"size"`
}

// FilterOptions holds the available filter values for the UI.
type FilterOptions struct {
	APIKeys      []string          `json:"api_keys"`
	APIKeyNames  map[string]string `json:"api_key_names"`
	APIKeyCounts map[string]int64  `json:"api_key_counts"`
	// APIKeyIDs / APIKeyIDNames / APIKeyIDCounts are public-safe key facets keyed by stable id.
	APIKeyIDs      []string          `json:"api_key_ids,omitempty"`
	APIKeyIDNames  map[string]string `json:"api_key_id_names,omitempty"`
	APIKeyIDCounts map[string]int64  `json:"api_key_id_counts,omitempty"`
	Models         []string          `json:"models"`
	// Channels is a legacy plain-name list kept for older clients.
	// Prefer ChannelOptions when both are present.
	Channels       []string              `json:"channels"`
	ChannelOptions []ChannelFilterOption `json:"channel_options,omitempty"`
	Statuses       []string              `json:"statuses"`
}

// ChannelFilterOption is one selectable channel in request-log filters.
// Value prefers auth_subject_id (account-level) when known; falls back to
// auth_index (credential instance) or display name for orphans.
type ChannelFilterOption struct {
	Value         string `json:"value"`
	Label         string `json:"label"`
	Provider      string `json:"provider,omitempty"`
	AuthType      string `json:"auth_type,omitempty"` // "oauth" | "api"
	AuthIndex     string `json:"auth_index,omitempty"`
	AuthSubjectID string `json:"auth_subject_id,omitempty"`
}

// LogStats holds aggregated stats over the filtered result set.
type LogStats struct {
	Total         int64   `json:"total"`
	SuccessRate   float64 `json:"success_rate"`
	TotalTokens   int64   `json:"total_tokens"`
	TotalSessions int64   `json:"total_sessions"`
	TotalCost     float64 `json:"total_cost"`
	CacheRate     float64 `json:"cache_rate"`
}

type ClearRequestLogsResult struct {
	DeletedLogs       int64 `json:"deleted_logs"`
	DeletedContents   int64 `json:"deleted_contents"`
	ClearedBodyRows   int64 `json:"cleared_body_rows"`
	ClearedDetailRows int64 `json:"cleared_detail_rows"`
	ClearedLegacyRows int64 `json:"cleared_legacy_rows"`
}
