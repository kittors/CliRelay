package usage

import (
	"fmt"
	"strings"
	"time"
)

func QueryDailyCallsByAuthSubject(matcher AuthSubjectMatcher, days int) ([]DailyCountPoint, error) {
	usagePoints, err := QueryDailyUsageByAuthSubject(matcher, days)
	if err != nil {
		return nil, err
	}
	result := make([]DailyCountPoint, 0, len(usagePoints))
	for _, point := range usagePoints {
		result = append(result, DailyCountPoint{Date: point.Date, Requests: point.Requests})
	}
	return result, nil
}

func QueryDailyUsageByAuthSubject(matcher AuthSubjectMatcher, days int) ([]DailyUsagePoint, error) {
	return QueryDailyUsageByAuthSubjectForTenant(systemTenantID, matcher, days)
}

func QueryDailyUsageByAuthSubjectForTenant(tenantID string, matcher AuthSubjectMatcher, days int) ([]DailyUsagePoint, error) {
	tenantID = normalizeTenantID(tenantID)
	db := getReadDB()
	if db == nil {
		return []DailyUsagePoint{}, nil
	}
	if days < 1 {
		days = 7
	}

	matchSQL, matchArgs := buildAuthSubjectMatchClause(matcher, "source", "channel_name")
	if matchSQL == "" {
		return []DailyUsagePoint{}, nil
	}

	// Aggregate in SQL instead of streaming every matching request_logs row into Go.
	// date(timestamp, 'localtime') is rewritten by the postgres compat driver to a
	// UTC day key; production keeps process TZ aligned with the configured usage zone.
	args := make([]interface{}, 0, len(matchArgs)+2)
	args = append(args, tenantID, CutoffStartUTC(days).Format(time.RFC3339))
	args = append(args, matchArgs...)

	rows, err := db.Query(fmt.Sprintf(`
		SELECT date(timestamp, 'localtime') as d, COUNT(*), COALESCE(SUM(cost), 0)
		FROM request_logs
		WHERE tenant_id = ? AND timestamp >= ? AND (%s)
		GROUP BY d
		ORDER BY d
	`, matchSQL), args...)
	if err != nil {
		return nil, fmt.Errorf("usage: daily usage by auth subject query: %w", err)
	}
	defer rows.Close()

	result := make([]DailyUsagePoint, 0, days)
	for rows.Next() {
		var point DailyUsagePoint
		if err := rows.Scan(&point.Date, &point.Requests, &point.Cost); err != nil {
			return nil, fmt.Errorf("usage: daily usage by auth subject scan: %w", err)
		}
		point.Date = strings.TrimSpace(point.Date)
		if point.Date == "" {
			continue
		}
		result = append(result, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func QueryHourlyCallsByAuthSubject(matcher AuthSubjectMatcher, hours int) ([]HourlyCountPoint, error) {
	usagePoints, err := QueryHourlyUsageByAuthSubject(matcher, hours)
	if err != nil {
		return nil, err
	}
	result := make([]HourlyCountPoint, 0, len(usagePoints))
	for _, point := range usagePoints {
		result = append(result, HourlyCountPoint{Hour: point.Hour, Requests: point.Requests})
	}
	return result, nil
}

func QueryHourlyUsageByAuthSubject(matcher AuthSubjectMatcher, hours int) ([]HourlyUsagePoint, error) {
	return QueryHourlyUsageByAuthSubjectForTenant(systemTenantID, matcher, hours)
}

func QueryHourlyUsageByAuthSubjectForTenant(tenantID string, matcher AuthSubjectMatcher, hours int) ([]HourlyUsagePoint, error) {
	return queryHourlyUsageByAuthSubject(normalizeTenantID(tenantID), matcher, hours, false)
}

// QueryHourlyUsageByAuthSubjectAcrossTenants aggregates the last N hours for one
// physical account across all tenants. Only pass share-eligible subject matchers;
// never use for tenant-scoped subjects or request-log list/detail.
func QueryHourlyUsageByAuthSubjectAcrossTenants(matcher AuthSubjectMatcher, hours int) ([]HourlyUsagePoint, error) {
	return queryHourlyUsageByAuthSubject("", matcher, hours, true)
}

func queryHourlyUsageByAuthSubject(tenantID string, matcher AuthSubjectMatcher, hours int, acrossTenants bool) ([]HourlyUsagePoint, error) {
	db := getReadDB()
	if db == nil {
		return []HourlyUsagePoint{}, nil
	}
	if hours < 1 {
		hours = 5
	}
	if hours > 24 {
		hours = 24
	}
	// Cross-tenant path must use stable subject id only (no email/index aliases).
	if acrossTenants {
		subjectID := strings.TrimSpace(matcher.SubjectID)
		if subjectID == "" {
			return EmptyHourlyUsageBuckets(hours), nil
		}
		matcher = AuthSubjectMatcher{SubjectID: subjectID}
	}

	matchSQL, matchArgs := buildAuthSubjectMatchClause(matcher, "source", "channel_name")
	if matchSQL == "" {
		return []HourlyUsagePoint{}, nil
	}

	loc := getUsageLocation()
	now := time.Now().In(loc).Truncate(time.Hour)
	start := now.Add(-time.Duration(hours-1) * time.Hour)
	buckets := make([]HourlyUsagePoint, 0, hours)
	byKey := make(map[string]*HourlyUsagePoint, hours)
	for i := 0; i < hours; i++ {
		key := start.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:00")
		buckets = append(buckets, HourlyUsagePoint{Hour: key})
		byKey[key] = &buckets[len(buckets)-1]
	}

	args := make([]interface{}, 0, len(matchArgs)+2)
	var where string
	if acrossTenants {
		args = append(args, start.UTC().Format(time.RFC3339))
		args = append(args, matchArgs...)
		// Narrow 5–24h scan by subject only; do not return bodies/content.
		where = fmt.Sprintf(`timestamp >= ? AND (%s)`, matchSQL)
	} else {
		args = append(args, tenantID, start.UTC().Format(time.RFC3339))
		args = append(args, matchArgs...)
		where = fmt.Sprintf(`tenant_id = ? AND timestamp >= ? AND (%s)`, matchSQL)
	}

	// Keep a narrow row scan for hourly (≤24h). SQL strftime('localtime') follows
	// process TZ, which can diverge from getUsageLocation() in tests and some
	// deployments; Go-side bucketing preserves the project timezone contract.
	// Daily aggregation above is the expensive 7d path and stays SQL-side.
	rows, err := db.Query(fmt.Sprintf(`
		SELECT timestamp, cost
		FROM request_logs
		WHERE %s
	`, where), args...)
	if err != nil {
		return nil, fmt.Errorf("usage: hourly usage by auth subject query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts storedTime
		var cost float64
		if err := rows.Scan(&ts, &cost); err != nil {
			return nil, fmt.Errorf("usage: hourly usage by auth subject scan: %w", err)
		}
		if !ts.Valid {
			continue
		}
		key := ts.Time.In(loc).Truncate(time.Hour).Format("2006-01-02 15:00")
		if bucket := byKey[key]; bucket != nil {
			bucket.Requests++
			bucket.Cost += cost
		}
	}
	return buckets, rows.Err()
}

func QueryRequestCountByAuthSubjectSince(matcher AuthSubjectMatcher, since time.Time) (int64, error) {
	return QueryRequestCountByAuthSubjectSinceForTenant(systemTenantID, matcher, since)
}

func QueryRequestCountByAuthSubjectSinceForTenant(tenantID string, matcher AuthSubjectMatcher, since time.Time) (int64, error) {
	return queryInt64ByAuthSubjectSince(tenantID, matcher, since, "COUNT(*)", "count")
}

func QueryTotalTokensByAuthSubjectSince(matcher AuthSubjectMatcher, since time.Time) (int64, error) {
	return QueryTotalTokensByAuthSubjectSinceForTenant(systemTenantID, matcher, since)
}

func QueryTotalTokensByAuthSubjectSinceForTenant(tenantID string, matcher AuthSubjectMatcher, since time.Time) (int64, error) {
	return queryInt64ByAuthSubjectSince(tenantID, matcher, since, "COALESCE(SUM(total_tokens), 0)", "total tokens")
}

func QueryCostByAuthSubjectSince(matcher AuthSubjectMatcher, since time.Time) (float64, error) {
	return QueryCostByAuthSubjectSinceForTenant(systemTenantID, matcher, since)
}

func QueryCostByAuthSubjectSinceForTenant(tenantID string, matcher AuthSubjectMatcher, since time.Time) (float64, error) {
	tenantID = normalizeTenantID(tenantID)
	db := getReadDB()
	if db == nil {
		return 0, nil
	}

	matchSQL, matchArgs := buildAuthSubjectMatchClause(matcher, "source", "channel_name")
	if matchSQL == "" {
		return 0, nil
	}

	args := make([]interface{}, 0, len(matchArgs)+2)
	args = append(args, tenantID, since.UTC().Format(time.RFC3339))
	args = append(args, matchArgs...)

	var total float64
	err := db.QueryRow(fmt.Sprintf(`
		SELECT COALESCE(SUM(cost), 0)
		FROM request_logs
		WHERE tenant_id = ? AND timestamp >= ? AND (%s)
	`, matchSQL), args...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("usage: request cost by auth subject query: %w", err)
	}
	return total, nil
}

func queryInt64ByAuthSubjectSince(tenantID string, matcher AuthSubjectMatcher, since time.Time, aggregate, label string) (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}

	matchSQL, matchArgs := buildAuthSubjectMatchClause(matcher, "source", "channel_name")
	if matchSQL == "" {
		return 0, nil
	}

	args := make([]interface{}, 0, len(matchArgs)+2)
	args = append(args, normalizeTenantID(tenantID), since.UTC().Format(time.RFC3339))
	args = append(args, matchArgs...)

	var total int64
	err := db.QueryRow(fmt.Sprintf(`
		SELECT %s
		FROM request_logs
		WHERE tenant_id = ? AND timestamp >= ? AND (%s)
	`, aggregate, matchSQL), args...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("usage: request %s by auth subject query: %w", label, err)
	}
	return total, nil
}

func buildAuthSubjectMatchClause(matcher AuthSubjectMatcher, sourceColumn string, channelColumn string) (string, []interface{}) {
	subjectID := strings.TrimSpace(matcher.SubjectID)
	authIndexes := dedupeExactStrings(matcher.AuthIndexes)
	sourceAliases := dedupeLowerTrimmedStrings(matcher.SourceAliases)
	channelAliases := dedupeLowerTrimmedStrings(matcher.ChannelAliases)

	clauses := make([]string, 0, 4)
	args := make([]interface{}, 0, 1+len(authIndexes)+len(sourceAliases)+len(channelAliases))

	if subjectID != "" {
		clauses = append(clauses, "trim(coalesce(auth_subject_id, '')) = ?")
		args = append(args, subjectID)
	}

	legacyClauses := make([]string, 0, 3)
	legacyArgs := make([]interface{}, 0, len(authIndexes)+len(sourceAliases)+len(channelAliases))
	if len(authIndexes) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(authIndexes)), ",")
		legacyClauses = append(legacyClauses, "auth_index IN ("+placeholders+")")
		for _, value := range authIndexes {
			legacyArgs = append(legacyArgs, value)
		}
	}
	if len(sourceAliases) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sourceAliases)), ",")
		legacyClauses = append(legacyClauses, "lower(trim(coalesce("+sourceColumn+", ''))) IN ("+placeholders+")")
		for _, value := range sourceAliases {
			legacyArgs = append(legacyArgs, value)
		}
	}
	if len(channelAliases) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(channelAliases)), ",")
		legacyClauses = append(legacyClauses, "lower(trim(coalesce("+channelColumn+", ''))) IN ("+placeholders+")")
		for _, value := range channelAliases {
			legacyArgs = append(legacyArgs, value)
		}
	}
	if len(legacyClauses) > 0 {
		clauses = append(clauses, "(trim(coalesce(auth_subject_id, '')) = '' AND ("+strings.Join(legacyClauses, " OR ")+"))")
		args = append(args, legacyArgs...)
	}

	return strings.Join(clauses, " OR "), args
}

func dedupeExactStrings(values []string) []string {
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
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func dedupeLowerTrimmedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}
