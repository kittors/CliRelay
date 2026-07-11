package usage

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func makePseudoRandomText(size int) string {
	b := make([]byte, size)
	var x uint32 = 1
	for i := range b {
		x = 1664525*x + 1013904223
		b[i] = byte(32 + x%95)
	}
	return string(b)
}

func initTestUsageDB(t *testing.T, cfg config.RequestLogStorageConfig) {
	t.Helper()
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, cfg, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)
}

func TestSQLiteDatabaseSizeBytesIncludesWALAndSHM(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	files := map[string]string{
		dbPath:          "database",
		dbPath + "-wal": "wal",
		dbPath + "-shm": "shm",
	}
	var want int64
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		want += int64(len(content))
	}

	got, err := sqliteDatabaseSizeBytes(dbPath)
	if err != nil {
		t.Fatalf("sqliteDatabaseSizeBytes() error = %v", err)
	}
	if got != want {
		t.Fatalf("sqliteDatabaseSizeBytes() = %d, want %d", got, want)
	}
}

func TestCutoffStartUTCAtUsesProjectTimezoneForDayBoundaries(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	loc := time.FixedZone("UTC+8", 8*3600)
	if err := InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, loc); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	nowUTC := time.Date(2026, 3, 12, 1, 0, 0, 0, time.UTC) // 09:00 at UTC+8 (local date: 2026-03-12)

	got := cutoffStartUTCAt(nowUTC, 1)
	want := time.Date(2026, 3, 11, 16, 0, 0, 0, time.UTC) // local 2026-03-12 00:00 at UTC+8
	if !got.Equal(want) {
		t.Fatalf("cutoffStartUTCAt(days=1) = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	got = cutoffStartUTCAt(nowUTC, 2)
	want = time.Date(2026, 3, 10, 16, 0, 0, 0, time.UTC) // local 2026-03-11 00:00 at UTC+8
	if !got.Equal(want) {
		t.Fatalf("cutoffStartUTCAt(days=2) = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestQueryDailyCallsByAuthIndexesBucketsByProjectTimezone(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	loc := time.FixedZone("UTC+14", 14*3600)
	if err := InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, loc); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	nowLocal := time.Now().In(loc)
	localToday := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 30, 0, 0, loc)
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-local-day", false, localToday, 1, 1, TokenStats{TotalTokens: 1}, "", "")

	points, err := QueryDailyCallsByAuthIndexes([]string{"auth-local-day"}, 1)
	if err != nil {
		t.Fatalf("QueryDailyCallsByAuthIndexes() error = %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("points len = %d, want 1: %+v", len(points), points)
	}
	wantDate := localToday.Format("2006-01-02")
	if points[0].Date != wantDate {
		t.Fatalf("point date = %q, want local day %q", points[0].Date, wantDate)
	}
	if points[0].Requests != 1 {
		t.Fatalf("point requests = %d, want 1", points[0].Requests)
	}
}

func TestQueryEntityStatsFiltersByRequestedEntities(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	now := time.Now().UTC()
	InsertLog("", "", "gpt-5.4", "codex-a", "Codex A", "auth-a", false, now, 10, 1, TokenStats{TotalTokens: 11}, "", "")
	InsertLog("", "", "gpt-5.4", "codex-b", "Codex B", "auth-b", true, now, 20, 1, TokenStats{TotalTokens: 21}, "", "")
	InsertLog("", "", "gpt-5.4", "codex-c", "Codex C", "auth-c", false, now, 30, 1, TokenStats{TotalTokens: 31}, "", "")

	authStats, err := QueryEntityStats("", 30, "auth_index", []string{"auth-a", "auth-b"})
	if err != nil {
		t.Fatalf("QueryEntityStats(auth_index) error = %v", err)
	}
	if len(authStats) != 2 {
		t.Fatalf("auth stats len = %d, want 2: %+v", len(authStats), authStats)
	}
	for _, point := range authStats {
		if point.EntityName == "auth-c" {
			t.Fatalf("auth stats included unrequested auth-c: %+v", authStats)
		}
	}

	sourceStats, err := QueryEntityStats("", 30, "source", []string{"codex-b"})
	if err != nil {
		t.Fatalf("QueryEntityStats(source) error = %v", err)
	}
	if len(sourceStats) != 1 {
		t.Fatalf("source stats len = %d, want 1: %+v", len(sourceStats), sourceStats)
	}
	if sourceStats[0].EntityName != "codex-b" {
		t.Fatalf("source entity = %q, want codex-b", sourceStats[0].EntityName)
	}
	if sourceStats[0].Requests != 1 || sourceStats[0].Failed != 1 {
		t.Fatalf("source stats = %+v, want one failed request", sourceStats[0])
	}
}

func TestCacheRateUsesEffectiveInputTokenTotals(t *testing.T) {
	testCases := []struct {
		name string
		rows []TokenStats
		want float64
	}{
		{
			name: "cached tokens are part of input tokens",
			rows: []TokenStats{{InputTokens: 1000, CachedTokens: 400, TotalTokens: 1000}},
			want: 40,
		},
		{
			name: "cached tokens are reported separately from small input tokens",
			rows: []TokenStats{{InputTokens: 21, CachedTokens: 188086, TotalTokens: 188107}},
			want: float64(188086) / float64(188107) * 100,
		},
		{
			name: "cached tokens without raw input tokens",
			rows: []TokenStats{{CachedTokens: 250, TotalTokens: 250}},
			want: 100,
		},
		{
			name: "mixed provider semantics are handled per request row",
			rows: []TokenStats{
				{InputTokens: 100, TotalTokens: 100},
				{CachedTokens: 50, TotalTokens: 50},
			},
			want: float64(50) / float64(150) * 100,
		},
		{
			name: "zero token totals",
			rows: []TokenStats{{TotalTokens: 0}},
			want: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			initTestUsageDB(t, config.RequestLogStorageConfig{})

			now := time.Now().UTC()
			for index, row := range tc.rows {
				InsertLog(
					"sk-cache-rate",
					"",
					"gpt-5.4",
					"codex",
					"Codex",
					"auth-cache-rate",
					false,
					now.Add(time.Duration(index)*time.Second),
					1,
					0,
					row,
					"",
					"",
				)
			}

			stats, err := QueryStats(LogQueryParams{Days: 30})
			if err != nil {
				t.Fatalf("QueryStats() error = %v", err)
			}
			if math.Abs(stats.CacheRate-tc.want) > 0.000001 {
				t.Fatalf("QueryStats().CacheRate = %.9f, want %.9f", stats.CacheRate, tc.want)
			}

			kpi, err := QueryDashboardKPI(30)
			if err != nil {
				t.Fatalf("QueryDashboardKPI() error = %v", err)
			}
			if math.Abs(kpi.CacheRate-tc.want) > 0.000001 {
				t.Fatalf("QueryDashboardKPI().CacheRate = %.9f, want %.9f", kpi.CacheRate, tc.want)
			}
		})
	}
}

func TestQuotaSnapshotPointsKeepFineGrainedSeries(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	recordedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	resetAt := recordedAt.Add(5 * time.Hour)
	remaining := 100.0

	err := RecordQuotaSnapshotPoints("auth-1", "codex", []QuotaSnapshotPoint{
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "additional:codex_bengalfox:5h",
			QuotaLabel:    "GPT-5.3-Codex-Spark: 5h",
			Percent:       &remaining,
			ResetAt:       &resetAt,
			WindowSeconds: 18000,
		},
	})
	if err != nil {
		t.Fatalf("RecordQuotaSnapshotPoints() error = %v", err)
	}

	points, err := QueryQuotaSnapshotPoints("auth-1", recordedAt.Add(-time.Minute), recordedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("QueryQuotaSnapshotPoints() error = %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	point := points[0]
	if point.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", point.Provider)
	}
	if point.QuotaKey != "additional:codex_bengalfox:5h" {
		t.Fatalf("QuotaKey = %q", point.QuotaKey)
	}
	if point.QuotaLabel != "GPT-5.3-Codex-Spark: 5h" {
		t.Fatalf("QuotaLabel = %q", point.QuotaLabel)
	}
	if point.Percent == nil || *point.Percent != 100 {
		t.Fatalf("Percent = %v, want 100", point.Percent)
	}
	if point.ResetAt == nil || !point.ResetAt.Equal(resetAt) {
		t.Fatalf("ResetAt = %v, want %v", point.ResetAt, resetAt)
	}
	if point.WindowSeconds != 18000 {
		t.Fatalf("WindowSeconds = %d, want 18000", point.WindowSeconds)
	}
}

func TestRecordQuotaSnapshotPointsIdentityStoresWeeklyCycleBySubject(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	recordedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	resetAt := recordedAt.Add(6 * 24 * time.Hour)
	remaining := 93.0

	err := RecordQuotaSnapshotPointsIdentity("auth-pro", "authsub_test", "codex", []QuotaSnapshotPoint{
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "code_week",
			QuotaLabel:    "m_quota.code_weekly",
			Percent:       &remaining,
			ResetAt:       &resetAt,
			WindowSeconds: 604800,
		},
	})
	if err != nil {
		t.Fatalf("RecordQuotaSnapshotPointsIdentity() error = %v", err)
	}

	cycle, err := QueryLatestWeeklyQuotaCycleByAuthSubject("authsub_test")
	if err != nil {
		t.Fatalf("QueryLatestWeeklyQuotaCycleByAuthSubject() error = %v", err)
	}
	if cycle == nil {
		t.Fatal("expected weekly cycle, got nil")
	}
	if cycle.AuthIndex != "auth-pro" {
		t.Fatalf("AuthIndex = %q, want auth-pro", cycle.AuthIndex)
	}
	if cycle.QuotaKey != "code_week" {
		t.Fatalf("QuotaKey = %q, want code_week", cycle.QuotaKey)
	}
	wantStart := resetAt.Add(-7 * 24 * time.Hour)
	if !cycle.CycleStartAt.Equal(wantStart) {
		t.Fatalf("CycleStartAt = %s, want %s", cycle.CycleStartAt.Format(time.RFC3339), wantStart.Format(time.RFC3339))
	}
	if !cycle.ResetAt.Equal(resetAt) {
		t.Fatalf("ResetAt = %s, want %s", cycle.ResetAt.Format(time.RFC3339), resetAt.Format(time.RFC3339))
	}
}

