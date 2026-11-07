package usage

import (
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// 快照查询里的条件聚合（SUM(CASE …)）在 PostgreSQL 上返回 numeric：这里确认它能被
// 扫描成整数与浮点，并且与 SQLite 上得出同样的数，重置写下的 usage_baseline 也能读回。
func TestPostgresQuotaUsageSnapshotAndReset(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	// The minute bucket below is months old, and the maintenance pass InitPostgres
	// starts prunes minute buckets older than a day: landing between the insert
	// and the read, it took the five-hour total to 0. Stopping it waits for that
	// pass to finish.
	stopRequestLogMaintenance()
	db := getDB()
	if _, err := db.Exec(`TRUNCATE usage_rollup_buckets, period_spending_resets, period_spending_reset_events, period_window_anchors`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	now := time.Date(2026, 7, 22, 12, 30, 45, 0, time.UTC)
	userID := "11111111-2222-3333-4444-555555555555"
	insert := func(kind, start, model string, cost float64, requests, tokens int64) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
			(tenant_id,bucket_kind,bucket_start,end_user_id,model,cost_total,request_count,total_tokens,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, systemTenantID, kind, start, userID, model, cost, requests, tokens, now); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insert(rollupBucketDay, "2026-07-20", "claude-opus-4-5", 10, 2, 200)
	insert(rollupBucketDay, "2026-07-22", "claude-opus-4-5", 5, 1, 100)
	insert(rollupBucketDay, "2026-07-22", "gpt-5", 1, 4, 40)
	insert(rollupBucketLifetime, rollupLifetimeStart, "gpt-5", 20, 30, 300)
	insert(rollupBucketQuotaMinuteUTC, "2026-07-22T12:00", "gpt-5", 2, 2, 20)
	seedFiveHourAnchor(t, systemTenantID, periodResetSubjectEndUser, userID, "2026-07-22T10:00")

	snapshots, err := QueryEndUserQuotaUsageSnapshotsAt(systemTenantID, []string{userID}, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snap := snapshots[userID]
	if snap.Total.Cost.Week != 16 || snap.Total.Requests.Day != 5 || snap.Total.Tokens.Week != 340 ||
		snap.Total.Requests.Lifetime != 30 || snap.Total.Tokens.FiveHour != 20 {
		t.Fatalf("total = %+v", snap.Total)
	}
	if opus := snap.MatchedUsage([]string{"claude-opus-*"}); opus.Requests.Week != 3 || opus.Cost.Day != 5 {
		t.Fatalf("opus = %+v", opus)
	}
	if _, err := resetPeriodSpendingForSubject(systemTenantID, periodSubjectEndUser, userID, []quota.Period{quota.PeriodDay}, PeriodSpendingResetActor{Kind: "test"}, now); err != nil {
		t.Fatalf("reset: %v", err)
	}
	snapshots, err = QueryEndUserQuotaUsageSnapshotsAt(systemTenantID, []string{userID}, now)
	if err != nil {
		t.Fatalf("snapshot after reset: %v", err)
	}
	if snap := snapshots[userID]; snap.Total.Requests.Day != 0 || snap.Total.Cost.Day != 0 || snap.MatchedUsage([]string{"gpt-5"}).Tokens.Day != 0 || snap.Total.Requests.Week != 7 {
		t.Fatalf("after reset = %+v", snap.Total)
	}
}
