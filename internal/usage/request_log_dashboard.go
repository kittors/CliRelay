package usage

import (
	"fmt"
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

type DashboardTrends struct {
	RequestVolume    []DashboardTrendPoint      `json:"request_volume"`
	SuccessRate      []DashboardTrendPoint      `json:"success_rate"`
	TotalTokens      []DashboardTrendPoint      `json:"total_tokens"`
	FailedRequests   []DashboardTrendPoint      `json:"failed_requests"`
	ThroughputSeries []DashboardThroughputPoint `json:"throughput_series"`
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
	db := getReadDB()
	if db == nil {
		return DashboardKPI{}, nil
	}
	if days < 1 {
		days = 7
	}

	cutoff := CutoffStartUTC(days).Format(time.RFC3339)

	var kpi DashboardKPI
	var effectiveInputTokens int64
	kpiSQL := "SELECT " +
		"COUNT(*), " +
		"COALESCE(SUM(CASE WHEN failed=0 THEN 1 ELSE 0 END), 0), " +
		"COALESCE(SUM(CASE WHEN failed=1 THEN 1 ELSE 0 END), 0), " +
		"COALESCE(SUM(input_tokens), 0), " +
		"COALESCE(SUM(output_tokens), 0), " +
		"COALESCE(SUM(reasoning_tokens), 0), " +
		"COALESCE(SUM(cached_tokens), 0), " +
		"COALESCE(SUM(total_tokens), 0), " +
		"COALESCE(SUM(cost), 0), " +
		"COALESCE(SUM(" + cacheRateEffectiveInputSQL + "), 0) " +
		"FROM request_logs WHERE timestamp >= ?"
	err := db.QueryRow(kpiSQL, cutoff).Scan(
		&kpi.TotalRequests,
		&kpi.SuccessRequests,
		&kpi.FailedRequests,
		&kpi.InputTokens,
		&kpi.OutputTokens,
		&kpi.ReasoningTokens,
		&kpi.CachedTokens,
		&kpi.TotalTokens,
		&kpi.TotalCost,
		&effectiveInputTokens,
	)
	if err != nil {
		return DashboardKPI{}, fmt.Errorf("usage: dashboard KPI query: %w", err)
	}

	if kpi.TotalRequests > 0 {
		kpi.SuccessRate = float64(kpi.SuccessRequests) / float64(kpi.TotalRequests) * 100
	}
	kpi.CacheRate = cacheRateFromTokenTotals(effectiveInputTokens, kpi.CachedTokens)

	return kpi, nil
}

// QueryDashboardTrends returns fixed-width trend buckets used by the dashboard.
// KPI trends follow the selected day range, while throughput always shows the
// most recent 7 one-minute buckets.
func QueryDashboardTrends(days int) (DashboardTrends, error) {
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

	rows, err := db.Query(`
		SELECT timestamp, failed, total_tokens
		FROM request_logs
		WHERE timestamp >= ?
	`, CutoffStartUTC(days).Format(time.RFC3339))
	if err != nil {
		return DashboardTrends{}, fmt.Errorf("usage: query dashboard trends: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts string
		var failedInt int
		var totalTokens int64
		if err := rows.Scan(&ts, &failedInt, &totalTokens); err != nil {
			return DashboardTrends{}, fmt.Errorf("usage: scan dashboard trend row: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
		}
		key := dashboardBucketKey(parsed.In(loc), days)
		bucket := byKey[key]
		if bucket == nil {
			continue
		}
		bucket.requests++
		bucket.totalToken += totalTokens
		if failedInt != 0 {
			bucket.failed++
		} else {
			bucket.success++
		}
	}
	if err := rows.Err(); err != nil {
		return DashboardTrends{}, fmt.Errorf("usage: iterate dashboard trends: %w", err)
	}

	throughputSeries, err := queryDashboardThroughputSeriesAt(time.Now(), loc)
	if err != nil {
		return DashboardTrends{}, err
	}

	trends := dashboardTrendsFromBuckets(buckets)
	trends.ThroughputSeries = throughputSeries
	return trends, nil
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

func queryDashboardThroughputSeriesAt(now time.Time, loc *time.Location) ([]DashboardThroughputPoint, error) {
	db := getReadDB()
	if db == nil {
		return throughputSeriesFromBuckets(buildRecentThroughputBucketsAt(now, loc)), nil
	}
	if loc == nil {
		loc = time.Local
	}

	buckets := buildRecentThroughputBucketsAt(now, loc)
	byKey := make(map[string]*dashboardBucket, len(buckets))
	for i := range buckets {
		byKey[buckets[i].key] = &buckets[i]
	}

	start := now.In(loc).Truncate(time.Minute).Add(-time.Duration(dashboardThroughputBucketCount-1) * time.Minute)
	rows, err := db.Query(`
		SELECT timestamp, total_tokens
		FROM request_logs
		WHERE timestamp >= ?
	`, start.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("usage: query dashboard throughput trends: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts string
		var totalTokens int64
		if err := rows.Scan(&ts, &totalTokens); err != nil {
			return nil, fmt.Errorf("usage: scan dashboard throughput row: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
		}
		key := parsed.In(loc).Truncate(time.Minute).Format("2006-01-02 15:04")
		bucket := byKey[key]
		if bucket == nil {
			continue
		}
		bucket.requests++
		bucket.totalToken += totalTokens
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate dashboard throughput rows: %w", err)
	}

	return throughputSeriesFromBuckets(buckets), nil
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
