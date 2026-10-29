package usage

import (
	"fmt"
	"strings"
	"time"
)

type rollupAgg struct {
	RequestCount    int64
	SuccessCount    int64
	FailureCount    int64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	TotalTokens     int64
	CostTotal       float64
	LatencySumMs    int64
	LatencyCount    int64
	Cache           cacheTotals
}

func (a rollupAgg) toLogStats() LogStats {
	var successRate float64
	if a.RequestCount > 0 {
		successRate = float64(a.SuccessCount) / float64(a.RequestCount) * 100
	}
	stats := LogStats{
		Total:       a.RequestCount,
		SuccessRate: successRate,
		TotalTokens: a.TotalTokens,
		TotalCost:   a.CostTotal,
		CacheUsage:  a.Cache.usage(),
	}
	stats.CacheRate = stats.legacyCacheRate()
	return stats
}

func (a rollupAgg) toDashboardKPI() DashboardKPI {
	kpi := DashboardKPI{
		TotalRequests:   a.RequestCount,
		SuccessRequests: a.SuccessCount,
		FailedRequests:  a.FailureCount,
		InputTokens:     a.InputTokens,
		OutputTokens:    a.OutputTokens,
		ReasoningTokens: a.ReasoningTokens,
		CachedTokens:    a.CachedTokens,
		TotalTokens:     a.TotalTokens,
		TotalCost:       a.CostTotal,
		CacheUsage:      a.Cache.usage(),
	}
	if a.RequestCount > 0 {
		kpi.SuccessRate = float64(a.SuccessCount) / float64(a.RequestCount) * 100
	}
	kpi.CacheRate = kpi.legacyCacheRate()
	return kpi
}

type rollupFilter struct {
	TenantID       string
	BucketKind     string
	BucketFrom     string // inclusive day/hour/minute key; empty = no lower bound
	BucketTo       string // exclusive optional
	APIKeyIDs      []string
	EndUserID      string
	AuthSubjectID  string
	AuthSubjectIDs []string
	Models         []string
	Sources        []string
	ChannelNames   []string
}

func queryRollupAgg(filter rollupFilter) (rollupAgg, error) {
	db := getReadDB()
	if db == nil {
		return rollupAgg{}, nil
	}
	filter.TenantID = normalizeTenantID(filter.TenantID)
	if filter.BucketKind == "" {
		filter.BucketKind = rollupBucketDay
	}

	var b strings.Builder
	args := make([]any, 0, 16)
	b.WriteString(`
		SELECT
			COALESCE(SUM(request_count), 0),
			COALESCE(SUM(success_count), 0),
			COALESCE(SUM(failure_count), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cost_total), 0),
			COALESCE(SUM(latency_sum_ms), 0),
			COALESCE(SUM(latency_count), 0),` + rollupCacheSumsSQL + `
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ?
	`)
	args = append(args, filter.TenantID, filter.BucketKind)
	if filter.BucketFrom != "" {
		b.WriteString(` AND bucket_start >= ?`)
		args = append(args, filter.BucketFrom)
	}
	if filter.BucketTo != "" {
		b.WriteString(` AND bucket_start < ?`)
		args = append(args, filter.BucketTo)
	}
	if ids := dedupeExactStrings(filter.APIKeyIDs); len(ids) > 0 {
		b.WriteString(` AND api_key_id IN (` + placeholders(len(ids)) + `)`)
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if eu := strings.TrimSpace(filter.EndUserID); eu != "" {
		b.WriteString(` AND end_user_id = ?`)
		args = append(args, eu)
	}
	subjects := append([]string{}, filter.AuthSubjectIDs...)
	if sub := strings.TrimSpace(filter.AuthSubjectID); sub != "" {
		subjects = append(subjects, sub)
	}
	if subjects = dedupeExactStrings(subjects); len(subjects) == 1 {
		b.WriteString(` AND auth_subject_id = ?`)
		args = append(args, subjects[0])
	} else if len(subjects) > 1 {
		b.WriteString(` AND auth_subject_id IN (` + placeholders(len(subjects)) + `)`)
		for _, subject := range subjects {
			args = append(args, subject)
		}
	}
	if models := dedupeExactStrings(filter.Models); len(models) > 0 {
		b.WriteString(` AND model IN (` + placeholders(len(models)) + `)`)
		for _, m := range models {
			args = append(args, m)
		}
	}
	if sources := dedupeExactStrings(filter.Sources); len(sources) > 0 {
		b.WriteString(` AND source IN (` + placeholders(len(sources)) + `)`)
		for _, s := range sources {
			args = append(args, s)
		}
	}
	if channels := dedupeLowerTrimmedStrings(filter.ChannelNames); len(channels) > 0 {
		b.WriteString(` AND lower(trim(channel_name)) IN (` + placeholders(len(channels)) + `)`)
		for _, channel := range channels {
			args = append(args, channel)
		}
	}

	var out rollupAgg
	err := db.QueryRow(b.String(), args...).Scan(append([]any{
		&out.RequestCount,
		&out.SuccessCount,
		&out.FailureCount,
		&out.InputTokens,
		&out.OutputTokens,
		&out.ReasoningTokens,
		&out.CachedTokens,
		&out.TotalTokens,
		&out.CostTotal,
		&out.LatencySumMs,
		&out.LatencyCount,
	}, out.Cache.scanTargets()...)...)
	if err != nil {
		return rollupAgg{}, fmt.Errorf("usage: rollup aggregate: %w", err)
	}
	return out, nil
}

func placeholders(n int) string {
	if n < 1 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func dayBucketFromDays(days int) string {
	if days < 1 {
		days = 7
	}
	return localDayKeyAt(CutoffStartUTC(days))
}

func requestedAPIKeys(params LogQueryParams) []string {
	keys := append([]string{}, params.APIKeys...)
	if k := strings.TrimSpace(params.APIKey); k != "" {
		keys = append(keys, k)
	}
	return dedupeExactStrings(keys)
}

func resolveAPIKeyIDsForStats(params LogQueryParams) []string {
	ids := make([]string, 0, 4)
	for _, id := range params.APIKeyIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			ids = append(ids, trimmed)
		}
	}
	for _, key := range requestedAPIKeys(params) {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if identity := ResolveAPIKeyIdentity(key); identity != nil && identity.ID != "" {
			ids = append(ids, identity.ID)
			continue
		}
		if row := GetAPIKey(key); row != nil && strings.TrimSpace(row.ID) != "" {
			ids = append(ids, strings.TrimSpace(row.ID))
		}
	}
	return dedupeExactStrings(ids)
}

