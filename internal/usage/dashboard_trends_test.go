package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestQueryDashboardTrendsReturnsFixedDailyBuckets(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: false})

	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1)
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, yesterday, 120, 20, TokenStats{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
	}, "", "")
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", true, now, 180, 30, TokenStats{
		InputTokens:  40,
		OutputTokens: 50,
		TotalTokens:  90,
	}, "", "")

	trends, err := QueryDashboardTrends(7)
	if err != nil {
		t.Fatalf("QueryDashboardTrends() error = %v", err)
	}

	if len(trends.RequestVolume) != 7 {
		t.Fatalf("request_volume buckets = %d, want 7", len(trends.RequestVolume))
	}
	if len(trends.SuccessRate) != 7 {
		t.Fatalf("success_rate buckets = %d, want 7", len(trends.SuccessRate))
	}
	if len(trends.TotalTokens) != 7 {
		t.Fatalf("total_tokens buckets = %d, want 7", len(trends.TotalTokens))
	}
	if len(trends.FailedRequests) != 7 {
		t.Fatalf("failed_requests buckets = %d, want 7", len(trends.FailedRequests))
	}
	if len(trends.ThroughputSeries) != 7 {
		t.Fatalf("throughput_series buckets = %d, want 7", len(trends.ThroughputSeries))
	}

	todayLabel := localDayKeyAt(now)
	yesterdayLabel := localDayKeyAt(yesterday)
	todayRequests := findTrendValue(t, trends.RequestVolume, todayLabel)
	yesterdayRequests := findTrendValue(t, trends.RequestVolume, yesterdayLabel)
	todayFailed := findTrendValue(t, trends.FailedRequests, todayLabel)
	todaySuccessRate := findTrendValue(t, trends.SuccessRate, todayLabel)
	todayTokens := findTrendValue(t, trends.TotalTokens, todayLabel)

	if todayRequests != 1 {
		t.Fatalf("today requests = %.0f, want 1", todayRequests)
	}
	if yesterdayRequests != 1 {
		t.Fatalf("yesterday requests = %.0f, want 1", yesterdayRequests)
	}
	if todayFailed != 1 {
		t.Fatalf("today failed = %.0f, want 1", todayFailed)
	}
	if todaySuccessRate != 0 {
		t.Fatalf("today success rate = %.2f, want 0", todaySuccessRate)
	}
	if todayTokens != 90 {
		t.Fatalf("today tokens = %.0f, want 90", todayTokens)
	}
}

func TestQueryDashboardTrendsReturnsRecentMinuteThroughputBuckets(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: false})

	// Fixed query time so the rolling latest window is deterministic.
	now := time.Date(2026, 7, 13, 15, 5, 40, 0, time.UTC)
	twoMinutesAgo := now.Add(-2 * time.Minute)
	sixMinutesAgo := now.Add(-6 * time.Minute)
	eightMinutesAgo := now.Add(-8 * time.Minute)

	// Latest point uses last-60s window: request at :15 is inside [15:04:40, 15:05:40].
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, now.Truncate(time.Minute).Add(15*time.Second), 100, 20, TokenStats{
		InputTokens:  40,
		OutputTokens: 50,
		TotalTokens:  90,
	}, "", "")
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, twoMinutesAgo.Truncate(time.Minute).Add(25*time.Second), 90, 20, TokenStats{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
	}, "", "")
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, sixMinutesAgo.Truncate(time.Minute).Add(35*time.Second), 80, 20, TokenStats{
		InputTokens:  20,
		OutputTokens: 30,
		TotalTokens:  50,
	}, "", "")
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, eightMinutesAgo.Truncate(time.Minute).Add(45*time.Second), 70, 20, TokenStats{
		InputTokens:  30,
		OutputTokens: 40,
		TotalTokens:  70,
	}, "", "")

	series, err := queryDashboardThroughputSeriesAt(systemTenantID, now, time.UTC, false)
	if err != nil {
		t.Fatalf("queryDashboardThroughputSeriesAt() error = %v", err)
	}

	if len(series) != 7 {
		t.Fatalf("throughput_series buckets = %d, want 7", len(series))
	}

	nowLabel := now.In(time.UTC).Format("15:04")
	twoMinutesAgoLabel := twoMinutesAgo.In(time.UTC).Format("15:04")
	sixMinutesAgoLabel := sixMinutesAgo.In(time.UTC).Format("15:04")
	eightMinutesAgoLabel := eightMinutesAgo.In(time.UTC).Format("15:04")

	nowRPM, nowTPM := findThroughputValues(t, series, nowLabel)
	twoMinutesAgoRPM, twoMinutesAgoTPM := findThroughputValues(t, series, twoMinutesAgoLabel)
	sixMinutesAgoRPM, sixMinutesAgoTPM := findThroughputValues(t, series, sixMinutesAgoLabel)

	if nowRPM != 1 || nowTPM != 90 {
		t.Fatalf("current rolling throughput = (%.0f, %.0f), want (1, 90)", nowRPM, nowTPM)
	}
	if twoMinutesAgoRPM != 1 || twoMinutesAgoTPM != 30 {
		t.Fatalf("two minutes ago throughput = (%.0f, %.0f), want (1, 30)", twoMinutesAgoRPM, twoMinutesAgoTPM)
	}
	if sixMinutesAgoRPM != 1 || sixMinutesAgoTPM != 50 {
		t.Fatalf("six minutes ago throughput = (%.0f, %.0f), want (1, 50)", sixMinutesAgoRPM, sixMinutesAgoTPM)
	}
	for _, point := range series {
		if point.Label == eightMinutesAgoLabel {
			t.Fatalf("unexpected throughput bucket for %s outside the last 7 minutes", eightMinutesAgoLabel)
		}
	}
}