func TestQueryLatestWeeklyQuotaCycleByAuthSubjectFiltersPreferredQuotaKeys(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	recordedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	codeResetAt := recordedAt.Add(6 * 24 * time.Hour)
	additionalResetAt := codeResetAt.Add(3 * time.Hour)
	codeRemaining := 99.0
	additionalRemaining := 100.0

	err := RecordQuotaSnapshotPointsIdentity("auth-pro", "authsub_test", "codex", []QuotaSnapshotPoint{
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "code_week",
			QuotaLabel:    "m_quota.code_weekly",
			Percent:       &codeRemaining,
			ResetAt:       &codeResetAt,
			WindowSeconds: 604800,
		},
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "additional:codex_bengalfox:week",
			QuotaLabel:    "GPT-5.3-Codex-Spark: Weekly",
			Percent:       &additionalRemaining,
			ResetAt:       &additionalResetAt,
			WindowSeconds: 604800,
		},
	})
	if err != nil {
		t.Fatalf("RecordQuotaSnapshotPointsIdentity() error = %v", err)
	}

	cycle, err := QueryLatestWeeklyQuotaCycleByAuthSubject("authsub_test", "code_week")
	if err != nil {
		t.Fatalf("QueryLatestWeeklyQuotaCycleByAuthSubject() error = %v", err)
	}
	if cycle == nil {
		t.Fatal("expected filtered weekly cycle, got nil")
	}
	if cycle.QuotaKey != "code_week" {
		t.Fatalf("QuotaKey = %q, want code_week", cycle.QuotaKey)
	}
	if !cycle.CycleStartAt.Equal(codeResetAt.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("CycleStartAt = %s, want %s", cycle.CycleStartAt.Format(time.RFC3339), codeResetAt.Add(-7*24*time.Hour).Format(time.RFC3339))
	}
}

func TestQueryUsageByAuthSubjectMatchesLegacyEmailRows(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	auth := &coreauth.Auth{
		ID:       "codex-pro-auth",
		FileName: "codex-user@example.com-pro.json",
		Provider: "codex",
		Label:    "user@example.com",
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct_user_1",
			"plan_type":  "pro",
		},
	}
	auth.EnsureIndex()
	identity := ResolveAuthSubjectIdentity(auth)
	if identity == nil || identity.ID == "" {
		t.Fatalf("ResolveAuthSubjectIdentity() = %+v, want non-empty subject id", identity)
	}
	matcher := BuildAuthSubjectMatcher(auth, []*coreauth.Auth{auth})

	if err := UpsertModelPricing("gpt-5.4", 1, 2, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}

	now := time.Now().UTC()
	start := now.Add(-24 * time.Hour)
	InsertLog("", "", "gpt-5.4", "user@example.com", "user@example.com", "legacy-plus-index", false, now.Add(-2*time.Hour), 1, 1, TokenStats{
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalTokens:  3000,
	}, "", "")
	InsertLogWithDetailsIdentitySubject("", "", identity.ID, "", "gpt-5.4", "user@example.com", "user@example.com", auth.Index, false, now.Add(-time.Hour), 1, 1, TokenStats{
		InputTokens:  1000,
		OutputTokens: 1000,
		TotalTokens:  2000,
	}, "", "", "")

	count, err := QueryRequestCountByAuthSubjectSince(matcher, start)
	if err != nil {
		t.Fatalf("QueryRequestCountByAuthSubjectSince() error = %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	cost, err := QueryCostByAuthSubjectSince(matcher, start)
	if err != nil {
		t.Fatalf("QueryCostByAuthSubjectSince() error = %v", err)
	}
	if math.Abs(cost-0.008) > 1e-12 {
		t.Fatalf("cost = %v, want 0.008", cost)
	}

	daily, err := QueryDailyCallsByAuthSubject(matcher, 2)
	if err != nil {
		t.Fatalf("QueryDailyCallsByAuthSubject() error = %v", err)
	}
	var total int64
	for _, point := range daily {
		total += point.Requests
	}
	if total != 2 {
		t.Fatalf("daily total requests = %d, want 2 (%+v)", total, daily)
	}
}

func TestQueryLogsSupportsSystemRequestLogFilterValue(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	now := time.Now().UTC()
	InsertLog("POST /image-generation/test", "", "gpt-image-2", "codex", "Codex", "auth-1", false, now, 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "")
	InsertLog("/v0/management/image-generation/test", "", "gpt-image-2", "codex", "Codex", "auth-2", true, now, 120, 12, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "")
	InsertLog("sk-live-123", "Primary", "gpt-5.4", "codex", "Codex", "auth-3", false, now, 140, 14, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "")

	result, err := QueryLogs(LogQueryParams{
		Page:   1,
		Size:   10,
		Days:   1,
		APIKey: systemRequestLogFilterValue,
	})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("system filter items = %d, want 2", len(result.Items))
	}
	for _, item := range result.Items {
		if item.APIKey == "sk-live-123" {
			t.Fatalf("unexpected non-system api key in system filter result: %q", item.APIKey)
		}
	}
}

func TestQueryLogsSupportsExplicitEmptyMultiSelectFilters(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	now := time.Now().UTC()
	InsertLog("sk-live-123", "Primary", "gpt-5.4", "codex", "Codex", "auth-1", false, now, 140, 14, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "")

	result, err := QueryLogs(LogQueryParams{
		Page:           1,
		Size:           10,
		Days:           1,
		MatchNoAPIKeys: true,
	})
	if err != nil {
		t.Fatalf("QueryLogs() with MatchNoAPIKeys error = %v", err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("MatchNoAPIKeys items = %d, want 0", len(result.Items))
	}
	if result.Total != 0 {
		t.Fatalf("MatchNoAPIKeys total = %d, want 0", result.Total)
	}

	stats, err := QueryStats(LogQueryParams{
		Page:          1,
		Size:          10,
		Days:          1,
		MatchNoModels: true,
	})
	if err != nil {
		t.Fatalf("QueryStats() with MatchNoModels error = %v", err)
	}
	if stats.Total != 0 {
		t.Fatalf("MatchNoModels stats.Total = %d, want 0", stats.Total)
	}
	if stats.TotalTokens != 0 || stats.TotalCost != 0 || stats.SuccessRate != 0 {
		t.Fatalf("MatchNoModels stats = %+v, want all zero values", stats)
	}
}

func TestQueryLogsReturnsVisionFallbackModelSeparately(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	now := time.Now().UTC()
	InsertLogWithDetailsIdentitySubjectUpstreamVision(
		"sk-live-123",
		"key-1",
		"subject-1",
		"Primary",
		"alias-model",
		"real-model",
		"vision-model",
		"codex",
		"Codex",
		"auth-1",
		false,
		now,
		140,
		14,
		TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		"",
		"",
		"",
	)

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(result.Items))
	}
	row := result.Items[0]
	if row.Model != "alias-model" {
		t.Fatalf("Model = %q, want alias-model", row.Model)
	}
	if row.UpstreamModel != "real-model" {
		t.Fatalf("UpstreamModel = %q, want real-model", row.UpstreamModel)
	}
	if row.VisionFallbackModel != "vision-model" {
		t.Fatalf("VisionFallbackModel = %q, want vision-model", row.VisionFallbackModel)
	}
}