// rollupIdentityFilter builds a fail-closed identity filter for stats.
// Unknown API keys must not widen to tenant-wide aggregates.
func rollupIdentityFilter(params LogQueryParams) (rollupFilter, bool) {
	tenantID := params.TenantID
	if tenantID == "" {
		tenantID = systemTenantID
	}
	authSubjectIDs := dedupeExactStrings(params.AuthSubjectIDs)
	channelNames := dedupeLowerTrimmedStrings(params.ChannelNames)
	keyIDs := resolveAPIKeyIDsForStats(params)
	keysRequested := len(requestedAPIKeys(params)) > 0 || len(params.APIKeyIDs) > 0
	endUserID := strings.TrimSpace(params.EndUserID)
	// Key secrets/ids were provided but none resolve to a stable id → empty stats.
	if keysRequested && len(keyIDs) == 0 && endUserID == "" {
		return rollupFilter{}, false
	}
	// A requested rollup-supported channel selector with no usable values must
	// not widen to tenant-wide aggregates. Unsupported selectors use details.
	if (len(params.AuthSubjectIDs) > 0 || len(params.ChannelNames) > 0) &&
		len(authSubjectIDs) == 0 && len(channelNames) == 0 &&
		len(params.AuthIndexes) == 0 && len(params.AuthIndexChannelNames) == 0 {
		return rollupFilter{}, false
	}
	filter := rollupFilter{
		TenantID:       tenantID,
		BucketKind:     rollupBucketDay,
		APIKeyIDs:      keyIDs,
		EndUserID:      endUserID,
		AuthSubjectIDs: authSubjectIDs,
		Models:         params.Models,
		ChannelNames:   channelNames,
	}
	if len(authSubjectIDs) == 1 {
		filter.AuthSubjectID = authSubjectIDs[0]
		filter.AuthSubjectIDs = nil
	}
	return filter, true
}

func queryStatsFromRollup(params LogQueryParams) (LogStats, error) {
	filter, ok := rollupIdentityFilter(params)
	if !ok {
		return LogStats{CacheRate: 0}, nil
	}
	return queryStatsFromRollupFilter(params, filter)
}

func queryStatsFromRollupFilter(params LogQueryParams, filter rollupFilter) (LogStats, error) {
	if params.Days < 1 {
		params.Days = 7
	}
	filter.BucketFrom = dayBucketFromDays(params.Days)
	agg, err := queryRollupAgg(filter)
	if err != nil {
		return LogStats{}, err
	}
	return agg.toLogStats(), nil
}

