package usage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// Production runs on PostgreSQL: tenant ids are UUIDs, times are TIMESTAMPTZ,
// and the job table and the one-hour cache column come from migrations. This
// runs a fill job end to end there, with a daily and a period reset taken after
// the request, and the reconcile, autofill and OpenRouter sync queries.
func TestPostgresPricingResync(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	stopRequestLogMaintenance()
	clean := func() {
		truncatePostgresRuntimeTables(t, getDB())
		for _, table := range []string{"pricing_resync_jobs", "api_key_daily_spending_resets", "period_spending_resets"} {
			if _, err := getDB().Exec(`DELETE FROM ` + table); err != nil {
				t.Fatalf("clean %s: %v", table, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	resetModelCatalogAutofillListForTest()

	if err := UpsertAPIKey(APIKeyRow{ID: "key-pg-resync", Key: "sk-pg-resync", Name: "Resync"}); err != nil {
		t.Fatalf("api key: %v", err)
	}
	now := time.Now().UTC()
	write := func(model string, tokens TokenStats, at time.Time) {
		t.Helper()
		InsertRequestLog(RequestLogEntry{
			APIKey: "sk-pg-resync", APIKeyID: "key-pg-resync", APIKeyName: "Resync", AuthSubjectID: "pg-subject",
			Model: model, Source: "codex", Timestamp: at, Tokens: tokens,
		})
	}
	write("pg-fresh", resyncTokens, now.Add(-3*time.Minute))
	write("pg-fresh", TokenStats{InputTokens: 200, OutputTokens: 50, TotalTokens: 1250, CacheReported: true,
		CacheReadTokens: 800, CacheWriteTokens: 200, CacheWrite1hTokens: 150}, now.Add(-2*time.Minute))

	// Resets taken after the requests.
	if err := UpsertDailySpendingReset(systemTenantID, "key-pg-resync", 0); err != nil {
		t.Fatalf("daily reset: %v", err)
	}
	if _, err := ResetPeriodSpendingByAPIKeyIDForTenant(systemTenantID, "key-pg-resync",
		[]quota.Period{quota.PeriodDay, quota.PeriodLifetime}, PeriodSpendingResetActor{}); err != nil {
		t.Fatalf("period reset: %v", err)
	}
	periodBefore, err := QueryPeriodSpendingByAPIKeyIDForTenant(systemTenantID, "key-pg-resync")
	if err != nil {
		t.Fatalf("period spending: %v", err)
	}
	todayBefore, err := QueryTodayEffectiveCostByKeyForTenant(systemTenantID, "sk-pg-resync")
	if err != nil {
		t.Fatalf("today: %v", err)
	}

	priceModel(t, systemTenantID, "pg-fresh", 3, 15, 0.3)
	id, err := enqueuePricingResyncDB(getDB(), systemTenantID, []string{"pg-fresh"}, PricingResyncFill, PricingResyncOriginManual, time.Now())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := getDB().Exec(`UPDATE pricing_resync_jobs SET not_before = ? WHERE id = ?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("make due: %v", err)
	}
	runDuePricingResyncJobs(context.Background(), getDB())

	jobs, err := ListPricingResyncJobs(systemTenantID, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs %+v, %v", jobs, err)
	}
	job := jobs[0]
	costs := requestCosts(t, "pg-fresh")
	if job.Status != pricingResyncDone || job.Changed != 2 || job.Skipped != 0 || !near(job.CostDelta, sum(costs)) || sum(costs) == 0 {
		t.Fatalf("job %+v costs %v", job, costs)
	}
	for _, kind := range []string{rollupBucketMinute, rollupBucketHour, rollupBucketDay, rollupBucketLifetime} {
		if cost, requests := bucketTotals(t, kind, "pg-fresh"); !near(cost, sum(costs)) || requests != 2 {
			t.Fatalf("%s bucket: cost %v requests %d", kind, cost, requests)
		}
	}
	// Spend counted since the resets did not move: both requests came before them.
	todayAfter, err := QueryTodayEffectiveCostByKeyForTenant(systemTenantID, "sk-pg-resync")
	if err != nil || !near(todayBefore, todayAfter) {
		t.Fatalf("today effective %v -> %v (%v)", todayBefore, todayAfter, err)
	}
	periodAfter, err := QueryPeriodSpendingByAPIKeyIDForTenant(systemTenantID, "key-pg-resync")
	if err != nil {
		t.Fatalf("period spending after: %v", err)
	}
	if !near(periodBefore.Day, periodAfter.Day) || !near(periodBefore.Lifetime, periodAfter.Lifetime) {
		t.Fatalf("period spending %+v -> %+v", periodBefore, periodAfter)
	}

	// The worker's other queries run on PostgreSQL.
	state := &pricingResyncWorkerState{reconciled: map[string]int64{}}
	if err := state.reconcile(context.Background(), getDB(), time.Now()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := catalogTenantIDs(); err != nil {
		t.Fatalf("catalog tenants: %v", err)
	}
	if _, err := modelCatalogAutofillTargets(time.Now()); err != nil {
		t.Fatalf("autofill targets: %v", err)
	}
	models := []OpenRouterRemoteModel{listing("openai/pg-sync", "0.000002", "0.00001")}
	if _, err := SyncOpenRouterModelListForTenant(context.Background(), systemTenantID, models); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if ids := modelConfigIDsWithPrefix(systemTenantID, "pg-"); len(ids) == 0 {
		t.Fatal("prefix lookup found nothing")
	}
}
