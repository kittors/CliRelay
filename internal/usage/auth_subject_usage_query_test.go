package usage

import (
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Chart timezone fixtures park the clock at 07:26 on 2026-09-23 in a UTC+8 usage
// timezone while UTC is still on 2026-09-22, the window in which SQL-side
// 'localtime' keys (process TZ on SQLite, UTC on PostgreSQL) disagree with it.
var (
	chartFixtureLoc = time.FixedZone("UTC+8", 8*3600)
	chartFixtureNow = time.Date(2026, 9, 22, 23, 26, 0, 0, time.UTC)
)

// Regression: day keys came from SQL date(timestamp, 'localtime') while the AI
// Accounts trend slots use the usage timezone, so between local and UTC midnight
// the newest rows keyed to a day outside the slots and vanished from the chart.
func TestQueryDailyUsageByAuthSubjectBucketsByUsageTimezone(t *testing.T) {
	CloseDB()
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, chartFixtureLoc); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	assertDailyUsageByAuthSubjectFollowsUsageTimezone(t)
}

// Regression: hourly buckets used time.Truncate(time.Hour), which truncates
// absolute time, so in a +05:30 usage timezone every bucket began at :30 and
// was labelled with the hour before it.
func TestQueryHourlyUsageByAuthSubjectFollowsLocalWallClockHours(t *testing.T) {
	CloseDB()
	loc := time.FixedZone("UTC+05:30", 5*3600+30*60)
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, loc); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	for _, at := range []time.Time{
		time.Date(2026, 9, 22, 0, 29, 59, 0, time.UTC), // 05:59:59 local, before the window
		time.Date(2026, 9, 22, 0, 30, 0, 0, time.UTC),  // 06:00 local, first bucket
		time.Date(2026, 9, 22, 4, 40, 0, 0, time.UTC),  // 10:10 local, current hour
	} {
		InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-half-hour", false, at, 1, 1, TokenStats{TotalTokens: 1}, "", "")
	}

	now := time.Date(2026, 9, 22, 4, 45, 0, 0, time.UTC) // 10:15 local
	hourly, err := queryHourlyUsageByAuthSubject(systemTenantID, AuthSubjectMatcher{AuthIndexes: []string{"auth-half-hour"}}, 5, false, now, loc)
	if err != nil {
		t.Fatalf("queryHourlyUsageByAuthSubject: %v", err)
	}

	want := []HourlyUsagePoint{
		{Hour: "2026-09-22 06:00", Requests: 1},
		{Hour: "2026-09-22 07:00"},
		{Hour: "2026-09-22 08:00"},
		{Hour: "2026-09-22 09:00"},
		{Hour: "2026-09-22 10:00", Requests: 1},
	}
	if !reflect.DeepEqual(hourly, want) {
		t.Fatalf("hourly = %+v, want %+v", hourly, want)
	}
	// Shared subjects without hour data fall back to these empty slots.
	empty := emptyHourlyUsageBucketsAt(now, 5, loc)
	for i := range want {
		want[i].Requests = 0
	}
	if !reflect.DeepEqual(empty, want) {
		t.Fatalf("empty buckets = %+v, want %+v", empty, want)
	}
}

// assertDailyUsageByAuthSubjectFollowsUsageTimezone runs on SQLite and on
// PostgreSQL, where production day keys come from.
func assertDailyUsageByAuthSubjectFollowsUsageTimezone(t *testing.T) {
	t.Helper()
	for _, at := range []time.Time{
		time.Date(2026, 9, 16, 15, 59, 59, 0, time.UTC), // 09-16 23:59:59 local, before the window
		time.Date(2026, 9, 16, 16, 0, 0, 0, time.UTC),   // 09-17 00:00 local, first slot
		time.Date(2026, 9, 22, 15, 59, 59, 0, time.UTC), // 09-22 23:59:59 local
		time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC),   // 09-23 00:00 local
		time.Date(2026, 9, 22, 21, 26, 0, 0, time.UTC),  // 09-23 05:26 local, the row that vanished
		time.Date(2026, 9, 23, 16, 0, 0, 0, time.UTC),   // 09-24 00:00 local, after the window
	} {
		InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-tz", false, at, 1, 1, TokenStats{TotalTokens: 1}, "", "")
	}

	daily, err := queryDailyUsageByAuthSubjectAt(systemTenantID, AuthSubjectMatcher{AuthIndexes: []string{"auth-tz"}}, 7, chartFixtureNow, chartFixtureLoc)
	if err != nil {
		t.Fatalf("queryDailyUsageByAuthSubjectAt: %v", err)
	}

	want := []DailyUsagePoint{
		{Date: "2026-09-17", Requests: 1},
		{Date: "2026-09-22", Requests: 1},
		{Date: "2026-09-23", Requests: 2},
	}
	if !reflect.DeepEqual(daily, want) {
		t.Fatalf("daily = %+v, want %+v", daily, want)
	}
}

