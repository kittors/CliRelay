package usage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// DailySeriesPoint holds one day of aggregated usage data.
type DailySeriesPoint struct {
	Date         string `json:"date"`
	Requests     int    `json:"requests"`
	FailedReq    int    `json:"failed_requests"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// DailyHeatmapPoint holds one day of usage for the calendar heatmap.
type DailyHeatmapPoint struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Sessions int64   `json:"sessions"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

// ModelDistributionPoint holds aggregated usage data for a single model.
type ModelDistributionPoint struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// PublicAPIKeyDistributionPoint exposes stable per-key usage without secrets.
type PublicAPIKeyDistributionPoint struct {
	APIKeyID string `json:"api_key_id"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

type PublicChartData struct {
	DailySeries        []DailySeriesPoint
	HeatmapSeries      []DailyHeatmapPoint
	ModelDistribution  []ModelDistributionPoint
	APIKeyDistribution []PublicAPIKeyDistributionPoint
	Stats              LogStats
}

// QueryPublicChartData aggregates the public API-key lookup charts in one indexed pass.
func QueryPublicChartData(apiKey string, days int) (PublicChartData, error) {
	row := GetAPIKey(apiKey)
	if row != nil && strings.TrimSpace(row.EndUserID) != "" {
		return QueryPublicChartDataForEndUser(row.TenantID, row.EndUserID, days)
	}
	return queryPublicChartData(LogQueryParams{
		TenantID: ResolveAPIKeyTenant(apiKey),
		APIKeys:  ExpandPublicLookupAPIKeys(apiKey),
	}, days)
}

func QueryPublicChartDataForEndUser(tenantID, endUserID string, days int) (PublicChartData, error) {
	return queryPublicChartData(LogQueryParams{
		TenantID:  tenantID,
		EndUserID: endUserID,
	}, days)
}

func queryPublicChartData(params LogQueryParams, days int) (PublicChartData, error) {
	if getReadDB() == nil {
		return PublicChartData{
			DailySeries:        []DailySeriesPoint{},
			HeatmapSeries:      []DailyHeatmapPoint{},
			ModelDistribution:  []ModelDistributionPoint{},
			APIKeyDistribution: []PublicAPIKeyDistributionPoint{},
			Stats:              LogStats{CacheRate: 0},
		}, nil
	}
	if days < 1 {
		days = 7
	}
	const heatmapDays = 365
	tenantID := params.TenantID
	if tenantID == "" {
		tenantID = systemTenantID
	}
	filter, ok := rollupIdentityFilter(params)
	if !ok {
		return PublicChartData{
			DailySeries:        []DailySeriesPoint{},
			HeatmapSeries:      []DailyHeatmapPoint{},
			ModelDistribution:  []ModelDistributionPoint{},
			APIKeyDistribution: []PublicAPIKeyDistributionPoint{},
			Stats:              LogStats{CacheRate: 0},
		}, nil
	}
	filter.BucketKind = rollupBucketDay
	filter.BucketFrom = dayBucketFromDays(heatmapDays)
	points, err := queryRollupDailySeries(filter)
	if err != nil {
		return PublicChartData{}, err
	}
	statsStartDay := dayBucketFromDays(days)
	dailyByDate := make(map[string]*DailySeriesPoint)
	heatmapByDate := make(map[string]*DailyHeatmapPoint)
	var stats LogStats
	var effectiveInputTokens int64
	var cachedTokens int64
	var successCount int64
	for _, p := range points {
		heat := heatmapByDate[p.Date]
		if heat == nil {
			heat = &DailyHeatmapPoint{Date: p.Date}
			heatmapByDate[p.Date] = heat
		}
		heat.Requests += p.Requests
		heat.Tokens += p.TotalTokens
		heat.Cost += p.Cost
		// Sessions only available within detail retention; leave 0 outside that window.
		if p.Date < statsStartDay {
			continue
		}
		daily := dailyByDate[p.Date]
		if daily == nil {
			daily = &DailySeriesPoint{Date: p.Date}
			dailyByDate[p.Date] = daily
		}
		daily.Requests += int(p.Requests)
		daily.FailedReq += int(p.FailedRequests)
		daily.InputTokens += int(p.InputTokens)
		daily.OutputTokens += int(p.OutputTokens)
		stats.Total += p.Requests
		stats.TotalTokens += p.TotalTokens
		stats.TotalCost += p.Cost
		successCount += p.SuccessCount
		effectiveInputTokens += p.EffectiveInput
		cachedTokens += p.CachedTokens
	}
	models, err := queryRollupModelDistribution(rollupFilter{
		TenantID:   tenantID,
		BucketKind: rollupBucketDay,
		BucketFrom: statsStartDay,
		APIKeyIDs:  filter.APIKeyIDs,
		EndUserID:  filter.EndUserID,
	})
	if err != nil {
		return PublicChartData{}, err
	}
	apiKeyDistribution := []PublicAPIKeyDistributionPoint{}
	if filter.EndUserID != "" {
		apiKeyDistribution, err = queryRollupAPIKeyDistributionForEndUser(tenantID, filter.EndUserID, days)
		if err != nil {
			return PublicChartData{}, err
		}
	}
	if stats.Total > 0 {
		stats.SuccessRate = float64(successCount) / float64(stats.Total) * 100
	}
	stats.CacheRate = cacheRateFromTokenTotals(effectiveInputTokens, cachedTokens)
	// Distinct sessions remain detail-retention diagnostics only.
	if sessionsByDate, sessErr := querySessionSetsByDate(params); sessErr == nil {
		statsStart := LocalDayKeyAt(CutoffStartUTC(days))
		totalSessions := make(map[string]struct{})
		for dateKey, sessions := range sessionsByDate {
			if point := heatmapByDate[dateKey]; point != nil {
				point.Sessions = int64(len(sessions))
			}
			if dateKey >= statsStart {
				for sessionID := range sessions {
					totalSessions[sessionID] = struct{}{}
				}
			}
		}
		stats.TotalSessions = int64(len(totalSessions))
	}
	return PublicChartData{
		DailySeries:        sortedDailySeries(dailyByDate),
		HeatmapSeries:      sortedHeatmapSeries(heatmapByDate),
		ModelDistribution:  models,
		APIKeyDistribution: apiKeyDistribution,
		Stats:              stats,
	}, nil
}

func effectiveInputTokenTotal(inputTokens, cachedTokens int64) int64 {
	if cachedTokens > inputTokens {
		return inputTokens + cachedTokens
	}
	return inputTokens
}

func sortedDailySeries(points map[string]*DailySeriesPoint) []DailySeriesPoint {
	result := make([]DailySeriesPoint, 0, len(points))
	for _, point := range points {
		result = append(result, *point)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Date < result[j].Date
	})
	return result
}

func sortedHeatmapSeries(points map[string]*DailyHeatmapPoint) []DailyHeatmapPoint {
	result := make([]DailyHeatmapPoint, 0, len(points))
	for _, point := range points {
		result = append(result, *point)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Date < result[j].Date
	})
	return result
}

func sortedModelDistribution(points map[string]*ModelDistributionPoint) []ModelDistributionPoint {
	result := make([]ModelDistributionPoint, 0, len(points))
	for _, point := range points {
		result = append(result, *point)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Requests == result[j].Requests {
			return result[i].Model < result[j].Model
		}
		return result[i].Requests > result[j].Requests
	})
	return result
}

// QueryDailySeries returns per-day aggregated request count and token usage for a given API key.
func QueryDailySeries(apiKey string, days int) ([]DailySeriesPoint, error) {
	return QueryDailySeriesForTenant(systemTenantID, apiKey, days)
}

func QueryDailySeriesForTenant(tenantID, apiKey string, days int) ([]DailySeriesPoint, error) {
	if getReadDB() == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	// Use usage_rollup_buckets day keys (usageLoc / project timezone), not
	// date(timestamp,'localtime') which PostgreSQL compat rewrites to UTC.
	params := LogQueryParams{TenantID: tenantID, APIKey: apiKey, Days: days}
	filter, ok := rollupIdentityFilter(params)
	if !ok {
		return []DailySeriesPoint{}, nil
	}
	filter.BucketKind = rollupBucketDay
	filter.BucketFrom = dayBucketFromDays(days)
	points, err := queryRollupDailySeries(filter)
	if err != nil {
		return nil, err
	}
	result := make([]DailySeriesPoint, 0, len(points))
	for _, p := range points {
		result = append(result, DailySeriesPoint{
			Date:         p.Date,
			Requests:     int(p.Requests),
			FailedReq:    int(p.FailedRequests),
			InputTokens:  int(p.InputTokens),
			OutputTokens: int(p.OutputTokens),
		})
	}
	return result, nil
}

// QueryDailyHeatmapSeries returns sparse daily usage for the calendar heatmap.
func QueryDailyHeatmapSeries(apiKey string, days int) ([]DailyHeatmapPoint, error) {
	return QueryDailyHeatmapSeriesForTenant(systemTenantID, apiKey, days)
}

func QueryDailyHeatmapSeriesForTenant(tenantID, apiKey string, days int) ([]DailyHeatmapPoint, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 365
	}

	params := LogQueryParams{TenantID: tenantID, APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)

	q := `SELECT date(timestamp, 'localtime') as d,
	             COUNT(*) as reqs,
	             COALESCE(SUM(total_tokens),0),
	             COALESCE(SUM(cost),0)
	      FROM request_logs` + where + `
	      GROUP BY d ORDER BY d`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: daily heatmap query: %w", err)
	}
	defer rows.Close()

	sessionsByDate, err := querySessionCountsByDate(params)
	if err != nil {
		return nil, err
	}

	var result []DailyHeatmapPoint
	for rows.Next() {
		var p DailyHeatmapPoint
		if err := rows.Scan(&p.Date, &p.Requests, &p.Tokens, &p.Cost); err != nil {
			return nil, fmt.Errorf("usage: daily heatmap scan: %w", err)
		}
		p.Sessions = sessionsByDate[p.Date]
		result = append(result, p)
	}
	return result, rows.Err()
}

// QuerySessionCount returns the distinct count of session-like request detail IDs.
func QuerySessionCount(params LogQueryParams) (int64, error) {
	seenByDate, err := querySessionSetsByDate(params)
	if err != nil {
		return 0, err
	}
	seen := make(map[string]struct{})
	for _, sessions := range seenByDate {
		for sessionID := range sessions {
			seen[sessionID] = struct{}{}
		}
	}
	return int64(len(seen)), nil
}

func querySessionCountsByDate(params LogQueryParams) (map[string]int64, error) {
	seenByDate, err := querySessionSetsByDate(params)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(seenByDate))
	for dateKey, sessions := range seenByDate {
		counts[dateKey] = int64(len(sessions))
	}
	return counts, nil
}

func querySessionSetsByDate(params LogQueryParams) (map[string]map[string]struct{}, error) {
	db := getReadDB()
	if db == nil {
		return map[string]map[string]struct{}{}, nil
	}
	if params.Days < 1 {
		params.Days = 7
	}

	where, args := buildWhereClause(params)
	rows, err := db.Query(
		`SELECT date(logs.timestamp, 'localtime'), content.session_id
		   FROM (SELECT tenant_id, id, timestamp FROM request_logs`+where+`) logs
		   JOIN request_log_content content ON content.tenant_id = logs.tenant_id AND content.log_id = logs.id
		  WHERE content.session_id <> ''`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("usage: session query: %w", err)
	}
	defer rows.Close()

	seenByDate := make(map[string]map[string]struct{})
	for rows.Next() {
		var dateKey, sessionID string
		if err := rows.Scan(&dateKey, &sessionID); err != nil {
			return nil, fmt.Errorf("usage: session scan: %w", err)
		}
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			continue
		}
		if seenByDate[dateKey] == nil {
			seenByDate[dateKey] = make(map[string]struct{})
		}
		seenByDate[dateKey][sessionID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return seenByDate, nil
}

func extractSessionIDFromDetails(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	payload := gjson.Parse(detail)
	if !payload.Exists() {
		return ""
	}
	bestRank := 99
	bestValue := ""
	var walk func(gjson.Result, string)
	walk = func(value gjson.Result, key string) {
		rank := sessionDetailKeyRank(key)
		if rank < bestRank {
			if text := sessionDetailString(value); text != "" {
				bestRank = rank
				bestValue = text
			}
		}
		if bestRank == 0 {
			return
		}
		if value.Type == gjson.JSON {
			value.ForEach(func(childKey, childValue gjson.Result) bool {
				walk(childValue, childKey.String())
				return bestRank != 0
			})
		}
	}
	walk(payload, "")
	return bestValue
}

func sessionDetailString(value gjson.Result) string {
	if value.Type == gjson.String {
		return strings.TrimSpace(value.String())
	}
	if value.IsArray() {
		result := ""
		value.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				result = strings.TrimSpace(item.String())
			}
			return result == ""
		})
		return result
	}
	return ""
}

func sessionDetailKeyRank(key string) int {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(key)))
	switch normalized {
	// Rank 0: stable client session ids. Grok/xAI clients send X-Grok-Session-Id
	// rather than generic Session-Id; without it request_log_content.session_id
	// stays empty for the bulk of Grok traffic and session-level analytics break.
	case "session_id", "sessionid", "x_session_id", "x_sessionid",
		"x_grok_session_id", "x_grok_sessionid":
		return 0
	case "conversation_id", "conversationid", "x_conversation_id", "x_conversationid",
		"openai_conversation_id", "x_grok_conv_id", "x_grok_convid":
		return 1
	default:
		return 99
	}
}

// QueryModelDistribution returns request count and token usage grouped by model for a given API key.
func QueryModelDistribution(apiKey string, days int) ([]ModelDistributionPoint, error) {
	return QueryModelDistributionForTenant(systemTenantID, apiKey, days)
}

func QueryModelDistributionForTenant(tenantID, apiKey string, days int) ([]ModelDistributionPoint, error) {
	if getReadDB() == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	params := LogQueryParams{TenantID: tenantID, APIKey: apiKey, Days: days}
	filter, ok := rollupIdentityFilter(params)
	if !ok {
		return []ModelDistributionPoint{}, nil
	}
	filter.BucketKind = rollupBucketDay
	filter.BucketFrom = dayBucketFromDays(days)
	return queryRollupModelDistribution(filter)
}

// APIKeyDistributionPoint holds aggregated usage data for a single API key.
type APIKeyDistributionPoint struct {
	APIKey   string `json:"api_key"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// QueryAPIKeyDistribution returns request count and token usage grouped by api_key.
func QueryAPIKeyDistribution(days int) ([]APIKeyDistributionPoint, error) {
	return QueryAPIKeyDistributionForTenant(systemTenantID, days)
}

func QueryAPIKeyDistributionForTenant(tenantID string, days int) ([]APIKeyDistributionPoint, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	params := LogQueryParams{TenantID: tenantID, Days: days}
	where, args := buildWhereClause(params)
	currentByID := currentAPIKeyRowsByIDForTenant(tenantID)
	currentByKey := currentAPIKeyRowsByKeyForTenant(tenantID)

	// Group by id-or-raw so logs that predate api_key_id still contribute.
	// Post-scan we resolve raw secrets via the live key table and merge
	// id-group + raw-group of the same key into one distribution point.
	// Without that merge, monitor "API Key usage" shows duplicate names
	// (e.g. 袁蔚 16.1k + 袁蔚 177) for a single key.
	q := `SELECT
	             CASE
	               WHEN trim(coalesce(api_key_id, '')) <> '' THEN api_key_id
	               ELSE 'raw:' || api_key
	             END as logical_selector,
	             COALESCE(MAX(NULLIF(trim(coalesce(api_key_id, '')), '')), '') as logical_id,
	             MAX(api_key) as snapshot_key,
	             COALESCE(NULLIF(MAX(api_key_name),''), '') as snapshot_name,
	             COUNT(*) as reqs,
	             COALESCE(SUM(total_tokens),0)
	      FROM request_logs` + where + `
	      AND api_key != ''
	      GROUP BY logical_selector ORDER BY reqs DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: apikey distribution query: %w", err)
	}
	defer rows.Close()

	repByEndUser, nameByEndUser := accountFilterRepresentatives(tenantID)
	merged := make(map[string]*APIKeyDistributionPoint)
	order := make([]string, 0)
	for rows.Next() {
		var logicalSelector string
		var logicalID sql.NullString
		var snapshotKey string
		var snapshotName string
		var p APIKeyDistributionPoint
		if err := rows.Scan(&logicalSelector, &logicalID, &snapshotKey, &snapshotName, &p.Requests, &p.Tokens); err != nil {
			return nil, fmt.Errorf("usage: apikey distribution scan: %w", err)
		}
		p.APIKey = strings.TrimSpace(snapshotKey)
		p.Name = strings.TrimSpace(snapshotName)

		// Prefer stable id identity; fall back to exact raw-key match for
		// legacy rows that never received api_key_id backfill.
		var live *APIKeyRow
		if row, ok := currentByID[trimNullString(logicalID)]; ok {
			copy := row
			live = &copy
		} else if row, ok := currentByKey[p.APIKey]; ok {
			copy := row
			live = &copy
		}
		mergeKey := p.APIKey
		if live != nil {
			if trimmed := strings.TrimSpace(live.Key); trimmed != "" {
				p.APIKey = trimmed
				mergeKey = trimmed
			}
			if eu := strings.TrimSpace(live.EndUserID); eu != "" {
				// Collapse multi-key accounts into one distribution slice.
				mergeKey = "eu:" + eu
				if rep := strings.TrimSpace(repByEndUser[eu]); rep != "" {
					p.APIKey = rep
				}
				if label := strings.TrimSpace(nameByEndUser[eu]); label != "" {
					p.Name = label
				} else if label := ResolveAPIKeyDisplayName(live, p.Name); label != "" {
					p.Name = label
				}
			} else if label := ResolveAPIKeyDisplayName(live, p.Name); label != "" {
				p.Name = label
			}
		}
		if p.APIKey == "" {
			continue
		}

		if existing, ok := merged[mergeKey]; ok {
			existing.Requests += p.Requests
			existing.Tokens += p.Tokens
			if existing.Name == "" && p.Name != "" {
				existing.Name = p.Name
			}
			continue
		}
		point := p
		merged[mergeKey] = &point
		order = append(order, mergeKey)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]APIKeyDistributionPoint, 0, len(order))
	for _, key := range order {
		result = append(result, *merged[key])
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Requests != result[j].Requests {
			return result[i].Requests > result[j].Requests
		}
		return result[i].APIKey < result[j].APIKey
	})
	return result, nil
}

// HourlyTokenPoint holds token usage per hour for the last N hours.
type HourlyTokenPoint struct {
	Hour            string `json:"hour"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
}

// HourlyModelPoint holds model request counts per hour.
type HourlyModelPoint struct {
	Hour     string `json:"hour"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

// QueryHourlySeries returns per-hour token and model aggregates for the last N hours.
func QueryHourlySeries(apiKey string, hours int) ([]HourlyTokenPoint, []HourlyModelPoint, error) {
	return QueryHourlySeriesForTenant(systemTenantID, apiKey, hours)
}

func QueryHourlySeriesForTenant(tenantID, apiKey string, hours int) ([]HourlyTokenPoint, []HourlyModelPoint, error) {
	tenantID = normalizeTenantID(tenantID)
	db := getReadDB()
	if db == nil {
		return nil, nil, nil
	}
	if hours < 1 {
		hours = 24
	}

	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)

	// Build WHERE clause directly with the correct hourly cutoff.
	// Previously this used buildWhereClause + strings.Replace, but that failed
	// because buildWhereClause uses parameterised queries (? placeholders)
	// so the time value lives in args, not in the where string.
	conditions := []string{"tenant_id = ?", "timestamp >= ?"}
	args := []interface{}{tenantID, cutoff}
	if apiKey != "" {
		if identity := GetAPIKeyForTenant(tenantID, apiKey); identity != nil {
			conditions = append(conditions, "(api_key_id = ? OR (trim(coalesce(api_key_id, '')) = '' AND api_key = ?))")
			args = append(args, identity.ID, identity.Key)
		} else {
			conditions = append(conditions, "api_key = ?")
			args = append(args, apiKey)
		}
	}
	where := " WHERE " + strings.Join(conditions, " AND ")

	// query tokens by hour
	tokenQuery := `SELECT strftime('%Y-%m-%d %H:00', timestamp, 'localtime') as h,
	                      COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
	                      COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0), COALESCE(SUM(total_tokens),0)
	               FROM request_logs` + where + ` GROUP BY h ORDER BY h`
	tokenRows, err := db.Query(tokenQuery, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: hourly token query: %w", err)
	}
	defer tokenRows.Close()

	var tokens []HourlyTokenPoint
	for tokenRows.Next() {
		var p HourlyTokenPoint
		if err := tokenRows.Scan(&p.Hour, &p.InputTokens, &p.OutputTokens, &p.ReasoningTokens, &p.CachedTokens, &p.TotalTokens); err != nil {
			return nil, nil, fmt.Errorf("usage: hourly token scan: %w", err)
		}
		tokens = append(tokens, p)
	}

	// query models by hour
	modelQuery := `SELECT strftime('%Y-%m-%d %H:00', timestamp, 'localtime') as h, model, COUNT(*) as reqs
	               FROM request_logs` + where + ` AND model != '' GROUP BY h, model ORDER BY h`
	modelRows, err := db.Query(modelQuery, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: hourly model query: %w", err)
	}
	defer modelRows.Close()

	var models []HourlyModelPoint
	for modelRows.Next() {
		var p HourlyModelPoint
		if err := modelRows.Scan(&p.Hour, &p.Model, &p.Requests); err != nil {
			return nil, nil, fmt.Errorf("usage: hourly model scan: %w", err)
		}
		models = append(models, p)
	}

	return tokens, models, nil
}

// EntityStatPoint holds aggregated usage data for a single entity (source or auth_index).
type EntityStatPoint struct {
	EntityName  string  `json:"entity_name"`
	Requests    int64   `json:"requests"`
	Failed      int64   `json:"failed"`
	AvgLatency  float64 `json:"avg_latency"`
	TotalTokens int64   `json:"total_tokens"`
}

// QueryEntityStats returns aggregates grouped by a given column (e.g. "source" or "auth_index").
// Time range is derived from days logic.
func QueryEntityStats(apiKey string, days int, groupColumn string, entityNames []string) ([]EntityStatPoint, error) {
	return QueryEntityStatsForTenant(systemTenantID, apiKey, days, groupColumn, entityNames)
}

func QueryEntityStatsForTenant(tenantID, apiKey string, days int, groupColumn string, entityNames []string) ([]EntityStatPoint, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}
	if groupColumn != "source" && groupColumn != "auth_index" {
		return nil, fmt.Errorf("usage: invalid group column")
	}

	params := LogQueryParams{TenantID: tenantID, APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)
	entityNames = normalizeEntityStatFilters(entityNames)
	if len(entityNames) > 0 {
		placeholders := make([]string, 0, len(entityNames))
		for _, name := range entityNames {
			placeholders = append(placeholders, "?")
			args = append(args, name)
		}
		if where == "" {
			where = " WHERE " + groupColumn + " IN (" + strings.Join(placeholders, ",") + ")"
		} else {
			where += " AND " + groupColumn + " IN (" + strings.Join(placeholders, ",") + ")"
		}
	}

	q := fmt.Sprintf(`
		SELECT %s, COUNT(*), COALESCE(SUM(failed),0), COALESCE(AVG(latency_ms),0), COALESCE(SUM(total_tokens),0)
		FROM request_logs%s AND %s != ''
		GROUP BY %s ORDER BY COUNT(*) DESC
	`, groupColumn, where, groupColumn, groupColumn)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: entity stats query: %w", err)
	}
	defer rows.Close()

	var result []EntityStatPoint
	for rows.Next() {
		var p EntityStatPoint
		if err := rows.Scan(&p.EntityName, &p.Requests, &p.Failed, &p.AvgLatency, &p.TotalTokens); err != nil {
			return nil, fmt.Errorf("usage: entity stats scan: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func normalizeEntityStatFilters(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}