func TestQueryLogContentKeepsMissingFailedOutputEmpty(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	now := time.Now().UTC()
	input := `{"model":"gpt-image-2","prompt":"draw a fox"}`
	InsertLog("POST /image-generation/test", "", "gpt-image-2", "codex", "Codex", "auth-1", true, now, 100, 10, TokenStats{}, input, "")

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}

	content, err := QueryLogContent(result.Items[0].ID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input {
		t.Fatalf("InputContent = %q, want %q", content.InputContent, input)
	}
	if content.OutputContent != "" {
		t.Fatalf("OutputContent = %q, want empty historical missing output", content.OutputContent)
	}

	part, err := QueryLogContentPart(result.Items[0].ID, "output")
	if err != nil {
		t.Fatalf("QueryLogContentPart() error = %v", err)
	}
	if part.Content != "" {
		t.Fatalf("part.Content = %q, want empty historical missing output", part.Content)
	}
}

func TestQueryLogContentPartReturnsStoredRequestDetails(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	now := time.Now().UTC()
	details := `{"client":{"ip":"203.0.113.8","headers":{"Authorization":"Bearer sk-client-plaintext"}},"upstream":{"headers":{"Authorization":"Bearer sk-upstream-plaintext"}},"response":{"headers":{"X-Request-Id":"req-plaintext"}}}`
	InsertLogWithDetails("sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1", false, now, 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, `{"messages":[]}`, `{"choices":[]}`, details)

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}

	part, err := QueryLogContentPart(result.Items[0].ID, "details")
	if err != nil {
		t.Fatalf("QueryLogContentPart(details) error = %v", err)
	}
	if part.Part != "details" {
		t.Fatalf("part.Part = %q, want details", part.Part)
	}
	if part.Content != details {
		t.Fatalf("details content = %q, want %q", part.Content, details)
	}
}

func TestBodyStorageDisabledPreservesDetailsWithoutEmbeddedBodies(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           false,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	details := `{"client":{"ip":"203.0.113.8"},"upstream":{"request_log":"=== API REQUEST 1 ===\nHeaders:\nX-Test: yes\nBody:\nsecret-request"},"response":{"upstream_log":"=== API RESPONSE 1 ===\nStatus: 200\nBody:\nsecret-response"}}`
	InsertLogWithDetails("sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1", false, time.Now().UTC(), 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, `{"secret":"request"}`, `{"secret":"response"}`, details)

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("QueryLogs() result=%+v error=%v", result, err)
	}
	input, err := QueryLogContentPart(result.Items[0].ID, "input")
	if err != nil {
		t.Fatalf("QueryLogContentPart(input) error = %v", err)
	}
	if input.Content != "" {
		t.Fatalf("input content = %q, want empty", input.Content)
	}
	detail, err := QueryLogContentPart(result.Items[0].ID, "details")
	if err != nil {
		t.Fatalf("QueryLogContentPart(details) error = %v", err)
	}
	if !strings.Contains(detail.Content, "203.0.113.8") || !strings.Contains(detail.Content, "X-Test: yes") {
		t.Fatalf("detail content lost diagnostic metadata: %q", detail.Content)
	}
	if strings.Contains(detail.Content, "secret-request") || strings.Contains(detail.Content, "secret-response") {
		t.Fatalf("detail content retained request/response bodies: %q", detail.Content)
	}
}

func TestInitDBMigratesFirstTokenAndStreamingColumns(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")

	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE request_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			api_key TEXT NOT NULL DEFAULT '',
			api_key_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			channel_name TEXT NOT NULL DEFAULT '',
			auth_index TEXT NOT NULL DEFAULT '',
			failed INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0,
			input_content TEXT NOT NULL DEFAULT '',
			output_content TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		t.Fatalf("create legacy request_logs table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	db := getDB()
	found := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(request_logs)")
	if err != nil {
		t.Fatalf("PRAGMA table_info(request_logs): %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table info: %v", err)
		}
		if name == "first_token_ms" || name == "streaming" || name == "vision_fallback_model" {
			found[name] = true
		}
	}
	if !found["first_token_ms"] {
		t.Fatalf("expected first_token_ms column to exist after InitDB migration")
	}
	if !found["streaming"] {
		t.Fatalf("expected streaming column to exist after InitDB migration")
	}
	if !found["vision_fallback_model"] {
		t.Fatalf("expected vision_fallback_model column to exist after InitDB migration")
	}
}

