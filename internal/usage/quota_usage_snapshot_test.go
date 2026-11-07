package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

type snapshotRow struct {
	kind, start, model string
	cost               float64
	requests, tokens   int64
}

func seedSnapshotRows(t *testing.T, column, subjectID string, now time.Time, rows []snapshotRow) {
	t.Helper()
	db := getDB()
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
			(tenant_id,bucket_kind,bucket_start,`+column+`,model,cost_total,request_count,total_tokens,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, systemTenantID, row.kind, row.start, subjectID, row.model, row.cost, row.requests, row.tokens, now); err != nil {
			t.Fatalf("insert %+v: %v", row, err)
		}
	}
}

// 快照与既有美元周期查询同源：同一份数据下 Total.Cost 必须与 QueryPeriodSpending 一致，
// 面板上同一格才不会出现两个数。
func TestQuotaUsageSnapshotMatchesPeriodSpendingAndSplitsByModel(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	now := time.Date(2026, 7, 22, 12, 30, 45, 0, time.UTC)
	userID := "snapshot-user"
	seedSnapshotRows(t, "end_user_id", userID, now, []snapshotRow{
		{rollupBucketDay, "2026-07-19", "claude-opus-4-5", 100, 9, 900}, // last week: month only
		{rollupBucketDay, "2026-07-20", "claude-opus-4-5", 10, 2, 200},  // this week
		{rollupBucketDay, "2026-07-22", "claude-opus-4-5", 5, 1, 100},   // today
		{rollupBucketDay, "2026-07-22", "gpt-5", 1, 4, 40},              // today, other model
		{rollupBucketDay, "2026-07-23", "claude-opus-4-5", 70, 7, 700},  // tomorrow: excluded
		{rollupBucketQuotaMinuteUTC, "2026-07-22T11:00", "claude-opus-4-5", 3, 1, 30},
		{rollupBucketQuotaMinuteUTC, "2026-07-22T12:30", "gpt-5", 2, 2, 20},
		{rollupBucketLifetime, rollupLifetimeStart, "claude-opus-4-5", 500, 50, 5000},
		{rollupBucketLifetime, rollupLifetimeStart, "gpt-5", 20, 30, 300},
	})
	seedFiveHourAnchor(t, systemTenantID, periodResetSubjectEndUser, userID, "2026-07-22T10:00")

	snapshots, err := QueryEndUserQuotaUsageSnapshotsAt(systemTenantID, []string{userID}, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snap := snapshots[userID]
	period, err := QueryPeriodSpendingByEndUsersForTenantAt(systemTenantID, []string{userID}, now)
	if err != nil {
		t.Fatalf("period: %v", err)
	}
	legacy := period[userID]
	projected := snap.PeriodSpending()
	if projected.FiveHour != legacy.FiveHour || projected.Day != legacy.Day || projected.Week != legacy.Week ||
		projected.Month != legacy.Month || projected.Lifetime != legacy.Lifetime || !projected.FiveHourWindowStart.Equal(legacy.FiveHourWindowStart) {
		t.Fatalf("snapshot cost %+v != period spending %+v", projected, legacy)
	}
	want := quota.Matrix{
		Cost:     quota.WindowValues{FiveHour: 5, Day: 6, Week: 16, Month: 116, Lifetime: 520},
		Requests: quota.WindowValues{FiveHour: 3, Day: 5, Week: 7, Month: 16, Lifetime: 80},
		Tokens:   quota.WindowValues{FiveHour: 50, Day: 140, Week: 340, Month: 1240, Lifetime: 5300},
	}
	if snap.Total != want {
		t.Fatalf("total = %+v\nwant  %+v", snap.Total, want)
	}
	opus := snap.MatchedUsage([]string{"claude-opus-*"})
	if opus.Cost.Week != 15 || opus.Requests.Day != 1 || opus.Tokens.FiveHour != 30 || opus.Cost.Lifetime != 500 {
		t.Fatalf("opus usage = %+v", opus)
	}
	if none := snap.MatchedUsage([]string{"gemini-*"}); !none.IsZero() {
		t.Fatalf("unmatched usage = %+v", none)
	}
}

// 重置一个周期会同时清零请求次数、Token 与按模型用量；之后的新用量照常累计。
func TestQuotaUsageSnapshotAppliesResetBaselinesToEveryDimension(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	now := time.Date(2026, 7, 22, 12, 30, 45, 0, time.UTC)
	keyID := "snapshot-key"
	seedSnapshotRows(t, "api_key_id", keyID, now, []snapshotRow{
		{rollupBucketDay, "2026-07-22", "claude-opus-4-5", 5, 3, 300},
		{rollupBucketDay, "2026-07-21", "claude-opus-4-5", 7, 1, 100},
	})
	if _, err := resetPeriodSpendingForSubject(systemTenantID, periodSubjectAPIKey, keyID, []quota.Period{quota.PeriodDay}, PeriodSpendingResetActor{Kind: "test"}, now); err != nil {
		t.Fatalf("reset day: %v", err)
	}
	snapshots, err := QueryAPIKeyQuotaUsageSnapshotsAt(systemTenantID, []string{keyID}, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snap := snapshots[keyID]
	if snap.Total.Cost.Day != 0 || snap.Total.Requests.Day != 0 || snap.Total.Tokens.Day != 0 {
		t.Fatalf("day after reset = %+v", snap.Total)
	}
	if snap.Total.Requests.Week != 4 || snap.Total.Cost.Week != 12 {
		t.Fatalf("week must not be reset by a day reset: %+v", snap.Total)
	}
	if opus := snap.MatchedUsage([]string{"claude-opus-4-5"}); opus.Requests.Day != 0 || opus.Cost.Day != 0 || opus.Requests.Week != 4 {
		t.Fatalf("opus after reset = %+v", opus)
	}

	seedSnapshotRows(t, "api_key_id", keyID, now, []snapshotRow{{rollupBucketDay, "2026-07-22", "gpt-5", 1, 2, 20}})
	snapshots, err = QueryAPIKeyQuotaUsageSnapshotsAt(systemTenantID, []string{keyID}, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap := snapshots[keyID]; snap.Total.Requests.Day != 2 || snap.MatchedUsage([]string{"gpt-5"}).Tokens.Day != 20 {
		t.Fatalf("usage after reset = %+v", snap.Total)
	}

	// 旧版本节点只改写了 cost_baseline/window_key、没碰 usage_baseline：内嵌窗口对不上，
	// 这份非美元基线作废，不能拿上一个窗口的基线去减本窗口的用量。
	if _, err := getDB().Exec(`UPDATE period_spending_resets SET window_key = ? WHERE subject_id = ?`, "2026-07-22", keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := getDB().Exec(`UPDATE period_spending_resets SET usage_baseline = ? WHERE subject_id = ?`,
		`{"window":"2026-07-21","requests":100,"tokens":100}`, keyID); err != nil {
		t.Fatal(err)
	}
	snapshots, err = QueryAPIKeyQuotaUsageSnapshotsAt(systemTenantID, []string{keyID}, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap := snapshots[keyID]; snap.Total.Requests.Day != 5 {
		t.Fatalf("stale usage baseline must be ignored: %+v", snap.Total)
	}
}

func TestQuotaUsageSnapshotEmptyAndUnknownSubjects(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	now := time.Date(2026, 7, 22, 12, 30, 45, 0, time.UTC)
	snapshots, err := QueryEndUserQuotaUsageSnapshotsAt(systemTenantID, []string{"", "  ", "nobody"}, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %+v, want only the non-empty id", snapshots)
	}
	if snap := snapshots["nobody"]; !snap.Total.IsZero() || snap.Models == nil {
		t.Fatalf("unknown subject snapshot = %+v", snap)
	}
}
