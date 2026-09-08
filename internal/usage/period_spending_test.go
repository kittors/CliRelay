package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

func TestPeriodWindowKeysAtUsesProjectTimezoneMondayWeek(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 7, 22, 12, 30, 0, 0, time.UTC)
	got := PeriodWindowKeysAt(now, loc)
	if got.WeekFrom != "2026-07-20" || got.MonthFrom != "2026-07-01" || got.DayTo != "2026-07-23" {
		t.Fatalf("windows = %+v, want Monday 2026-07-20, month 2026-07-01, tomorrow 2026-07-23", got)
	}
	// 5h 的下界是 per-subject 锚点，这里只校验「含当前分钟」的上界。
	if got.FiveHourTo != "2026-07-22T12:31" {
		t.Fatalf("5h upper bound = %q, want 2026-07-22T12:31", got.FiveHourTo)
	}
}

func TestQueryPeriodSpendingWeekAndFiveHourBoundaries(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	now := time.Date(2026, 7, 22, 12, 30, 45, 0, time.UTC)
	keyID := "period-key"
	insert := func(kind, start string, cost float64) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
			(tenant_id,bucket_kind,bucket_start,api_key_id,cost_total,updated_at)
			VALUES (?,?,?,?,?,?)`, systemTenantID, kind, start, keyID, cost, now); err != nil {
			t.Fatalf("insert %s %s: %v", kind, start, err)
		}
	}
	insert(rollupBucketDay, "2026-07-19", 100) // previous Sunday: excluded from week
	insert(rollupBucketDay, "2026-07-20", 10)  // Monday inclusive
	insert(rollupBucketDay, "2026-07-22", 5)
	insert(rollupBucketDay, "2026-07-23", 20)                   // tomorrow exclusive
	insert(rollupBucketQuotaMinuteUTC, "2026-07-22T07:59", 100) // before window start
	insert(rollupBucketQuotaMinuteUTC, "2026-07-22T08:00", 3)   // window start included
	insert(rollupBucketQuotaMinuteUTC, "2026-07-22T12:30", 4)   // current minute included
	insert(rollupBucketQuotaMinuteUTC, "2026-07-22T12:31", 100) // exclusive upper bound
	insert(rollupBucketLifetime, rollupLifetimeStart, 999)

	// 没有锚点就没有活跃窗口，5h 用量按 0 计，其余周期照常。
	got, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, now)
	if err != nil {
		t.Fatalf("QueryPeriodSpending: %v", err)
	}
	if used := got[keyID]; used.FiveHour != 0 || !used.FiveHourWindowStart.IsZero() {
		t.Fatalf("without anchor used = %+v, want 5h=0 and zero window start", used)
	}

	// 窗口 [08:00,13:00) 仍在有效期内，只统计窗口内到当前分钟为止的消费。
	seedFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, keyID, "2026-07-22T08:00")
	got, err = QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, now)
	if err != nil {
		t.Fatalf("QueryPeriodSpending: %v", err)
	}
	used := got[keyID]
	if used.FiveHour != 7 || used.Day != 5 || used.Week != 15 || used.Month != 115 || used.Lifetime != 999 {
		t.Fatalf("used = %+v, want 5h=7 day=5 week=15 month=115 lifetime=999", used)
	}
	if want := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC); !used.FiveHourWindowStart.Equal(want) {
		t.Fatalf("window start = %v, want %v", used.FiveHourWindowStart, want)
	}
}

func TestFiveHourQuotaProjectionReadinessRequiresFullCoverage(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	ensureUsageProjectionMarkerTable(db)
	set := func(start time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO usage_projection_markers(marker_key,marker_value,updated_at)
			VALUES(?,?,?) ON CONFLICT(marker_key) DO UPDATE SET marker_value=excluded.marker_value,updated_at=excluded.updated_at`,
			quotaMinuteCoverageStartMarker, start.Format(time.RFC3339), now); err != nil {
			t.Fatalf("set marker: %v", err)
		}
	}
	set(now.Add(-4*time.Hour - 59*time.Minute))
	if FiveHourQuotaProjectionReadyAt(now) {
		t.Fatal("projection should still be warming before five hours")
	}
	set(now.Add(-5 * time.Hour))
	if !FiveHourQuotaProjectionReadyAt(now) {
		t.Fatal("projection should be ready at full five-hour coverage")
	}
}