func TestQueryDashboardThroughputLatestPointUsesRollingMinuteWindow(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: false})

	// Early in a new calendar minute: calendar-bucket semantics would show RPM=0,
	// but the rolling window still counts requests from the previous minute.
	now := time.Date(2026, 7, 13, 15, 5, 5, 0, time.UTC)
	prevMinuteLate := time.Date(2026, 7, 13, 15, 4, 30, 0, time.UTC)
	// Window is [15:04:05, 15:05:05]; 15:03:50 is outside last 60s but still in 15:03/series history.
	prevMinuteEarlier := time.Date(2026, 7, 13, 15, 4, 0, 0, time.UTC) // outside last 60s
	twoMinutesAgo := time.Date(2026, 7, 13, 15, 3, 20, 0, time.UTC)

	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, prevMinuteLate, 100, 20, TokenStats{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  100,
	}, "", "")
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, prevMinuteLate.Add(5*time.Second), 100, 20, TokenStats{
		InputTokens:  20,
		OutputTokens: 30,
		TotalTokens:  200,
	}, "", "")
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, prevMinuteEarlier, 100, 20, TokenStats{
		InputTokens:  5,
		OutputTokens: 5,
		TotalTokens:  50,
	}, "", "")
	InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, twoMinutesAgo, 100, 20, TokenStats{
		InputTokens:  1,
		OutputTokens: 1,
		TotalTokens:  10,
	}, "", "")

	series, err := queryDashboardThroughputSeriesAt(systemTenantID, now, time.UTC, false)
	if err != nil {
		t.Fatalf("queryDashboardThroughputSeriesAt() error = %v", err)
	}
	if len(series) != 7 {
		t.Fatalf("throughput_series buckets = %d, want 7", len(series))
	}

	latest := series[len(series)-1]
	if latest.Label != "15:05" {
		t.Fatalf("latest label = %q, want 15:05", latest.Label)
	}
	// Rolling last-60s window [15:04:05, 15:05:05] includes two 15:04 late requests,
	// not the empty in-progress calendar minute 15:05.
	if latest.RPM != 2 || latest.TPM != 300 {
		t.Fatalf("latest rolling throughput = (%.0f, %.0f), want (2, 300)", latest.RPM, latest.TPM)
	}

	// Completed previous calendar minute reports full-minute rollup totals (3 requests).
	prevRPM, prevTPM := findThroughputValues(t, series, "15:04")
	if prevRPM != 3 || prevTPM != 350 {
		t.Fatalf("previous minute throughput = (%.0f, %.0f), want (3, 350)", prevRPM, prevTPM)
	}

	// Older completed minute is unchanged.
	olderRPM, olderTPM := findThroughputValues(t, series, "15:03")
	if olderRPM != 1 || olderTPM != 10 {
		t.Fatalf("15:03 throughput = (%.0f, %.0f), want (1, 10)", olderRPM, olderTPM)
	}
}

func findTrendValue(t *testing.T, points []DashboardTrendPoint, label string) float64 {
	t.Helper()
	for _, point := range points {
		if point.Label == label {
			return point.Value
		}
	}
	t.Fatalf("missing trend point with label %q", label)
	return 0
}

func findThroughputValues(t *testing.T, points []DashboardThroughputPoint, label string) (float64, float64) {
	t.Helper()
	for _, point := range points {
		if point.Label == label {
			return point.RPM, point.TPM
		}
	}
	t.Fatalf("missing throughput point with label %q", label)
	return 0, 0
}