func TestInitDBEnsuresRequestLogLookupIndexes(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	rows, err := getDB().Query("PRAGMA index_list(request_logs)")
	if err != nil {
		t.Fatalf("PRAGMA index_list(request_logs): %v", err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_list: %v", err)
	}

	for _, name := range []string{
		"idx_logs_api_key_timestamp",
		"idx_logs_api_key_id_timestamp",
		"idx_logs_api_key_chart_cover",
		"idx_logs_api_key_id_chart_cover",
	} {
		if !found[name] {
			t.Fatalf("expected %s to exist after InitDB", name)
		}
	}
}

func TestInitDBMigratesAuthSubjectColumnsAndCycleTable(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")

	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE request_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			api_key TEXT NOT NULL DEFAULT '',
			api_key_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			channel_name TEXT NOT NULL DEFAULT '',
			auth_index TEXT NOT NULL DEFAULT '',
			failed INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			first_token_ms INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0,
			input_content TEXT NOT NULL DEFAULT '',
			output_content TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE auth_file_quota_snapshots (
			date_key TEXT NOT NULL,
			auth_index TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			quota_key TEXT NOT NULL,
			percent REAL,
			recorded_at DATETIME NOT NULL,
			PRIMARY KEY (date_key, auth_index, quota_key)
		);
		CREATE TABLE auth_file_quota_snapshot_points (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			recorded_at DATETIME NOT NULL,
			auth_index TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			quota_key TEXT NOT NULL,
			quota_label TEXT NOT NULL DEFAULT '',
			percent REAL,
			reset_at DATETIME,
			window_seconds INTEGER NOT NULL DEFAULT 0
		);
	`); err != nil {
		t.Fatalf("create legacy tables: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	db := getDB()
	assertColumnExists := func(table string, want string) {
		t.Helper()
		rows, err := db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatalf("PRAGMA table_info(%s): %v", table, err)
		}
		defer rows.Close()

		for rows.Next() {
			var cid int
			var name, dataType string
			var notNull int
			var defaultValue any
			var pk int
			if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
				t.Fatalf("scan table info for %s: %v", table, err)
			}
			if name == want {
				return
			}
		}
		t.Fatalf("expected %s.%s to exist after InitDB migration", table, want)
	}

	assertColumnExists("request_logs", "auth_subject_id")
	assertColumnExists("auth_file_quota_snapshots", "auth_subject_id")
	assertColumnExists("auth_file_quota_snapshot_points", "auth_subject_id")

	var cycleTableName string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'auth_subject_quota_cycles'").Scan(&cycleTableName); err != nil {
		t.Fatalf("query auth_subject_quota_cycles existence: %v", err)
	}
	if cycleTableName != "auth_subject_quota_cycles" {
		t.Fatalf("cycle table name = %q, want auth_subject_quota_cycles", cycleTableName)
	}
}

func TestInsertLogStoresCompressedContentOutsideMainTable(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	timestamp := time.Now().UTC()
	input := `{"messages":[{"role":"user","content":"hello world"}]}`
	output := `{"id":"resp_123","output":"done"}`

	InsertLog("sk-test", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 123, 45, TokenStats{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
	}, input, output)

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}
	if result.Items[0].FirstTokenMs != 45 {
		t.Fatalf("FirstTokenMs = %d, want %d", result.Items[0].FirstTokenMs, 45)
	}
	if result.Items[0].Streaming {
		t.Fatalf("Streaming = true, want false for non-stream request")
	}
	if !result.Items[0].HasContent {
		t.Fatalf("expected HasContent to be true")
	}

	content, err := QueryLogContent(result.Items[0].ID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input {
		t.Fatalf("InputContent = %q, want %q", content.InputContent, input)
	}
	if content.OutputContent != output {
		t.Fatalf("OutputContent = %q, want %q", content.OutputContent, output)
	}

	db := getDB()
	var legacyInput, legacyOutput string
	if err := db.QueryRow(
		"SELECT input_content, output_content FROM request_logs WHERE id = ?",
		result.Items[0].ID,
	).Scan(&legacyInput, &legacyOutput); err != nil {
		t.Fatalf("query legacy columns: %v", err)
	}
	if legacyInput != "" || legacyOutput != "" {
		t.Fatalf("expected main table content columns to be empty, got input=%q output=%q", legacyInput, legacyOutput)
	}

	var compressedInput, compressedOutput []byte
	if err := db.QueryRow(
		"SELECT input_content, output_content FROM request_log_content WHERE log_id = ?",
		result.Items[0].ID,
	).Scan(&compressedInput, &compressedOutput); err != nil {
		t.Fatalf("query compressed content row: %v", err)
	}
	if len(compressedInput) == 0 || len(compressedOutput) == 0 {
		t.Fatalf("expected compressed content blobs to be present")
	}
}

func TestQueryLogsMarksStreamingRequests(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	timestamp := time.Now().UTC()
	InsertLog("sk-test", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 123, 45, TokenStats{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
	}, `{"model":"gpt-test","stream":true}`, "")

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}
	if !result.Items[0].Streaming {
		t.Fatalf("Streaming = false, want true")
	}
}

func TestQueryLogsHydratesLegacyStreamingFlagFromContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})

	timestamp := time.Now().UTC()
	InsertLog("sk-test", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 123, 45, TokenStats{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
	}, `{"model":"gpt-test","stream":true}`, "")

	db := getDB()
	if _, err := db.Exec("UPDATE request_logs SET streaming = 0"); err != nil {
		t.Fatalf("reset streaming flag: %v", err)
	}

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}
	if !result.Items[0].Streaming {
		t.Fatalf("Streaming = false, want true from stored content")
	}
}

func TestQueryLogsHydratesLegacyStreamingFlagFromInlineContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	db := getDB()
	timestamp := time.Now().UTC()
	_, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-legacy", "gpt-test", "source", "channel", "auth-legacy",
		0, 123, 10, 20, 0, 0, 30, 0, `{"model":"gpt-test","stream":true}`,
	)
	if err != nil {
		t.Fatalf("insert legacy inline row: %v", err)
	}

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}
	if !result.Items[0].Streaming {
		t.Fatalf("Streaming = false, want true from inline content")
	}
}

func TestMigrateLegacyContentBatchMovesContentOutOfMainTable(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	db := getDB()
	timestamp := time.Now().UTC()
	input := "legacy-input"
	output := "legacy-output"

	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-legacy", "legacy-model", "legacy-source", "legacy-channel", "auth-legacy",
		0, 10, 1, 2, 0, 0, 3, 0, input, output,
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}

	migrated, err := migrateLegacyContentBatch(db, 100)
	if err != nil {
		t.Fatalf("migrateLegacyContentBatch() error = %v", err)
	}
	if migrated != 1 {
		t.Fatalf("migrated = %d, want 1", migrated)
	}

	content, err := QueryLogContent(logID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input || content.OutputContent != output {
		t.Fatalf("unexpected migrated content: %+v", content)
	}

	var legacyInput, legacyOutput string
	if err := db.QueryRow(
		"SELECT input_content, output_content FROM request_logs WHERE id = ?",
		logID,
	).Scan(&legacyInput, &legacyOutput); err != nil {
		t.Fatalf("query cleared legacy columns: %v", err)
	}
	if legacyInput != "" || legacyOutput != "" {
		t.Fatalf("expected legacy columns cleared after migration, got input=%q output=%q", legacyInput, legacyOutput)
	}
}

func TestCleanupExpiredLogContentKeepsMetadataRows(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	timestamp := time.Now().UTC().AddDate(0, 0, -40)
	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-old", "old-model", "source", "channel", "auth-old",
		0, 5, 1, 1, 0, 0, 2, 0,
	)
	if err != nil {
		t.Fatalf("insert metadata row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if err := insertLogContentTx(tx, logID, timestamp, "expired-input", "expired-output", ""); err != nil {
		t.Fatalf("insertLogContentTx() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	_, err = cleanupExpiredLogContent(db)
	if err != nil {
		t.Fatalf("cleanupExpiredLogContent() error = %v", err)
	}

	var metadataRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE id = ?", logID).Scan(&metadataRows); err != nil {
		t.Fatalf("count metadata rows: %v", err)
	}
	if metadataRows != 1 {
		t.Fatalf("metadata row count = %d, want 1", metadataRows)
	}

	var contentRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&contentRows); err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	if contentRows != 0 {
		t.Fatalf("content row count = %d, want 0", contentRows)
	}
}

func TestGetRequestLogStorageBytesCountsCompressedAndLegacyContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	timestamp := time.Now().UTC()
	input := `{"messages":[{"role":"user","content":"hello world"}]}`
	output := `{"id":"resp_123","output":"done"}`

	InsertLog("sk-test", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 123, 33, TokenStats{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
	}, input, output)

	db := getDB()
	var compressedInputBytes, compressedOutputBytes int64
	if err := db.QueryRow(
		`SELECT length(input_content), length(output_content)
		 FROM request_log_content
		 ORDER BY log_id DESC
		 LIMIT 1`,
	).Scan(&compressedInputBytes, &compressedOutputBytes); err != nil {
		t.Fatalf("query compressed content lengths: %v", err)
	}

	legacyInput := "legacy-inline-input"
	legacyOutput := "legacy-inline-output"
	if _, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-legacy", "legacy-model", "legacy-source", "legacy-channel", "auth-legacy",
		0, 10, 1, 2, 0, 0, 3, 0, legacyInput, legacyOutput,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	totalBytes, err := GetRequestLogStorageBytes()
	if err != nil {
		t.Fatalf("GetRequestLogStorageBytes() error = %v", err)
	}

	want := compressedInputBytes + compressedOutputBytes + int64(len(legacyInput)+len(legacyOutput))
	if totalBytes != want {
		t.Fatalf("GetRequestLogStorageBytes() = %d, want %d", totalBytes, want)
	}
}

func TestMigrateLegacyContentBatchKeepsAllContentWhenRetentionUnlimited(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   0,
		CleanupIntervalMinutes: 1440,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	timestamp := time.Now().UTC().AddDate(0, 0, -90)
	input := "legacy-unlimited-input"
	output := "legacy-unlimited-output"

	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-legacy", "legacy-model", "legacy-source", "legacy-channel", "auth-legacy",
		0, 10, 1, 2, 0, 0, 3, 0, input, output,
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}

	migrated, err := migrateLegacyContentBatch(db, 100)
	if err != nil {
		t.Fatalf("migrateLegacyContentBatch() error = %v", err)
	}
	if migrated != 1 {
		t.Fatalf("migrated = %d, want 1", migrated)
	}

	content, err := QueryLogContent(logID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input || content.OutputContent != output {
		t.Fatalf("unexpected migrated content: %+v", content)
	}
}

func TestMigrateLegacyContentBatchPreservesInlineContentWhenStorageDisabled(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           false,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	timestamp := time.Now().UTC()
	input := "legacy-inline-input"
	output := "legacy-inline-output"

	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-inline", "inline-model", "inline-source", "inline-channel", "auth-inline",
		0, 10, 1, 2, 0, 0, 3, 0, input, output,
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}

	migrated, err := migrateLegacyContentBatch(db, 100)
	if err != nil {
		t.Fatalf("migrateLegacyContentBatch() error = %v", err)
	}
	if migrated != 0 {
		t.Fatalf("migrated = %d, want 0", migrated)
	}

	var inlineInput, inlineOutput string
	if err := db.QueryRow(
		"SELECT input_content, output_content FROM request_logs WHERE id = ?",
		logID,
	).Scan(&inlineInput, &inlineOutput); err != nil {
		t.Fatalf("query legacy inline content: %v", err)
	}
	if inlineInput != input || inlineOutput != output {
		t.Fatalf("legacy inline content changed: input=%q output=%q", inlineInput, inlineOutput)
	}

	var contentRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&contentRows); err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	if contentRows != 0 {
		t.Fatalf("content row count = %d, want 0", contentRows)
	}

	content, err := QueryLogContent(logID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input || content.OutputContent != output {
		t.Fatalf("unexpected fallback content: %+v", content)
	}
}

func TestCleanupExpiredLogContentSkipsWhenRetentionUnlimited(t *testing.T) {
	testCases := []struct {
		name string
		cfg  config.RequestLogStorageConfig
	}{
		{
			name: "retention unlimited",
			cfg: config.RequestLogStorageConfig{
				StoreContent:           true,
				ContentRetentionDays:   0,
				CleanupIntervalMinutes: 1440,
				VacuumOnCleanup:        false,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			initTestUsageDB(t, tc.cfg)

			db := getDB()
			timestamp := time.Now().UTC().AddDate(0, 0, -40)
			result, err := db.Exec(
				`INSERT INTO request_logs
					(timestamp, api_key, model, source, channel_name, auth_index,
					 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				timestamp.Format(time.RFC3339Nano),
				"sk-old", "old-model", "source", "channel", "auth-old",
				0, 5, 1, 1, 0, 0, 2, 0,
			)
			if err != nil {
				t.Fatalf("insert metadata row: %v", err)
			}
			logID, err := result.LastInsertId()
			if err != nil {
				t.Fatalf("LastInsertId() error = %v", err)
			}

			inputCompressed, err := compressLogContent("expired-input")
			if err != nil {
				t.Fatalf("compressLogContent(input) error = %v", err)
			}
			outputCompressed, err := compressLogContent("expired-output")
			if err != nil {
				t.Fatalf("compressLogContent(output) error = %v", err)
			}
			if _, err := db.Exec(
				`INSERT INTO request_log_content (log_id, timestamp, compression, input_content, output_content)
				 VALUES (?, ?, ?, ?, ?)`,
				logID,
				timestamp.Format(time.RFC3339Nano),
				requestLogContentCompression,
				inputCompressed,
				outputCompressed,
			); err != nil {
				t.Fatalf("insert request_log_content row: %v", err)
			}

			deleted, err := cleanupExpiredLogContent(db)
			if err != nil {
				t.Fatalf("cleanupExpiredLogContent() error = %v", err)
			}
			if deleted != 0 {
				t.Fatalf("deleted = %d, want 0", deleted)
			}

			var contentRows int
			if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&contentRows); err != nil {
				t.Fatalf("count content rows: %v", err)
			}
			if contentRows != 1 {
				t.Fatalf("content row count = %d, want 1", contentRows)
			}
		})
	}
}