func TestRollupBucketStartsProjectsQuotaMinuteInUTC(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*60*60)
	at := time.Date(2026, 7, 22, 23, 59, 30, 0, loc)
	starts := rollupBucketStarts(at, loc)
	if starts[rollupBucketMinute] != "2026-07-22T23:59" {
		t.Fatalf("local minute = %q", starts[rollupBucketMinute])
	}
	if starts[rollupBucketQuotaMinuteUTC] != "2026-07-22T15:59" {
		t.Fatalf("quota UTC minute = %q, want 2026-07-22T15:59", starts[rollupBucketQuotaMinuteUTC])
	}
}

func TestPeriodSpendingResetWeekOnlyAndMultiplePeriods(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	now := time.Date(2026, 7, 22, 12, 30, 45, 0, time.UTC)
	keyID := "period-reset-key"
	insert := func(kind, start, model string, cost float64) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
			(tenant_id,bucket_kind,bucket_start,api_key_id,model,cost_total,updated_at)
			VALUES (?,?,?,?,?,?,?)`, systemTenantID, kind, start, keyID, model, cost, now); err != nil {
			t.Fatalf("insert %s %s: %v", kind, start, err)
		}
	}
	insert(rollupBucketDay, "2026-07-20", "monday", 10)
	insert(rollupBucketDay, "2026-07-22", "today", 5)
	insert(rollupBucketQuotaMinuteUTC, "2026-07-22T12:30", "minute", 7)
	// 5h 需要活跃窗口才有用量，窗口 [12:00,17:00) 覆盖上面这一分钟。
	seedFiveHourAnchor(t, systemTenantID, periodResetSubjectAPIKey, keyID, "2026-07-22T12:00")

	reset, err := resetPeriodSpendingForSubject(systemTenantID, periodSubjectAPIKey, keyID, []quota.Period{quota.PeriodWeek}, PeriodSpendingResetActor{Kind: "test"}, now)
	if err != nil {
		t.Fatalf("reset week: %v", err)
	}
	if reset.EffectiveUsedBefore.Week != 15 || reset.CostBaselines[quota.PeriodWeek] != 15 {
		t.Fatalf("week reset = %+v, want used/baseline 15", reset)
	}
	got, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, now)
	if err != nil {
		t.Fatalf("query after week reset: %v", err)
	}
	if used := got[keyID]; used.Week != 0 || used.Day != 5 || used.Month != 15 || used.FiveHour != 7 {
		t.Fatalf("week-only used = %+v, want week=0 day=5 month=15 5h=7", used)
	}

	insert(rollupBucketDay, "2026-07-22", "after-reset", 2)
	got, err = QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, now)
	if err != nil {
		t.Fatalf("query incremental: %v", err)
	}
	if used := got[keyID]; used.Week != 2 || used.Day != 7 || used.Month != 17 {
		t.Fatalf("incremental used = %+v, want week=2 day=7 month=17", used)
	}

	reset, err = resetPeriodSpendingForSubject(systemTenantID, periodSubjectAPIKey, keyID,
		[]quota.Period{quota.PeriodMonth, quota.PeriodFiveHour, quota.PeriodDay, quota.PeriodMonth}, PeriodSpendingResetActor{Kind: "test"}, now)
	if err != nil {
		t.Fatalf("reset month+day: %v", err)
	}
	if len(reset.Periods) != 3 || reset.Periods[0] != quota.PeriodMonth || reset.Periods[1] != quota.PeriodFiveHour || reset.Periods[2] != quota.PeriodDay {
		t.Fatalf("deduped periods = %v", reset.Periods)
	}
	got, err = QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, now)
	if err != nil {
		t.Fatalf("query after multi reset: %v", err)
	}
	if used := got[keyID]; used.Month != 0 || used.Day != 0 || used.Week != 2 || used.FiveHour != 0 {
		t.Fatalf("multi-period used = %+v, want month/day/5h=0 week=2", used)
	}
	count, err := CountAPIKeyPeriodSpendingResetEvents(systemTenantID, keyID)
	if err != nil || count != 2 {
		t.Fatalf("event count = %d, %v; want 2", count, err)
	}
}

func TestPeriodSpendingResetSubjectAndTenantIsolation(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	now := time.Date(2026, 7, 22, 12, 30, 0, 0, time.UTC)
	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"
	keyID := "shared-key-id"
	endUserID := "shared-user-id"
	for _, tenantID := range []string{tenantA, tenantB} {
		if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
			(tenant_id,bucket_kind,bucket_start,api_key_id,end_user_id,model,cost_total,updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, tenantID, rollupBucketDay, "2026-07-22", keyID, endUserID, "model", 9, now); err != nil {
			t.Fatalf("insert tenant %s: %v", tenantID, err)
		}
	}
	if _, err := resetPeriodSpendingForSubject(tenantA, periodSubjectAPIKey, keyID, []quota.Period{quota.PeriodDay}, PeriodSpendingResetActor{Kind: "test"}, now); err != nil {
		t.Fatalf("reset tenant A key: %v", err)
	}
	keyA, _ := QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantA, []string{keyID}, now)
	keyB, _ := QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantB, []string{keyID}, now)
	userA, _ := QueryPeriodSpendingByEndUsersForTenantAt(tenantA, []string{endUserID}, now)
	if keyA[keyID].Day != 0 || keyB[keyID].Day != 9 || userA[endUserID].Day != 9 {
		t.Fatalf("after key reset keyA=%+v keyB=%+v userA=%+v", keyA[keyID], keyB[keyID], userA[endUserID])
	}
	if _, err := resetPeriodSpendingForSubject(tenantA, periodSubjectEndUser, endUserID, []quota.Period{quota.PeriodDay}, PeriodSpendingResetActor{Kind: "test"}, now); err != nil {
		t.Fatalf("reset tenant A account: %v", err)
	}
	keyA, _ = QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantA, []string{keyID}, now)
	userA, _ = QueryPeriodSpendingByEndUsersForTenantAt(tenantA, []string{endUserID}, now)
	if keyA[keyID].Day != 0 || userA[endUserID].Day != 0 {
		t.Fatalf("account/key isolation after both resets keyA=%+v userA=%+v", keyA[keyID], userA[endUserID])
	}
}

func TestPeriodSpendingResetExpiresWhenWindowChanges(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	firstWeek := time.Date(2026, 7, 22, 12, 30, 0, 0, time.UTC)
	keyID := "window-reset-key"
	if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
		(tenant_id,bucket_kind,bucket_start,api_key_id,model,cost_total,updated_at)
		VALUES (?,?,?,?,?,?,?)`, systemTenantID, rollupBucketDay, "2026-07-22", keyID, "first", 9, firstWeek); err != nil {
		t.Fatalf("insert first week: %v", err)
	}
	if _, err := resetPeriodSpendingForSubject(systemTenantID, periodSubjectAPIKey, keyID, []quota.Period{quota.PeriodWeek}, PeriodSpendingResetActor{Kind: "test"}, firstWeek); err != nil {
		t.Fatalf("reset first week: %v", err)
	}
	nextWeek := firstWeek.AddDate(0, 0, 7)
	if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
		(tenant_id,bucket_kind,bucket_start,api_key_id,model,cost_total,updated_at)
		VALUES (?,?,?,?,?,?,?)`, systemTenantID, rollupBucketDay, "2026-07-29", keyID, "next", 4, nextWeek); err != nil {
		t.Fatalf("insert next week: %v", err)
	}
	got, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(systemTenantID, []string{keyID}, nextWeek)
	if err != nil {
		t.Fatalf("query next week: %v", err)
	}
	if got[keyID].Week != 4 {
		t.Fatalf("next-week used = %+v, want week=4 (old baseline expired)", got[keyID])
	}
}

// TestLifetimeAllowanceResetsToFullOnGrant covers the operator-facing promise:
// granting a new allowance makes the account spendable again for the full
// amount, no matter how much it spent historically. Before cumulative spend
// became resettable, a $1000 limit on an account that had already spent $594
// only bought it $406 of runway, forcing operators to hand-compute
// "past spend + intended allowance" on every top-up.
func TestLifetimeAllowanceResetsToFullOnGrant(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	const endUserID = "lifetime-grant-user"

	if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
		(tenant_id,bucket_kind,bucket_start,end_user_id,cost_total,updated_at)
		VALUES (?,?,?,?,?,?)`, systemTenantID, rollupBucketLifetime, rollupLifetimeStart, endUserID, 594.92, now); err != nil {
		t.Fatalf("seed lifetime spend: %v", err)
	}

	spent, err := QueryTotalCostByEndUser(endUserID)
	if err != nil {
		t.Fatalf("QueryTotalCostByEndUser: %v", err)
	}
	if spent != 594.92 {
		t.Fatalf("spend before grant = %v, want 594.92", spent)
	}

	if _, err := resetPeriodSpendingForSubject(systemTenantID, periodSubjectEndUser, endUserID,
		[]quota.Period{quota.PeriodLifetime}, PeriodSpendingResetActor{Username: "admin"}, now); err != nil {
		t.Fatalf("grant allowance: %v", err)
	}

	spent, err = QueryTotalCostByEndUser(endUserID)
	if err != nil {
		t.Fatalf("QueryTotalCostByEndUser after grant: %v", err)
	}
	if spent != 0 {
		t.Fatalf("spend after grant = %v, want 0 so the full allowance is available", spent)
	}

	// Spend that lands after the grant counts against the new allowance.
	if _, err := db.Exec(`UPDATE usage_rollup_buckets SET cost_total = ? WHERE end_user_id = ? AND bucket_kind = ?`,
		630.92, endUserID, rollupBucketLifetime); err != nil {
		t.Fatalf("add post-grant spend: %v", err)
	}
	spent, err = QueryTotalCostByEndUser(endUserID)
	if err != nil {
		t.Fatalf("QueryTotalCostByEndUser after new spend: %v", err)
	}
	if spent != 36 {
		t.Fatalf("post-grant spend = %v, want 36", spent)
	}
}