func queryDashboardKPIFromRollup(tenantID string, days int) (DashboardKPI, error) {
	if days < 1 {
		days = 7
	}
	agg, err := queryRollupAgg(rollupFilter{
		TenantID:   tenantID,
		BucketKind: rollupBucketDay,
		BucketFrom: dayBucketFromDays(days),
	})
	if err != nil {
		return DashboardKPI{}, err
	}
	return agg.toDashboardKPI(), nil
}

func queryTodayCostByAPIKeyIDFromRollup(tenantID, apiKeyID string) (float64, error) {
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return 0, nil
	}
	return sumRollupCostForDay(tenantID, rollupBucketDay, localDayKeyAt(time.Now()), apiKeyID, "", "")
}

func sumRollupCostForDay(tenantID, kind, dayKey, apiKeyID, endUserID, authSubjectID string) (float64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	var b strings.Builder
	args := []any{normalizeTenantID(tenantID), kind, dayKey}
	b.WriteString(`SELECT COALESCE(SUM(cost_total), 0) FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start = ?`)
	if apiKeyID != "" {
		b.WriteString(` AND api_key_id = ?`)
		args = append(args, apiKeyID)
	}
	if endUserID != "" {
		b.WriteString(` AND end_user_id = ?`)
		args = append(args, endUserID)
	}
	if authSubjectID != "" {
		b.WriteString(` AND auth_subject_id = ?`)
		args = append(args, authSubjectID)
	}
	var total float64
	if err := db.QueryRow(b.String(), args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("usage: rollup day cost: %w", err)
	}
	return total, nil
}

func queryLifetimeCountByAPIKeyIDFromRollup(tenantID, apiKeyID string) (int64, error) {
	agg, err := queryRollupAgg(rollupFilter{
		TenantID:   tenantID,
		BucketKind: rollupBucketLifetime,
		APIKeyIDs:  []string{strings.TrimSpace(apiKeyID)},
	})
	if err != nil {
		return 0, err
	}
	return agg.RequestCount, nil
}

func queryTodayCountByAPIKeyIDFromRollup(tenantID, apiKeyID string) (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	var count int64
	err := db.QueryRow(`
		SELECT COALESCE(SUM(request_count), 0)
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start = ? AND api_key_id = ?
	`, normalizeTenantID(tenantID), rollupBucketDay, localDayKeyAt(time.Now()), strings.TrimSpace(apiKeyID)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: rollup today count: %w", err)
	}
	return count, nil
}

func queryTodayCostByEndUserFromRollup(tenantID, endUserID string) (float64, error) {
	return sumRollupCostForDay(tenantID, rollupBucketDay, localDayKeyAt(time.Now()), "", strings.TrimSpace(endUserID), "")
}

func queryLifetimeCountByEndUserFromRollup(tenantID, endUserID string) (int64, error) {
	agg, err := queryRollupAgg(rollupFilter{
		TenantID:   tenantID,
		BucketKind: rollupBucketLifetime,
		EndUserID:  endUserID,
	})
	if err != nil {
		return 0, err
	}
	return agg.RequestCount, nil
}

func queryTodayCountByEndUserFromRollup(tenantID, endUserID string) (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	var count int64
	err := db.QueryRow(`
		SELECT COALESCE(SUM(request_count), 0)
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start = ? AND end_user_id = ?
	`, normalizeTenantID(tenantID), rollupBucketDay, localDayKeyAt(time.Now()), strings.TrimSpace(endUserID)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: rollup today end-user count: %w", err)
	}
	return count, nil
}

type rollupDayPoint struct {
	Date           string
	Requests       int64
	FailedRequests int64
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	Cost           float64
	CachedTokens   int64
	SuccessCount   int64
	Model          string
	Cache          cacheTotals
}