// Regression: AI Accounts card trend used to SELECT every matching request_logs
// row for the 7-day daily series and aggregate in Go, which pegged CPU on large
// tenants. Daily totals must stay correct after SQL-side GROUP BY.
func TestQueryDailyUsageByAuthSubjectAggregatesInSQL(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	loc := getUsageLocation()
	nowLocal := time.Now().In(loc)
	// Place three requests: two on "today" local day, one on yesterday local day.
	todayMorning := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 9, 15, 0, 0, loc)
	todayAfternoon := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 14, 45, 0, 0, loc)
	yesterday := todayMorning.AddDate(0, 0, -1)

	if err := UpsertModelPricing("gpt-5.4", 1, 1, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}

	matcher := AuthSubjectMatcher{AuthIndexes: []string{"auth-sql-agg"}}
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-sql-agg", false, yesterday.UTC(), 10, 1, TokenStats{
		InputTokens: 1000, OutputTokens: 0, TotalTokens: 1000,
	}, "", "")
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-sql-agg", false, todayMorning.UTC(), 10, 1, TokenStats{
		InputTokens: 1000, OutputTokens: 0, TotalTokens: 1000,
	}, "", "")
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-sql-agg", false, todayAfternoon.UTC(), 10, 1, TokenStats{
		InputTokens: 2000, OutputTokens: 0, TotalTokens: 2000,
	}, "", "")
	// Noise row for a different auth_index must not appear.
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-other", false, todayMorning.UTC(), 10, 1, TokenStats{
		InputTokens: 9999, OutputTokens: 0, TotalTokens: 9999,
	}, "", "")

	daily, err := QueryDailyUsageByAuthSubject(matcher, 3)
	if err != nil {
		t.Fatalf("QueryDailyUsageByAuthSubject: %v", err)
	}
	byDate := map[string]DailyUsagePoint{}
	for _, point := range daily {
		byDate[point.Date] = point
	}
	todayKey := todayMorning.Format("2006-01-02")
	yesterdayKey := yesterday.Format("2006-01-02")
	if got := byDate[todayKey]; got.Requests != 2 {
		t.Fatalf("today requests = %d cost=%v, want 2 (points=%+v)", got.Requests, got.Cost, daily)
	}
	if got := byDate[yesterdayKey]; got.Requests != 1 {
		t.Fatalf("yesterday requests = %d, want 1 (points=%+v)", got.Requests, daily)
	}
	// Pricing is $1 / 1M input tokens → 1000+2000 tokens today = 0.003
	if math.Abs(byDate[todayKey].Cost-0.003) > 1e-9 {
		t.Fatalf("today cost = %v, want ~0.003", byDate[todayKey].Cost)
	}

	// Hourly path remains a narrow window; just ensure it still sees recent rows.
	recent := time.Now().Add(-30 * time.Minute)
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-sql-agg", false, recent.UTC(), 10, 1, TokenStats{
		InputTokens: 500, OutputTokens: 0, TotalTokens: 500,
	}, "", "")

	hourly, err := QueryHourlyUsageByAuthSubject(matcher, 5)
	if err != nil {
		t.Fatalf("QueryHourlyUsageByAuthSubject: %v", err)
	}
	if len(hourly) != 5 {
		t.Fatalf("hourly len = %d, want 5", len(hourly))
	}
	var hourlyTotal int64
	for _, point := range hourly {
		hourlyTotal += point.Requests
	}
	if hourlyTotal < 1 {
		t.Fatalf("hourly total requests = %d, want >= 1 (buckets=%+v)", hourlyTotal, hourly)
	}
}
