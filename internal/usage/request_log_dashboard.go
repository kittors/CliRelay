package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// DashboardKPI holds the aggregated KPI data needed by the dashboard page.
type DashboardKPI struct {
	TotalRequests   int64   `json:"total_requests"`
	SuccessRequests int64   `json:"success_requests"`
	FailedRequests  int64   `json:"failed_requests"`
	SuccessRate     float64 `json:"success_rate"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	TotalCost       float64 `json:"total_cost"`
	CacheRate       float64 `json:"cache_rate"`
}

type DashboardTrendPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type DashboardThroughputPoint struct {
	Label string  `json:"label"`
	RPM   float64 `json:"rpm"`
	TPM   float64 `json:"tpm"`
}

type DashboardTenantThroughputItem struct {
	TenantID         string                     `json:"tenant_id"`
	TenantName       string                     `json:"tenant_name,omitempty"`
	ThroughputSeries []DashboardThroughputPoint `json:"throughput_series"`
	CurrentRPM       float64                    `json:"current_rpm"`
	CurrentTPM       float64                    `json:"current_tpm"`
}

type DashboardTrends struct {
	RequestVolume    []DashboardTrendPoint           `json:"request_volume"`
	SuccessRate      []DashboardTrendPoint           `json:"success_rate"`
	TotalTokens      []DashboardTrendPoint           `json:"total_tokens"`
	FailedRequests   []DashboardTrendPoint           `json:"failed_requests"`
	ThroughputSeries []DashboardThroughputPoint      `json:"throughput_series"`
	Tenants          []DashboardTenantThroughputItem `json:"tenants,omitempty"`
}

type dashboardBucket struct {
	label      string
	key        string
	minutes    float64
	requests   int64
	success    int64
	failed     int64
	totalToken int64
}

const dashboardThroughputBucketCount = 7

// QueryDashboardKPI returns aggregated KPI data from SQLite for the dashboard.
// This replaces the old in-memory snapshot-based counting which lost data on restart.
func QueryDashboardKPI(days int) (DashboardKPI, error) {
	return QueryDashboardKPIForTenant(systemTenantID, days)
}

func QueryDashboardKPIForTenant(tenantID string, days int) (DashboardKPI, error) {
	if getReadDB() == nil {
		return DashboardKPI{}, nil
	}
	return queryDashboardKPIFromRollup(tenantID, days)
}

// QueryDashboardTrends returns fixed-width trend buckets used by the dashboard.
// KPI trends follow the selected day range, while throughput always shows the
// most recent 7 points: completed minutes use calendar-minute totals, and the
// latest point is a rolling last-60-seconds window so RPM/TPM stay continuous
// across minute boundaries.
func QueryDashboardTrends(days int) (DashboardTrends, error) {
	return QueryDashboardTrendsForTenant(systemTenantID, days)
}

func QueryDashboardTrendsForTenant(tenantID string, days int) (DashboardTrends, error) {
	db := getReadDB()
	if db == nil {
		return emptyDashboardTrends(days), nil
	}
	if days < 1 {
		days = 7
	}

	loc := getUsageLocation()
	buckets := buildDashboardBuckets(days, loc)
	byKey := make(map[string]*dashboardBucket, len(buckets))
	for i := range buckets {
		byKey[buckets[i].key] = &buckets[i]
	}

	// Trends read hour/day rollup buckets (not request_logs).
	tenantID = normalizeTenantID(tenantID)
	kind := rollupBucketDay
	fromKey := dayBucketFromDays(days)
	if days == 1 {
		kind = rollupBucketHour
		fromKey = CutoffStartUTC(1).In(loc).Format("2006-01-02T15")
	}
	rows, err := db.Query(`
		SELECT bucket_start,
		       COALESCE(SUM(request_count), 0),
		       COALESCE(SUM(success_count), 0),
		       COALESCE(SUM(failure_count), 0),
		       COALESCE(SUM(total_tokens), 0)
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start >= ?
		GROUP BY bucket_start
	`, tenantID, kind, fromKey)
	if err != nil {
		return DashboardTrends{}, fmt.Errorf("usage: query dashboard trends: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var bucketStart string
		var requests, success, failure, totalTokens int64
		if err := rows.Scan(&bucketStart, &requests, &success, &failure, &totalTokens); err != nil {
			return DashboardTrends{}, fmt.Errorf("usage: scan dashboard trend row: %w", err)
		}
		key := normalizeDashboardSQLBucketKey(rollupBucketToDashboardKey(bucketStart, days), days)
		bucket := byKey[key]
		if bucket == nil {
			continue
		}
		bucket.requests += requests
		bucket.totalToken += totalTokens
		bucket.failed += failure
		bucket.success += success
	}
	if err := rows.Err(); err != nil {
		return DashboardTrends{}, fmt.Errorf("usage: iterate dashboard trends: %w", err)
	}

	throughputSeries, err := queryDashboardThroughputSeriesAt(tenantID, time.Now(), loc, false)
	if err != nil {
		return DashboardTrends{}, err
	}

	trends := dashboardTrendsFromBuckets(buckets)
	trends.ThroughputSeries = throughputSeries
	return trends, nil
}

// rollupBucketToDashboardKey maps rollup bucket_start onto dashboard key shapes.
// day: "YYYY-MM-DD"; hour: "YYYY-MM-DDTHH" → "YYYY-MM-DD HH".
func rollupBucketToDashboardKey(bucketStart string, days int) string {
	bucketStart = strings.TrimSpace(bucketStart)
	if days == 1 && strings.Contains(bucketStart, "T") {
		// "2006-01-02T15" → "2006-01-02 15"
		return strings.Replace(bucketStart, "T", " ", 1)
	}
	return bucketStart
}

// normalizeDashboardSQLBucketKey maps SQL group keys onto dashboardBucketKey shapes.
// Hourly SQL uses "YYYY-MM-DD HH:00"; Go keys drop the trailing ":00".
func normalizeDashboardSQLBucketKey(raw string, days int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if days == 1 {
		if strings.HasSuffix(raw, ":00") {
			return strings.TrimSuffix(raw, ":00")
		}
		// Accept already-normalized "YYYY-MM-DD HH".
		if len(raw) == len("2006-01-02 15") {
			return raw
		}
	}
	return raw
}

// QueryDashboardThroughputAcrossTenants returns the recent 7 RPM/TPM points
// aggregated across every tenant (same continuous latest-window semantics as
// tenant trends). Used by platform super-admins.
func QueryDashboardThroughputAcrossTenants() ([]DashboardThroughputPoint, error) {
	return queryDashboardThroughputSeriesAt("", time.Now(), getUsageLocation(), true)
}

// QueryDashboardTenantBreakdownThroughput returns the recent 7 RPM/TPM points
// for each active tenant with traffic in the recent window or known tenants.
// Only accessible by platform super-admins.
func QueryDashboardTenantBreakdownThroughput(tenantNames map[string]string) ([]DashboardTenantThroughputItem, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	loc := getUsageLocation()
	now := time.Now().In(loc)
	start := now.Truncate(time.Minute).Add(-time.Duration(dashboardThroughputBucketCount-1) * time.Minute)
	fromMinute := start.Format("2006-01-02T15:04")
	toMinute := now.Add(time.Minute).Format("2006-01-02T15:04")
	fromUTC := now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	toUTC := now.UTC().Format(time.RFC3339Nano)

	// 1. Identify distinct tenant IDs active in rollup window or rolling window
	tenantSet := make(map[string]struct{})
	for tid := range tenantNames {
		if tid != "" {
			tenantSet[normalizeTenantID(tid)] = struct{}{}
		}
	}

	rows, err := db.Query(`
		SELECT DISTINCT tenant_id
		FROM usage_rollup_buckets
		WHERE bucket_kind = ? AND bucket_start >= ? AND bucket_start < ?
	`, rollupBucketMinute, fromMinute, toMinute)
	if err == nil {
		for rows.Next() {
			var tid string
			if scanErr := rows.Scan(&tid); scanErr == nil && tid != "" {
				tenantSet[normalizeTenantID(tid)] = struct{}{}
			}
		}
		_ = rows.Close()
	}

	wRows, wErr := db.Query(`
		SELECT DISTINCT tenant_id
		FROM request_logs
		WHERE timestamp >= ? AND timestamp <= ?
	`, fromUTC, toUTC)
	if wErr == nil {
		for wRows.Next() {
			var tid string
			if scanErr := wRows.Scan(&tid); scanErr == nil && tid != "" {
				tenantSet[normalizeTenantID(tid)] = struct{}{}
			}
		}
		_ = wRows.Close()
	}

	if len(tenantSet) == 0 {
		tenantSet[systemTenantID] = struct{}{}
	}

	// 2. Query each tenant's throughput series
	result := make([]DashboardTenantThroughputItem, 0, len(tenantSet))
	for tid := range tenantSet {
		series, sErr := queryDashboardThroughputSeriesAt(tid, now, loc, false)
		if sErr != nil {
			continue
		}
		var curRPM, curTPM float64
		if len(series) > 0 {
			curRPM = series[len(series)-1].RPM
			curTPM = series[len(series)-1].TPM
		}
		name := tid
		if tenantNames != nil {
			if n, ok := tenantNames[tid]; ok && n != "" {
				name = n
			}
		}
		result = append(result, DashboardTenantThroughputItem{
			TenantID:         tid,
			TenantName:       name,
			ThroughputSeries: series,
			CurrentRPM:       curRPM,
			CurrentTPM:       curTPM,
		})
	}

	return result, nil
}

func emptyDashboardTrends(days int) DashboardTrends {
	if days < 1 {
		days = 7
	}
	loc := getUsageLocation()
	trends := dashboardTrendsFromBuckets(buildDashboardBuckets(days, loc))
	trends.ThroughputSeries = throughputSeriesFromBuckets(buildRecentThroughputBucketsAt(time.Now(), loc))
	return trends
}

func buildDashboardBuckets(days int, loc *time.Location) []dashboardBucket {
	if loc == nil {
		loc = time.Local
	}
	start := CutoffStartUTC(days).In(loc)
	if days == 1 {
		buckets := make([]dashboardBucket, 0, 24)
		for i := 0; i < 24; i++ {
			at := start.Add(time.Duration(i) * time.Hour)
			buckets = append(buckets, dashboardBucket{
				label:   at.Format("15:04"),
				key:     dashboardBucketKey(at, days),
				minutes: 60,
			})
		}
		return buckets
	}

	buckets := make([]dashboardBucket, 0, days)
	for i := 0; i < days; i++ {
		at := start.AddDate(0, 0, i)
		buckets = append(buckets, dashboardBucket{
			label:   at.Format("2006-01-02"),
			key:     dashboardBucketKey(at, days),
			minutes: 24 * 60,
		})
	}
	return buckets
}

func dashboardBucketKey(t time.Time, days int) string {
	if days == 1 {
		return t.Format("2006-01-02 15")
	}
	return t.Format("2006-01-02")
}

func buildRecentThroughputBucketsAt(now time.Time, loc *time.Location) []dashboardBucket {
	if loc == nil {
		loc = time.Local
	}
	currentMinute := now.In(loc).Truncate(time.Minute)
	start := currentMinute.Add(-time.Duration(dashboardThroughputBucketCount-1) * time.Minute)
	buckets := make([]dashboardBucket, 0, dashboardThroughputBucketCount)
	for i := 0; i < dashboardThroughputBucketCount; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		buckets = append(buckets, dashboardBucket{
			label:   at.Format("15:04"),
			key:     at.Format("2006-01-02 15:04"),
			minutes: 1,
		})
	}
	return buckets
}

func queryDashboardThroughputSeriesAt(tenantID string, now time.Time, loc *time.Location, allTenants bool) ([]DashboardThroughputPoint, error) {
	db := getReadDB()
	if db == nil {
		return throughputSeriesFromBuckets(buildRecentThroughputBucketsAt(now, loc)), nil
	}
	if loc == nil {
		loc = time.Local
	}

	now = now.In(loc)
	buckets := buildRecentThroughputBucketsAt(now, loc)
	byKey := make(map[string]*dashboardBucket, len(buckets))
	for i := range buckets {
		byKey[buckets[i].key] = &buckets[i]
	}

	// Completed minute points stay calendar-aligned via minute rollup.
	// The latest point is a real rolling last-60s aggregate from request_logs so
	// RPM/TPM do not drop to zero at each calendar-minute boundary (rollup alone
	// only has the in-progress minute bucket, which is empty early in the minute).
	windowStart := now.Add(-time.Minute)
	start := now.Truncate(time.Minute).Add(-time.Duration(dashboardThroughputBucketCount-1) * time.Minute)

	var (
		rows *sql.Rows
		err  error
	)
	// Minute rollup keys are local "YYYY-MM-DDTHH:MM".
	fromMinute := start.Format("2006-01-02T15:04")
	toMinute := now.Add(time.Minute).Format("2006-01-02T15:04")
	if allTenants {
		rows, err = db.Query(`
			SELECT bucket_start,
			       COALESCE(SUM(request_count), 0),
			       COALESCE(SUM(total_tokens), 0)
			FROM usage_rollup_buckets
			WHERE bucket_kind = ? AND bucket_start >= ? AND bucket_start < ?
			GROUP BY bucket_start
		`, rollupBucketMinute, fromMinute, toMinute)
	} else {
		rows, err = db.Query(`
			SELECT bucket_start,
			       COALESCE(SUM(request_count), 0),
			       COALESCE(SUM(total_tokens), 0)
			FROM usage_rollup_buckets
			WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start >= ? AND bucket_start < ?
			GROUP BY bucket_start
		`, normalizeTenantID(tenantID), rollupBucketMinute, fromMinute, toMinute)
	}
	if err != nil {
		return nil, fmt.Errorf("usage: query dashboard throughput trends: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var bucketStart string
		var requests, totalTokens int64
		if err := rows.Scan(&bucketStart, &requests, &totalTokens); err != nil {
			return nil, fmt.Errorf("usage: scan dashboard throughput row: %w", err)
		}
		// "YYYY-MM-DDTHH:MM" → "YYYY-MM-DD HH:MM"
		key := strings.Replace(bucketStart, "T", " ", 1)
		if bucket := byKey[key]; bucket != nil {
			bucket.requests += requests
			bucket.totalToken += totalTokens
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate dashboard throughput rows: %w", err)
	}

	windowRequests, windowTokens, err := queryRollingThroughputWindow(db, tenantID, windowStart, now, allTenants)
	if err != nil {
		return nil, err
	}
	if len(buckets) > 0 {
		// Replace the in-progress calendar minute with the rolling window so the
		// rightmost chart point and headline RPM/TPM stay continuous.
		buckets[len(buckets)-1].requests = windowRequests
		buckets[len(buckets)-1].totalToken = windowTokens
	}

	return throughputSeriesFromBuckets(buckets), nil
}

// queryRollingThroughputWindow aggregates the last ~60s from request_logs.
// Bounded timestamp range only — avoids the multi-day dashboard poll storm.
func queryRollingThroughputWindow(db *sql.DB, tenantID string, windowStart, now time.Time, allTenants bool) (requests int64, totalTokens int64, err error) {
	fromUTC := windowStart.UTC().Format(time.RFC3339Nano)
	toUTC := now.UTC().Format(time.RFC3339Nano)
	if allTenants {
		err = db.QueryRow(`
			SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(total_tokens), 0)
			FROM request_logs
			WHERE timestamp >= ? AND timestamp <= ?
		`, fromUTC, toUTC).Scan(&requests, &totalTokens)
	} else {
		err = db.QueryRow(`
			SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(total_tokens), 0)
			FROM request_logs
			WHERE tenant_id = ? AND timestamp >= ? AND timestamp <= ?
		`, normalizeTenantID(tenantID), fromUTC, toUTC).Scan(&requests, &totalTokens)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("usage: query rolling throughput window: %w", err)
	}
	return requests, totalTokens, nil
}

func dashboardTrendsFromBuckets(buckets []dashboardBucket) DashboardTrends {
	trends := DashboardTrends{
		RequestVolume:    make([]DashboardTrendPoint, 0, len(buckets)),
		SuccessRate:      make([]DashboardTrendPoint, 0, len(buckets)),
		TotalTokens:      make([]DashboardTrendPoint, 0, len(buckets)),
		FailedRequests:   make([]DashboardTrendPoint, 0, len(buckets)),
		ThroughputSeries: make([]DashboardThroughputPoint, 0),
	}

	for _, bucket := range buckets {
		successRate := 0.0
		if bucket.requests > 0 {
			successRate = float64(bucket.success) / float64(bucket.requests) * 100
		}

		trends.RequestVolume = append(trends.RequestVolume, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.requests)})
		trends.SuccessRate = append(trends.SuccessRate, DashboardTrendPoint{Label: bucket.label, Value: successRate})
		trends.TotalTokens = append(trends.TotalTokens, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.totalToken)})
		trends.FailedRequests = append(trends.FailedRequests, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.failed)})
	}

	return trends
}

func throughputSeriesFromBuckets(buckets []dashboardBucket) []DashboardThroughputPoint {
	points := make([]DashboardThroughputPoint, 0, len(buckets))
	for _, bucket := range buckets {
		rpm := 0.0
		tpm := 0.0
		if bucket.minutes > 0 {
			rpm = float64(bucket.requests) / bucket.minutes
			tpm = float64(bucket.totalToken) / bucket.minutes
		}
		points = append(points, DashboardThroughputPoint{
			Label: bucket.label,
			RPM:   rpm,
			TPM:   tpm,
		})
	}
	return points
}