func TestCleanupOversizedLogContentPrunesOldestRows(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         1,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	maxBytes := int64(1024 * 1024)
	payload := makePseudoRandomText(420 * 1024)
	compressed, err := compressLogContent(payload)
	if err != nil {
		t.Fatalf("compressLogContent() error = %v", err)
	}
	rowBytes := int64(len(compressed))
	if rowBytes >= maxBytes {
		t.Fatalf("test payload compressed too large: %d", rowBytes)
	}
	if rowBytes*3 <= maxBytes {
		t.Fatalf("test payload compressed too small to exceed cap: %d", rowBytes)
	}

	insertRawContentRow := func(ts time.Time, apiKey string) int64 {
		t.Helper()
		result, err := db.Exec(
			`INSERT INTO request_logs
				(timestamp, api_key, model, source, channel_name, auth_index,
				 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ts.Format(time.RFC3339Nano),
			apiKey, "model", "source", "channel", apiKey,
			0, 5, 1, 1, 0, 0, 2, 0,
		)
		if err != nil {
			t.Fatalf("insert request_logs row: %v", err)
		}
		logID, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId() error = %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO request_log_content (log_id, timestamp, compression, input_content, output_content)
			 VALUES (?, ?, ?, ?, ?)`,
			logID,
			ts.Format(time.RFC3339Nano),
			requestLogContentCompression,
			compressed,
			[]byte{},
		); err != nil {
			t.Fatalf("insert request_log_content row: %v", err)
		}
		return logID
	}

	oldestID := insertRawContentRow(time.Now().UTC().Add(-3*time.Hour), "sk-oldest")
	_ = insertRawContentRow(time.Now().UTC().Add(-2*time.Hour), "sk-middle")
	newestID := insertRawContentRow(time.Now().UTC().Add(-1*time.Hour), "sk-newest")

	deleted, err := cleanupOversizedLogContent(db, maxBytes)
	if err != nil {
		t.Fatalf("cleanupOversizedLogContent() error = %v", err)
	}
	if deleted == 0 {
		t.Fatalf("expected oversized cleanup to delete at least one row")
	}

	totalBytes, err := queryStoredContentBytes(db)
	if err != nil {
		t.Fatalf("queryStoredContentBytes() error = %v", err)
	}
	if totalBytes > maxBytes {
		t.Fatalf("total stored bytes = %d, want <= %d", totalBytes, maxBytes)
	}

	var oldestRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", oldestID).Scan(&oldestRows); err != nil {
		t.Fatalf("count oldest row: %v", err)
	}
	if oldestRows != 0 {
		t.Fatalf("expected oldest row to be pruned, count=%d", oldestRows)
	}

	var newestRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", newestID).Scan(&newestRows); err != nil {
		t.Fatalf("count newest row: %v", err)
	}
	if newestRows != 1 {
		t.Fatalf("expected newest row to remain, count=%d", newestRows)
	}
}

func TestInsertLogContentTxSkipsSingleRowLargerThanSizeCap(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         1,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	maxBytes := int64(1024 * 1024)
	payload := makePseudoRandomText(2 * 1024 * 1024)
	compressed, err := compressLogContent(payload)
	if err != nil {
		t.Fatalf("compressLogContent() error = %v", err)
	}
	if int64(len(compressed)) <= maxBytes {
		t.Fatalf("expected compressed payload to exceed cap, got %d", len(compressed))
	}

	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
		"sk-large", "model", "source", "channel", "auth-large",
		0, 5, 1, 1, 0, 0, 2, 0,
	)
	if err != nil {
		t.Fatalf("insert request_logs row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if err := insertLogContentTx(tx, logID, time.Now().UTC(), payload, "", ""); err != nil {
		t.Fatalf("insertLogContentTx() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	var contentRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&contentRows); err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	if contentRows != 0 {
		t.Fatalf("content row count = %d, want 0", contentRows)
	}
}

func TestDeleteLogsByAPIKeyRemovesLogsAndContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	timestamp := time.Now().UTC()
	input := `{"messages":[{"role":"user","content":"hello"}]}`
	output := `{"id":"resp_1","output":"done"}`

	// Insert 3 logs: 2 for "sk-target", 1 for "sk-other"
	InsertLog("sk-target", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 100, 10, TokenStats{
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
	}, input, output)
	InsertLog("sk-target", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 200, 20, TokenStats{
		InputTokens: 15, OutputTokens: 25, TotalTokens: 40,
	}, input, output)
	InsertLog("sk-other", "", "gpt-test", "source", "channel", "auth-2", false, timestamp, 300, 30, TokenStats{
		InputTokens: 5, OutputTokens: 10, TotalTokens: 15,
	}, input, output)

	// Verify all inserted
	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 3 {
		t.Fatalf("expected 3 log rows, got %d", len(result.Items))
	}

	// Delete logs for sk-target
	deleted, err := DeleteLogsByAPIKey("sk-target")
	if err != nil {
		t.Fatalf("DeleteLogsByAPIKey() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("DeleteLogsByAPIKey() deleted = %d, want 2", deleted)
	}

	// Verify only sk-other remains
	result, err = QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() after delete error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row after delete, got %d", len(result.Items))
	}
	if result.Items[0].APIKey != "sk-other" {
		t.Fatalf("remaining log api_key = %q, want sk-other", result.Items[0].APIKey)
	}

	// Verify content rows are also deleted for sk-target
	db := getDB()
	var contentCount int
	err = db.QueryRow("SELECT COUNT(*) FROM request_log_content").Scan(&contentCount)
	if err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	// Only sk-other's content should remain (1 row)
	if contentCount != 1 {
		t.Fatalf("expected 1 content row (sk-other only), got %d", contentCount)
	}
}

