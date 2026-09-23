package usage

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestQueryDailySeriesUsesProjectTimezoneDayBuckets(t *testing.T) {
	// Repro: Asia/Shanghai local 00:00–08:00 must count as the same local day as
	// mid-day traffic (not UTC day), matching request-log "today" rollup stats.
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "daily-series-tz.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, loc); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})

	const (
		apiKey   = "sk-chart-tz"
		apiKeyID = "key-chart-tz"
	)
	if err := UpsertAPIKey(APIKeyRow{ID: apiKeyID, Key: apiKey}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	// 2026-07-21 00:30 and 08:30 Asia/Shanghai → both local day 2026-07-21;
	// UTC would split them into 07/20 and 07/21.
	earlyLocal := time.Date(2026, 7, 21, 0, 30, 0, 0, loc)
	lateLocal := time.Date(2026, 7, 21, 8, 30, 0, 0, loc)
	for i, at := range []time.Time{earlyLocal, lateLocal} {
		tx, err := getDB().Begin()
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		if err := projectUsageRollupTx(tx, rollupEvent{
			TenantID: systemTenantID,
			APIKeyID: apiKeyID,
			Model:    "gpt-test",
			Source:   "openai",
			Tokens: TokenStats{
				InputTokens:  100 + int64(i),
				OutputTokens: 10 + int64(i),
				TotalTokens:  110 + int64(i)*2,
			},
			Cost: 0.01,
			At:   at,
		}); err != nil {
			_ = tx.Rollback()
			t.Fatalf("project %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	// Freeze "now" semantics: dayBucketFromDays uses time.Now via CutoffStartUTC.
	// Project enough history and query days large enough to include 2026-07-21.
	// Also assert the local day key itself.
	if got := localDayKeyAt(earlyLocal); got != "2026-07-21" {
		t.Fatalf("localDayKeyAt(early) = %q, want 2026-07-21", got)
	}
	if got := localDayKeyAt(lateLocal); got != "2026-07-21" {
		t.Fatalf("localDayKeyAt(late) = %q, want 2026-07-21", got)
	}

	// Direct rollup series (no days lower bound) via filter from day key.
	filter, ok := rollupIdentityFilter(LogQueryParams{TenantID: systemTenantID, APIKey: apiKey})
	if !ok {
		t.Fatal("rollupIdentityFilter failed")
	}
	filter.BucketKind = rollupBucketDay
	filter.BucketFrom = "2026-07-21"
	points, err := queryRollupDailySeries(filter)
	if err != nil {
		t.Fatalf("queryRollupDailySeries: %v", err)
	}
	if len(points) != 1 || points[0].Date != "2026-07-21" || points[0].Requests != 2 {
		t.Fatalf("rollup day points = %#v, want single 2026-07-21 with 2 requests", points)
	}
	if points[0].InputTokens != 201 || points[0].OutputTokens != 21 {
		t.Fatalf("tokens = in %d out %d, want 201/21", points[0].InputTokens, points[0].OutputTokens)
	}

	// QueryDailySeries with a wide window must surface the same local day.
	// Seed a "today" marker so dayBucketFromDays includes historical day when now is later.
	// If CI clock is far from 2026, use BucketFrom path above as authority and still call API with large days.
	series, err := QueryDailySeries(apiKey, 3650)
	if err != nil {
		t.Fatalf("QueryDailySeries: %v", err)
	}
	var found *DailySeriesPoint
	for i := range series {
		if series[i].Date == "2026-07-21" {
			found = &series[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("QueryDailySeries missing 2026-07-21: %#v", series)
	}
	if found.Requests != 2 || found.InputTokens != 201 || found.OutputTokens != 21 {
		t.Fatalf("QueryDailySeries point = %#v, want reqs=2 in=201 out=21", *found)
	}

	stats, err := QueryStats(LogQueryParams{TenantID: systemTenantID, APIKey: apiKey, Days: 3650})
	if err != nil {
		t.Fatalf("QueryStats: %v", err)
	}
	if stats.Total != 2 {
		t.Fatalf("QueryStats.Total = %d, want 2 (same local-day scope as chart)", stats.Total)
	}
}

// Regression: hour keys came from SQL strftime(..., 'localtime'), so the monitor
// panel, which prints them as local wall-clock hours, showed UTC hours on
// PostgreSQL.
func TestQueryHourlySeriesKeysHoursInUsageTimezone(t *testing.T) {
	CloseDB()
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, chartFixtureLoc); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	assertHourlySeriesFollowsUsageTimezone(t)
}

// assertHourlySeriesFollowsUsageTimezone runs on SQLite and on PostgreSQL.
func assertHourlySeriesFollowsUsageTimezone(t *testing.T) {
	t.Helper()
	const apiKey = "sk-hourly-tz"
	for _, row := range []struct {
		at     time.Time
		tokens int64
	}{
		{time.Date(2026, 9, 21, 23, 25, 59, 0, time.UTC), 1}, // 09-22 07:25:59 local, before the 24h window
		{time.Date(2026, 9, 21, 23, 26, 0, 0, time.UTC), 2},  // 09-22 07:26 local, first hour
		{time.Date(2026, 9, 22, 15, 59, 59, 0, time.UTC), 4}, // 09-22 23:59:59 local
		{time.Date(2026, 9, 22, 21, 26, 0, 0, time.UTC), 8},  // 09-23 05:26 local
		{time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC), 16},   // 09-23 08:00 local, after the window
	} {
		InsertLog(apiKey, "", "gpt-5.4", "codex", "Codex", "auth-hourly-tz", false, row.at, 1, 1, TokenStats{InputTokens: row.tokens, TotalTokens: row.tokens}, "", "")
	}

	tokens, models, err := queryHourlySeriesAt(systemTenantID, apiKey, 24, chartFixtureNow, chartFixtureLoc)
	if err != nil {
		t.Fatalf("queryHourlySeriesAt: %v", err)
	}

	type hourTotal struct {
		Hour  string
		Total int64
	}
	gotTokens := make([]hourTotal, 0, len(tokens))
	for _, point := range tokens {
		gotTokens = append(gotTokens, hourTotal{point.Hour, point.TotalTokens})
	}
	wantTokens := []hourTotal{{"2026-09-22 07:00", 2}, {"2026-09-22 23:00", 4}, {"2026-09-23 05:00", 8}}
	if !reflect.DeepEqual(gotTokens, wantTokens) {
		t.Fatalf("hourly tokens = %+v, want %+v", gotTokens, wantTokens)
	}
	wantModels := []HourlyModelPoint{
		{Hour: "2026-09-22 07:00", Model: "gpt-5.4", Requests: 1},
		{Hour: "2026-09-22 23:00", Model: "gpt-5.4", Requests: 1},
		{Hour: "2026-09-23 05:00", Model: "gpt-5.4", Requests: 1},
	}
	if !reflect.DeepEqual(models, wantModels) {
		t.Fatalf("hourly models = %+v, want %+v", models, wantModels)
	}
}

// Regression: session day keys came from SQL date(timestamp, 'localtime') but
// are matched against usage_rollup_buckets day keys, which follow the usage
// timezone, so public lookup charts pinned sessions to the wrong day.
func TestQueryPublicChartDataSessionsFollowUsageTimezone(t *testing.T) {
	CloseDB()
	// UTC+14 differs from both CI (UTC) and Asia/Shanghai machines, and this
	// check runs on the wall clock, so a regression shows up on either.
	loc := time.FixedZone("UTC+14", 14*3600)
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{StoreContent: true}, loc); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	assertPublicChartSessionsFollowUsageTimezone(t, loc)
}

// assertPublicChartSessionsFollowUsageTimezone expects the database to be
// initialised with loc as its usage timezone.
func assertPublicChartSessionsFollowUsageTimezone(t *testing.T, loc *time.Location) {
	t.Helper()
	const apiKey, apiKeyID = "sk-session-tz", "key-session-tz"
	if err := UpsertAPIKey(APIKeyRow{ID: apiKeyID, Key: apiKey, Name: "Session TZ"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	nowLocal := time.Now().In(loc)
	midnight := func(daysAgo int) time.Time {
		return time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day()-daysAgo, 0, 0, 0, 0, loc)
	}
	for sessionID, at := range map[string]time.Time{
		"session-yesterday-open":  midnight(1),
		"session-yesterday-close": midnight(0).Add(-time.Second),
		"session-today-open":      midnight(0),
	} {
		InsertLogWithDetailsIdentity(apiKey, apiKeyID, "Session TZ", "gpt-5.4", "codex", "Codex", "auth-session-tz", false, at, 10, 5,
			TokenStats{TotalTokens: 1}, "{}", "{}", `{"session_id":"`+sessionID+`"}`)
	}

	chart, err := QueryPublicChartData(apiKey, 7)
	if err != nil {
		t.Fatalf("QueryPublicChartData: %v", err)
	}
	byDate := make(map[string]DailyHeatmapPoint, len(chart.HeatmapSeries))
	for _, point := range chart.HeatmapSeries {
		byDate[point.Date] = point
	}
	yesterday, today := byDate[midnight(1).Format("2006-01-02")], byDate[midnight(0).Format("2006-01-02")]
	if yesterday.Requests != 2 || yesterday.Sessions != 2 || today.Requests != 1 || today.Sessions != 1 {
		t.Fatalf("heatmap = %+v, want yesterday 2 requests/2 sessions and today 1/1", chart.HeatmapSeries)
	}
	if chart.Stats.TotalSessions != 3 {
		t.Fatalf("total sessions = %d, want 3", chart.Stats.TotalSessions)
	}
}

func TestExtractSessionIDFromDetailsRecognizesXSessionID(t *testing.T) {
	detail := `{"client":{"headers":{"X-Session-Id":["zcode-session"],"Conversation-Id":["conversation"]}}}`
	if got := extractSessionIDFromDetails(detail); got != "zcode-session" {
		t.Fatalf("session_id = %q, want zcode-session", got)
	}
}

func TestExtractSessionIDFromDetailsRecognizesGrokSessionHeaders(t *testing.T) {
	t.Run("x-grok-session-id", func(t *testing.T) {
		detail := `{"client":{"headers":{"X-Grok-Session-Id":["019f5a53-5c9c-7222-a74c-3dbab60349d3"],"X-Grok-Conv-Id":["should-not-win"]}}}`
		want := "019f5a53-5c9c-7222-a74c-3dbab60349d3"
		if got := extractSessionIDFromDetails(detail); got != want {
			t.Fatalf("session_id = %q, want %s", got, want)
		}
	})

	t.Run("x-grok-conv-id fallback", func(t *testing.T) {
		detail := `{"client":{"fingerprint_headers":{"X-Grok-Conv-Id":["019f5a53-5c9c-7222-a74c-3dbab60349d3"]}}}`
		want := "019f5a53-5c9c-7222-a74c-3dbab60349d3"
		if got := extractSessionIDFromDetails(detail); got != want {
			t.Fatalf("session_id = %q, want %s", got, want)
		}
	})

	t.Run("generic session still beats grok conv", func(t *testing.T) {
		detail := `{"client":{"headers":{"Session-Id":["generic-session"],"X-Grok-Conv-Id":["grok-conv"]}}}`
		if got := extractSessionIDFromDetails(detail); got != "generic-session" {
			t.Fatalf("session_id = %q, want generic-session", got)
		}
	})
}
