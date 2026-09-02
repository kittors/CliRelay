package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func seedDailyQuotaSnapshot(t *testing.T, tenantID, dateKey, authIndex, quotaKey string, percent float64) {
	t.Helper()
	if _, err := getDB().Exec(`
		INSERT INTO auth_file_quota_snapshots (
			tenant_id, date_key, auth_index, auth_subject_id, provider, quota_key, percent, recorded_at
		) VALUES (?, ?, ?, '', 'antigravity', ?, ?, ?)
	`, tenantID, dateKey, authIndex, quotaKey, percent, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func TestQueryWeeklyQuotaSeriesReturnsEveryWeeklyWindow(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	const tenantID = "00000000-0000-0000-0000-0000000000aa"
	now := time.Now().UTC()
	gemini, third := 51.0, 90.0
	fiveHour := 72.0
	week := WeeklyQuotaWindowSeconds
	indexes := []string{"ag-1", "ag-2"}
	for _, idx := range indexes {
		if err := RecordQuotaSnapshotPointsIdentityForTenant(tenantID, idx, "sub-"+idx, "antigravity", []QuotaSnapshotPoint{
			{RecordedAt: now, QuotaKey: "antigravity:gemini_weekly", QuotaLabel: "Gemini Models", Percent: &gemini, WindowSeconds: week},
			{RecordedAt: now, QuotaKey: "antigravity:3p_weekly", QuotaLabel: "Claude and GPT models", Percent: &third, WindowSeconds: week},
			{RecordedAt: now, QuotaKey: "antigravity:gemini_5h", QuotaLabel: "Gemini Models", Percent: &fiveHour, WindowSeconds: 5 * 60 * 60},
		}); err != nil {
			t.Fatal(err)
		}
	}
	today := localDayKeyAt(now)
	seedDailyQuotaSnapshot(t, tenantID, today, "ag-1", "antigravity:gemini_weekly", 40)
	seedDailyQuotaSnapshot(t, tenantID, today, "ag-2", "antigravity:gemini_weekly", 60)
	seedDailyQuotaSnapshot(t, tenantID, today, "ag-1", "antigravity:3p_weekly", 80)
	seedDailyQuotaSnapshot(t, tenantID, today, "ag-2", "antigravity:3p_weekly", 100)
	seedDailyQuotaSnapshot(t, tenantID, today, "ag-1", "antigravity:gemini_5h", 10)

	series, err := QueryWeeklyQuotaSeriesByAuthIndexesForTenant(tenantID, indexes, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 {
		t.Fatalf("series = %+v, want gemini_weekly and 3p_weekly only", series)
	}
	byKey := map[string]DailyQuotaSeries{}
	for _, item := range series {
		byKey[item.QuotaKey] = item
	}
	geminiSeries, ok := byKey["antigravity:gemini_weekly"]
	if !ok || geminiSeries.QuotaLabel != "Gemini Models" {
		t.Fatalf("gemini series = %+v", geminiSeries)
	}
	thirdSeries, ok := byKey["antigravity:3p_weekly"]
	if !ok {
		t.Fatalf("missing 3p series: %+v", series)
	}
	avg := func(item DailyQuotaSeries) float64 {
		for _, point := range item.Points {
			if point.Date == today && point.Percent != nil {
				return *point.Percent
			}
		}
		t.Fatalf("no today point in %+v", item)
		return 0
	}
	if got := avg(geminiSeries); got != 50 {
		t.Fatalf("gemini avg = %v, want 50", got)
	}
	if got := avg(thirdSeries); got != 90 {
		t.Fatalf("3p avg = %v, want 90", got)
	}
}

func TestPreferPrimaryWeeklyQuotaKeyPutsCodeWeekFirst(t *testing.T) {
	got := preferPrimaryWeeklyQuotaKey([]weeklyQuotaKey{
		{QuotaKey: "antigravity:gemini_weekly"},
		{QuotaKey: "code_week"},
		{QuotaKey: "review_week"},
	})
	if len(got) != 3 || got[0].QuotaKey != "code_week" {
		t.Fatalf("order = %+v, want code_week first", got)
	}
}

func TestQueryWeeklyQuotaSeriesFallsBackToCodeWeekDailySnapshots(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	const tenantID = "00000000-0000-0000-0000-0000000000bb"
	today := localDayKeyAt(time.Now().UTC())
	seedDailyQuotaSnapshot(t, tenantID, today, "codex-1", "code_week", 70)
	series, err := QueryWeeklyQuotaSeriesByAuthIndexesForTenant(tenantID, []string{"codex-1"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].QuotaKey != "code_week" {
		t.Fatalf("series = %+v, want code_week fallback", series)
	}
}