func TestBackfillRequestLogAPIKeyIDsUsesUniqueAPIKeyName(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	if err := UpsertAPIKey(APIKeyRow{ID: "stable-key-1", Key: "sk-current", Name: "袁蔚"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-current): %v", err)
	}

	db := getDB()
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_name, model, source, channel_name, auth_index,
			 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp, "sk-legacy", "袁蔚", "gpt-test", "source", "channel", "auth-1",
		0, 123, 45, 10, 20, 0, 0, 30, 0,
	); err != nil {
		t.Fatalf("insert legacy request_log: %v", err)
	}

	backfillRequestLogAPIKeyIDs(db)

	var apiKeyID string
	if err := db.QueryRow("SELECT api_key_id FROM request_logs WHERE api_key = ?", "sk-legacy").Scan(&apiKeyID); err != nil {
		t.Fatalf("query api_key_id: %v", err)
	}
	if apiKeyID != "stable-key-1" {
		t.Fatalf("api_key_id = %q, want stable-key-1", apiKeyID)
	}
}

func TestBackfillRequestLogAPIKeyIDsUsesHistoricalRawKeyIdentity(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	if err := UpsertAPIKey(APIKeyRow{ID: "stable-key-1", Key: "sk-current", Name: "袁蔚"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-current): %v", err)
	}

	db := getDB()
	firstTS := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	secondTS := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_id, api_key_name, model, source, channel_name, auth_index,
			 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		firstTS, "sk-legacy", "stable-key-1", "袁蔚", "gpt-test", "source", "channel", "auth-1",
		0, 123, 45, 10, 20, 0, 0, 30, 0,
	); err != nil {
		t.Fatalf("insert resolved request_log: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_id, api_key_name, model, source, channel_name, auth_index,
			 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		secondTS, "sk-legacy", "", "", "gpt-test", "source", "channel", "auth-1",
		0, 123, 45, 10, 20, 0, 0, 30, 0,
	); err != nil {
		t.Fatalf("insert orphan request_log: %v", err)
	}

	backfillRequestLogAPIKeyIDs(db)

	var apiKeyID string
	if err := db.QueryRow(
		"SELECT api_key_id FROM request_logs WHERE api_key = ? AND timestamp = ?",
		"sk-legacy", secondTS,
	).Scan(&apiKeyID); err != nil {
		t.Fatalf("query orphan api_key_id: %v", err)
	}
	if apiKeyID != "stable-key-1" {
		t.Fatalf("orphan api_key_id = %q, want stable-key-1", apiKeyID)
	}
}

func TestUniqueRequestLogAPIKeyIDByKeyOnlyChecksMissingRawKeys(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	db := getDB()
	now := time.Now().UTC()
	InsertLogWithDetailsIdentity("sk-missing", "missing-id", "", "gpt-test", "source", "channel", "auth-1", false, now, 1, 1, TokenStats{TotalTokens: 1}, "", "", "")
	InsertLogWithDetailsIdentity("sk-unrelated", "unrelated-id", "", "gpt-test", "source", "channel", "auth-1", false, now, 1, 1, TokenStats{TotalTokens: 1}, "", "", "")
	if _, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_id, api_key_name, model, source, channel_name, auth_index,
			 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now.Add(time.Second).Format(time.RFC3339Nano), "sk-missing", "", "", "gpt-test", "source", "channel", "auth-1",
		0, 123, 45, 10, 20, 0, 0, 30, 0,
	); err != nil {
		t.Fatalf("insert missing api_key_id request_log: %v", err)
	}

	got := uniqueRequestLogAPIKeyIDByKeyFromDB(db)
	if got["sk-missing"] != "missing-id" {
		t.Fatalf("sk-missing id = %q, want missing-id", got["sk-missing"])
	}
	if _, ok := got["sk-unrelated"]; ok {
		t.Fatalf("sk-unrelated should not be queried when it has no missing api_key_id rows")
	}
}

func TestQueryAPIKeySelectorsHandleLegacyRowsWithoutAPIKeyID(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	db := getDB()
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_name, model, source, channel_name, auth_index,
			 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp, "sk-legacy", "袁蔚", "gpt-test", "source", "channel", "auth-1",
		0, 123, 45, 10, 20, 0, 0, 30, 0,
	); err != nil {
		t.Fatalf("insert legacy request_log: %v", err)
	}

	filters, err := QueryFilters(7)
	if err != nil {
		t.Fatalf("QueryFilters() error = %v", err)
	}
	if len(filters.APIKeys) != 1 || filters.APIKeys[0] != "sk-legacy" {
		t.Fatalf("filters.APIKeys = %#v, want [sk-legacy]", filters.APIKeys)
	}
	if filters.APIKeyNames["sk-legacy"] != "袁蔚" {
		t.Fatalf("filters.APIKeyNames[sk-legacy] = %q, want 袁蔚", filters.APIKeyNames["sk-legacy"])
	}

	dist, err := QueryAPIKeyDistribution(7)
	if err != nil {
		t.Fatalf("QueryAPIKeyDistribution() error = %v", err)
	}
	if len(dist) != 1 {
		t.Fatalf("distribution len = %d, want 1: %#v", len(dist), dist)
	}
	if dist[0].APIKey != "sk-legacy" {
		t.Fatalf("distribution api_key = %q, want sk-legacy", dist[0].APIKey)
	}
	if dist[0].Name != "袁蔚" {
		t.Fatalf("distribution name = %q, want 袁蔚", dist[0].Name)
	}
	if dist[0].Requests != 1 || dist[0].Tokens != 30 {
		t.Fatalf("distribution point = %#v, want one request and 30 tokens", dist[0])
	}
}

func TestQueryFiltersForLogsLinksFilterFacets(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	now := time.Now().UTC()
	InsertLog("sk-a", "A", "glm-5.2", "source-a", "OpenCode", "auth-a", false, now, 1, 1, TokenStats{TotalTokens: 1}, "", "")
	InsertLog("sk-b", "B", "gpt-4.1", "source-b", "Codex", "auth-b", false, now, 1, 1, TokenStats{TotalTokens: 1}, "", "")
	InsertLog("sk-c", "C", "glm-5.2", "source-c", "Anthropic", "auth-c", true, now, 1, 1, TokenStats{TotalTokens: 1}, "", "")

	filters, err := QueryFiltersForLogs(LogQueryParams{Days: 7, Models: []string{"glm-5.2"}})
	if err != nil {
		t.Fatalf("QueryFiltersForLogs() error = %v", err)
	}
	if !slices.Equal(filters.APIKeys, []string{"sk-a", "sk-c"}) {
		t.Fatalf("filters.APIKeys = %#v, want [sk-a sk-c]", filters.APIKeys)
	}
	if !slices.Equal(filters.Channels, []string{"Anthropic", "OpenCode"}) {
		t.Fatalf("filters.Channels = %#v, want [Anthropic OpenCode]", filters.Channels)
	}
	if !slices.Equal(filters.Models, []string{"glm-5.2", "gpt-4.1"}) {
		t.Fatalf("filters.Models = %#v, want own facet to stay expandable", filters.Models)
	}
	if !slices.Equal(filters.Statuses, []string{"success", "failed"}) {
		t.Fatalf("filters.Statuses = %#v, want [success failed]", filters.Statuses)
	}

	filters, err = QueryFiltersForLogs(LogQueryParams{
		Days:    7,
		APIKeys: []string{"sk-a"},
		Models:  []string{"glm-5.2"},
	})
	if err != nil {
		t.Fatalf("QueryFiltersForLogs(api key + model) error = %v", err)
	}
	if !slices.Equal(filters.APIKeys, []string{"sk-a", "sk-c"}) {
		t.Fatalf("filters.APIKeys = %#v, want model-compatible keys", filters.APIKeys)
	}
	if !slices.Equal(filters.Models, []string{"glm-5.2"}) {
		t.Fatalf("filters.Models = %#v, want key-compatible models", filters.Models)
	}
	if !slices.Equal(filters.Statuses, []string{"success"}) {
		t.Fatalf("filters.Statuses = %#v, want key-compatible statuses", filters.Statuses)
	}
}