func TestLifetimeAllowanceBaselineSurvivesWindowRollover(t *testing.T) {
	// Rolling periods lose their baseline when the window changes. A lifetime
	// allowance has no window, so a grant must still hold days later — otherwise
	// the account would silently revert to counting its whole history.
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	granted := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	const endUserID = "lifetime-rollover-user"

	if _, err := db.Exec(`INSERT INTO usage_rollup_buckets
		(tenant_id,bucket_kind,bucket_start,end_user_id,cost_total,updated_at)
		VALUES (?,?,?,?,?,?)`, systemTenantID, rollupBucketLifetime, rollupLifetimeStart, endUserID, 500, granted); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	if _, err := resetPeriodSpendingForSubject(systemTenantID, periodSubjectEndUser, endUserID,
		[]quota.Period{quota.PeriodLifetime}, PeriodSpendingResetActor{Username: "admin"}, granted); err != nil {
		t.Fatalf("grant allowance: %v", err)
	}

	muchLater := granted.AddDate(0, 2, 3)
	usage, err := QueryPeriodSpendingByEndUsersForTenantAt(systemTenantID, []string{endUserID}, muchLater)
	if err != nil {
		t.Fatalf("QueryPeriodSpending: %v", err)
	}
	if got := usage[endUserID].Lifetime; got != 0 {
		t.Fatalf("lifetime used two months after the grant = %v, want 0", got)
	}
}