func queryRollupDailySeries(filter rollupFilter) ([]rollupDayPoint, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	filter.TenantID = normalizeTenantID(filter.TenantID)
	if filter.BucketKind == "" {
		filter.BucketKind = rollupBucketDay
	}
	var b strings.Builder
	args := make([]any, 0, 12)
	b.WriteString(`
		SELECT bucket_start,
			COALESCE(SUM(request_count), 0),
			COALESCE(SUM(failure_count), 0),
			COALESCE(SUM(success_count), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cost_total), 0),
			COALESCE(SUM(cached_tokens), 0),` + rollupCacheSumsSQL + `
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ?
	`)
	args = append(args, filter.TenantID, filter.BucketKind)
	if filter.BucketFrom != "" {
		b.WriteString(` AND bucket_start >= ?`)
		args = append(args, filter.BucketFrom)
	}
	if ids := dedupeExactStrings(filter.APIKeyIDs); len(ids) > 0 {
		b.WriteString(` AND api_key_id IN (` + placeholders(len(ids)) + `)`)
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if eu := strings.TrimSpace(filter.EndUserID); eu != "" {
		b.WriteString(` AND end_user_id = ?`)
		args = append(args, eu)
	}
	b.WriteString(` GROUP BY bucket_start ORDER BY bucket_start`)

	rows, err := db.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("usage: rollup daily series: %w", err)
	}
	defer rows.Close()
	out := make([]rollupDayPoint, 0)
	for rows.Next() {
		var p rollupDayPoint
		if err := rows.Scan(append([]any{
			&p.Date, &p.Requests, &p.FailedRequests, &p.SuccessCount,
			&p.InputTokens, &p.OutputTokens, &p.TotalTokens, &p.Cost,
			&p.CachedTokens,
		}, p.Cache.scanTargets()...)...); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func queryRollupModelDistribution(filter rollupFilter) ([]ModelDistributionPoint, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	filter.TenantID = normalizeTenantID(filter.TenantID)
	if filter.BucketKind == "" {
		filter.BucketKind = rollupBucketDay
	}
	var b strings.Builder
	args := make([]any, 0, 12)
	b.WriteString(`
		SELECT model,
			COALESCE(SUM(request_count), 0),
			COALESCE(SUM(total_tokens), 0)
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND model != ''
	`)
	args = append(args, filter.TenantID, filter.BucketKind)
	if filter.BucketFrom != "" {
		b.WriteString(` AND bucket_start >= ?`)
		args = append(args, filter.BucketFrom)
	}
	if ids := dedupeExactStrings(filter.APIKeyIDs); len(ids) > 0 {
		b.WriteString(` AND api_key_id IN (` + placeholders(len(ids)) + `)`)
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if eu := strings.TrimSpace(filter.EndUserID); eu != "" {
		b.WriteString(` AND end_user_id = ?`)
		args = append(args, eu)
	}
	b.WriteString(` GROUP BY model ORDER BY 2 DESC`)

	rows, err := db.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("usage: rollup model distribution: %w", err)
	}
	defer rows.Close()
	out := make([]ModelDistributionPoint, 0)
	for rows.Next() {
		var p ModelDistributionPoint
		if err := rows.Scan(&p.Model, &p.Requests, &p.Tokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func queryRollupAPIKeyDistributionForEndUser(tenantID, endUserID string, days int) ([]PublicAPIKeyDistributionPoint, error) {
	db := getReadDB()
	if db == nil {
		return []PublicAPIKeyDistributionPoint{}, nil
	}
	tenantID = normalizeTenantID(tenantID)
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return []PublicAPIKeyDistributionPoint{}, nil
	}
	if days < 1 {
		days = 7
	}

	rows, err := db.Query(`
		SELECT api_key_id,
			COALESCE(SUM(request_count), 0),
			COALESCE(SUM(total_tokens), 0)
		FROM usage_rollup_buckets
		WHERE tenant_id = ?
		  AND bucket_kind = ?
		  AND bucket_start >= ?
		  AND end_user_id = ?
		  AND trim(api_key_id) != ''
		GROUP BY api_key_id
		ORDER BY 2 DESC, api_key_id
	`, tenantID, rollupBucketDay, dayBucketFromDays(days), endUserID)
	if err != nil {
		return nil, fmt.Errorf("usage: public rollup apikey distribution: %w", err)
	}
	defer rows.Close()

	currentByID := currentAPIKeyRowsByIDForTenant(tenantID)
	out := make([]PublicAPIKeyDistributionPoint, 0)
	for rows.Next() {
		var point PublicAPIKeyDistributionPoint
		if err := rows.Scan(&point.APIKeyID, &point.Requests, &point.Tokens); err != nil {
			return nil, fmt.Errorf("usage: public rollup apikey distribution scan: %w", err)
		}
		point.APIKeyID = strings.TrimSpace(point.APIKeyID)
		if row, ok := currentByID[point.APIKeyID]; ok {
			// Public charts use the key's own current name, never the owner display name.
			point.Name = strings.TrimSpace(row.Name)
		}
		out = append(out, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