func TestRequestStatisticsPersistsAPIKeyIdentitySnapshotAcrossRename(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	const stableID = "stable-key-1"
	if err := UpsertAPIKey(APIKeyRow{ID: stableID, Key: "sk-old", Name: "袁蔚"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-old): %v", err)
	}
	if err := UpdateAPIKeyByID(APIKeyRow{ID: stableID, Key: "sk-new", Name: "袁蔚"}); err != nil {
		t.Fatalf("UpdateAPIKeyByID(sk-new): %v", err)
	}

	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "sk-old",
		APIKeyID:    stableID,
		APIKeyName:  "袁蔚",
		Model:       "gpt-5.5",
		Source:      "source",
		ChannelName: "channel",
		AuthIndex:   "auth-1",
		RequestedAt: time.Now().UTC(),
		Detail: coreusage.Detail{
			InputTokens:  1,
			OutputTokens: 2,
			TotalTokens:  3,
		},
	})

	filters, err := QueryFilters(7)
	if err != nil {
		t.Fatalf("QueryFilters() error = %v", err)
	}
	if len(filters.APIKeys) != 1 || filters.APIKeys[0] != "sk-new" {
		t.Fatalf("filters.APIKeys = %#v, want [sk-new]", filters.APIKeys)
	}
	if filters.APIKeyNames["sk-new"] != "袁蔚" {
		t.Fatalf("filters.APIKeyNames[sk-new] = %q, want 袁蔚", filters.APIKeyNames["sk-new"])
	}

	dist, err := QueryAPIKeyDistribution(7)
	if err != nil {
		t.Fatalf("QueryAPIKeyDistribution() error = %v", err)
	}
	if len(dist) != 1 || dist[0].APIKey != "sk-new" {
		t.Fatalf("distribution = %#v, want one sk-new point", dist)
	}
	if dist[0].Name != "袁蔚" {
		t.Fatalf("distribution name = %q, want 袁蔚", dist[0].Name)
	}
}

func TestRequestStatisticsPersistsRequestLogWhenStatisticsDisabled(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	wasEnabled := StatisticsEnabled()
	SetStatisticsEnabled(false)
	t.Cleanup(func() { SetStatisticsEnabled(wasEnabled) })

	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "sk-disabled-stats",
		APIKeyName:  "Disabled Stats",
		Model:       "glm-5.2",
		Source:      "source",
		ChannelName: "channel",
		AuthIndex:   "auth-1",
		RequestedAt: time.Now().UTC(),
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
		},
	})

	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 0 {
		t.Fatalf("snapshot.TotalRequests = %d, want 0 when statistics disabled", snapshot.TotalRequests)
	}

	var count int
	if err := getDB().QueryRow("SELECT COUNT(*) FROM request_logs WHERE api_key = ?", "sk-disabled-stats").Scan(&count); err != nil {
		t.Fatalf("query persisted request log: %v", err)
	}
	if count != 1 {
		t.Fatalf("persisted request log count = %d, want 1", count)
	}
}

func TestInitDBDoesNotRunProviderEchoModelBackfill(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("initial InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()

	db := getDB()
	id := insertProviderEchoRequestLog(t, db, "accounts/fireworks/models/glm-5p2", 100)
	insertRequestLogContentRow(t, db, id, `{"model":"glm-5.2","messages":[]}`)
	CloseDB()

	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("second InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	var model string
	if err := getDB().QueryRow("SELECT model FROM request_logs WHERE id = ?", id).Scan(&model); err != nil {
		t.Fatalf("query provider echo row: %v", err)
	}
	if model != "accounts/fireworks/models/glm-5p2" {
		t.Fatalf("model = %q, want InitDB to leave provider-echo history unchanged", model)
	}
}

func TestClearAllRequestLogsRemovesRequestLogTablesOnly(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	if err := UpsertAPIKey(APIKeyRow{
		Key:       "sk-config-keep",
		Name:      "Config Keep",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertAPIKey() error = %v", err)
	}

	timestamp := time.Now().UTC()
	input := `{"messages":[{"role":"user","content":"hello"}]}`
	output := `{"id":"resp_1","output":"done"}`

	InsertLog("sk-target", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 100, 10, TokenStats{
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
	}, input, output)
	InsertLog("sk-other", "", "gpt-test", "source", "channel", "auth-2", false, timestamp, 200, 20, TokenStats{
		InputTokens: 5, OutputTokens: 10, TotalTokens: 15,
	}, input, output)

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("expected 2 log rows before cleanup, got %d", len(result.Items))
	}

	cleared, err := ClearAllRequestLogs()
	if err != nil {
		t.Fatalf("ClearAllRequestLogs() error = %v", err)
	}
	if cleared.DeletedLogs != 2 {
		t.Fatalf("DeletedLogs = %d, want 2", cleared.DeletedLogs)
	}
	if cleared.DeletedContents != 2 {
		t.Fatalf("DeletedContents = %d, want 2", cleared.DeletedContents)
	}

	result, err = QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() after cleanup error = %v", err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("expected 0 log rows after cleanup, got %d", len(result.Items))
	}

	db := getDB()
	var contentCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content").Scan(&contentCount); err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	if contentCount != 0 {
		t.Fatalf("expected 0 content rows after cleanup, got %d", contentCount)
	}

	keys := ListAPIKeys()
	if len(keys) != 1 || keys[0].Key != "sk-config-keep" {
		t.Fatalf("ListAPIKeys() = %#v, want preserved api key row", keys)
	}
}

func TestClearRequestLogsClearsBodiesButKeepsRequestRows(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	now := time.Now().UTC()
	details := `{"request_id":"req-1","provider":"codex"}`
	InsertLogWithDetails("sk-target", "Primary", "gpt-5.4", "codex", "Codex", "auth-1", false, now, 123, 45, TokenStats{
		InputTokens: 11, OutputTokens: 22, TotalTokens: 33,
	}, `{"messages":[{"role":"user","content":"hello"}]}`, `{"id":"resp_1"}`, details)

	before, err := GetRequestLogStorageBytes()
	if err != nil {
		t.Fatalf("GetRequestLogStorageBytes() before error = %v", err)
	}
	if before <= 0 {
		t.Fatalf("expected request log storage bytes before cleanup, got %d", before)
	}

	result, err := ClearRequestLogs(ClearRequestLogsOptions{
		ClearBodyContent:   true,
		ClearDetailContent: true,
	})
	if err != nil {
		t.Fatalf("ClearRequestLogs() error = %v", err)
	}
	if result.DeletedLogs != 0 {
		t.Fatalf("DeletedLogs = %d, want 0", result.DeletedLogs)
	}

	rows, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() after cleanup error = %v", err)
	}
	if len(rows.Items) != 1 {
		t.Fatalf("expected 1 request log row after cleanup, got %d", len(rows.Items))
	}
	if rows.Items[0].HasContent {
		t.Fatalf("HasContent = true, want false after content cleanup")
	}

	content, err := QueryLogContent(rows.Items[0].ID)
	if err != nil {
		t.Fatalf("QueryLogContent() after cleanup error = %v", err)
	}
	if content.InputContent != "" || content.OutputContent != "" {
		t.Fatalf("expected empty input/output content after cleanup, got input=%q output=%q", content.InputContent, content.OutputContent)
	}

	part, err := QueryLogContentPart(rows.Items[0].ID, "details")
	if err != nil {
		t.Fatalf("QueryLogContentPart(details) after cleanup error = %v", err)
	}
	if part.Content != "" {
		t.Fatalf("expected empty details content after cleanup, got %q", part.Content)
	}

	after, err := GetRequestLogStorageBytes()
	if err != nil {
		t.Fatalf("GetRequestLogStorageBytes() after error = %v", err)
	}
	if after != 0 {
		t.Fatalf("expected request log storage bytes to be 0 after cleanup, got %d", after)
	}
}

func TestQueryStatsAndHeatmapCountSessionsFromDetails(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	// Keep both samples inside the rolling query window instead of letting a
	// hard-coded calendar date age out as the test suite advances.
	today := CutoffStartUTC(1).Add(12 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	InsertLogWithDetails("sk-heatmap", "Heatmap", "gpt-5.4", "codex", "Codex", "auth-1", false, today, 10, 5, TokenStats{
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
	}, "{}", "{}", `{"session_id":"session-a","request_id":"req-a1"}`)
	InsertLogWithDetails("sk-heatmap", "Heatmap", "gpt-5.4", "codex", "Codex", "auth-1", false, today.Add(time.Minute), 10, 5, TokenStats{
		InputTokens: 20, OutputTokens: 30, TotalTokens: 50,
	}, "{}", "{}", `{"session_id":"session-a","request_id":"req-a2"}`)
	InsertLogWithDetails("sk-heatmap", "Heatmap", "gpt-5.4", "codex", "Codex", "auth-1", false, yesterday, 10, 5, TokenStats{
		InputTokens: 1, OutputTokens: 2, TotalTokens: 3,
	}, "{}", "{}", `{"conversation_id":"session-b"}`)

	var storedSessions int
	if err := getDB().QueryRow("SELECT COUNT(*) FROM request_log_content WHERE session_id <> ''").Scan(&storedSessions); err != nil {
		t.Fatalf("count stored session_id rows: %v", err)
	}
	if storedSessions != 3 {
		t.Fatalf("stored session_id rows = %d, want 3", storedSessions)
	}

	stats, err := QueryStats(LogQueryParams{APIKey: "sk-heatmap", Days: 7})
	if err != nil {
		t.Fatalf("QueryStats() error = %v", err)
	}
	if stats.Total != 3 || stats.TotalTokens != 83 {
		t.Fatalf("stats = %#v, want total=3 total_tokens=83", stats)
	}
	sessionCount, err := QuerySessionCount(LogQueryParams{APIKey: "sk-heatmap", Days: 7})
	if err != nil {
		t.Fatalf("QuerySessionCount() error = %v", err)
	}
	if sessionCount != 2 {
		t.Fatalf("QuerySessionCount() = %d, want 2", sessionCount)
	}

	points, err := QueryDailyHeatmapSeries("sk-heatmap", 7)
	if err != nil {
		t.Fatalf("QueryDailyHeatmapSeries() error = %v", err)
	}
	byDate := make(map[string]DailyHeatmapPoint, len(points))
	for _, point := range points {
		byDate[point.Date] = point
	}
	todayPoint := byDate[LocalDayKeyAt(today)]
	if todayPoint.Requests != 2 || todayPoint.Tokens != 80 || todayPoint.Sessions != 1 {
		t.Fatalf("today heatmap point = %#v, want requests=2 tokens=80 sessions=1", todayPoint)
	}
	yesterdayPoint := byDate[LocalDayKeyAt(yesterday)]
	if yesterdayPoint.Requests != 1 || yesterdayPoint.Tokens != 3 || yesterdayPoint.Sessions != 1 {
		t.Fatalf("yesterday heatmap point = %#v, want requests=1 tokens=3 sessions=1", yesterdayPoint)
	}

	chartData, err := QueryPublicChartData("sk-heatmap", 7)
	if err != nil {
		t.Fatalf("QueryPublicChartData() error = %v", err)
	}
	if chartData.Stats.Total != 3 || chartData.Stats.TotalTokens != 83 || chartData.Stats.TotalSessions != 2 {
		t.Fatalf("public chart stats = %#v, want total=3 total_tokens=83 sessions=2", chartData.Stats)
	}
	if len(chartData.DailySeries) != 2 {
		t.Fatalf("public chart daily len = %d, want 2", len(chartData.DailySeries))
	}
	if len(chartData.ModelDistribution) != 1 || chartData.ModelDistribution[0].Requests != 3 || chartData.ModelDistribution[0].Tokens != 83 {
		t.Fatalf("public chart models = %#v, want one model with 3 requests and 83 tokens", chartData.ModelDistribution)
	}
	chartHeatmap := make(map[string]DailyHeatmapPoint, len(chartData.HeatmapSeries))
	for _, point := range chartData.HeatmapSeries {
		chartHeatmap[point.Date] = point
	}
	if chartHeatmap[LocalDayKeyAt(today)].Sessions != 1 || chartHeatmap[LocalDayKeyAt(yesterday)].Sessions != 1 {
		t.Fatalf("public chart heatmap = %#v, want sessions by day populated", chartHeatmap)
	}

	if _, err := ClearRequestLogs(ClearRequestLogsOptions{ClearDetailContent: true}); err != nil {
		t.Fatalf("ClearRequestLogs(details) error = %v", err)
	}
	sessionCount, err = QuerySessionCount(LogQueryParams{APIKey: "sk-heatmap", Days: 7})
	if err != nil {
		t.Fatalf("QuerySessionCount() after detail cleanup error = %v", err)
	}
	if sessionCount != 0 {
		t.Fatalf("QuerySessionCount() after detail cleanup = %d, want 0", sessionCount)
	}
}

func TestStartRequestLogContentSessionIDBackfillDoesNotScanHistoricalContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	compressed, err := compressLogContent(`{"session_id":"historical-session"}`)
	if err != nil {
		t.Fatalf("compressLogContent() error = %v", err)
	}
	if _, err := getDB().Exec(
		`INSERT INTO request_log_content (log_id, timestamp, compression, input_content, output_content, detail_content, session_id)
		 VALUES (?, ?, ?, ?, ?, ?, '')`,
		int64(9001),
		time.Now().UTC().Format(time.RFC3339Nano),
		requestLogContentCompression,
		[]byte{},
		[]byte{},
		compressed,
	); err != nil {
		t.Fatalf("insert historical request_log_content: %v", err)
	}

	startRequestLogContentSessionIDBackfill(getDB())

	var sessionID string
	if err := getDB().QueryRow("SELECT session_id FROM request_log_content WHERE log_id = ?", int64(9001)).Scan(&sessionID); err != nil {
		t.Fatalf("query session_id: %v", err)
	}
	if sessionID != "" {
		t.Fatalf("session_id = %q, want unchanged empty historical value", sessionID)
	}
}

func TestClearRequestLogsAllowsNewContentAfterSizeCapCleanup(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         1,
	})

	maxBytes := maxLogContentBytes()
	if maxBytes <= 0 {
		t.Fatalf("maxLogContentBytes() = %d, want positive cap", maxBytes)
	}

	now := time.Now().UTC()
	firstBody := makePseudoRandomText(650 * 1024)
	InsertLogWithDetails("sk-target", "Primary", "gpt-5.4", "codex", "Codex", "auth-1", false, now, 123, 45, TokenStats{
		InputTokens: 11, OutputTokens: 22, TotalTokens: 33,
	}, firstBody, `{"id":"first"}`, `{"request_id":"req-first"}`)

	before, err := GetRequestLogStorageBytes()
	if err != nil {
		t.Fatalf("GetRequestLogStorageBytes() before error = %v", err)
	}
	if before <= 0 || before >= maxBytes {
		t.Fatalf("expected first stored body to be below cap, got before=%d max=%d", before, maxBytes)
	}

	if _, err := ClearRequestLogs(ClearRequestLogsOptions{
		ClearBodyContent:   true,
		ClearDetailContent: true,
	}); err != nil {
		t.Fatalf("ClearRequestLogs() error = %v", err)
	}

	after, err := GetRequestLogStorageBytes()
	if err != nil {
		t.Fatalf("GetRequestLogStorageBytes() after cleanup error = %v", err)
	}
	if after != 0 {
		t.Fatalf("expected stored bytes to be 0 after cleanup, got %d", after)
	}

	secondBody := makePseudoRandomText(650 * 1024)
	InsertLogWithDetails("sk-target", "Primary", "gpt-5.4", "codex", "Codex", "auth-1", false, now.Add(time.Second), 123, 45, TokenStats{
		InputTokens: 11, OutputTokens: 22, TotalTokens: 33,
	}, secondBody, `{"id":"second"}`, `{"request_id":"req-second"}`)

	rows, err := QueryLogs(LogQueryParams{Page: 1, Size: 1, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() after reinserting content error = %v", err)
	}
	if len(rows.Items) != 1 {
		t.Fatalf("expected latest request log row, got %d", len(rows.Items))
	}
	if !rows.Items[0].HasContent {
		t.Fatalf("HasContent = false for new log after cleanup, want true")
	}
}
