package usage

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func setupDailySpendingResetDB(t *testing.T) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "daily-spending-reset-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	dbPath := tmpFile.Name()
	_ = tmpFile.Close()
	t.Cleanup(func() {
		CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func insertTodayCost(t *testing.T, tenantID, apiKeyID, apiKey string, cost float64) {
	t.Helper()
	db := getDB()
	at := CutoffStartUTC(1).Add(time.Hour)
	ts := at.Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO request_logs
		 (tenant_id, timestamp, api_key, api_key_id, model, source, failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 1, 0, 0, 0, 0, 0, ?)`,
		tenantID, ts, apiKey, apiKeyID, "model", "test", cost,
	); err != nil {
		t.Fatalf("insert request log: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin rollup: %v", err)
	}
	if err := projectUsageRollupTx(usageTestMetadataTx(tx), rollupEvent{
		TenantID: tenantID,
		APIKeyID: apiKeyID,
		Model:    "model",
		Source:   "test",
		Cost:     cost,
		At:       at,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("project rollup: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rollup: %v", err)
	}
}

func TestQueryTodayCostByKeyAppliesSameDayBaseline(t *testing.T) {
	setupDailySpendingResetDB(t)
	if err := UpsertAPIKey(APIKeyRow{ID: "key-1", Key: "sk-reset", DailySpendingLimit: 100}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	insertTodayCost(t, systemTenantID, "key-1", "sk-reset", 20)
	insertTodayCost(t, systemTenantID, "key-1", "sk-reset", 5)

	raw, err := QueryRawTodayCostByKeyForTenant(systemTenantID, "sk-reset")
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	if math.Abs(raw-25) > 1e-12 {
		t.Fatalf("raw = %v, want 25", raw)
	}

	got, err := QueryTodayCostByKey("sk-reset")
	if err != nil {
		t.Fatalf("effective before reset: %v", err)
	}
	if math.Abs(got-25) > 1e-12 {
		t.Fatalf("effective before reset = %v, want 25", got)
	}

	if err := UpsertDailySpendingReset(systemTenantID, "key-1", raw); err != nil {
		t.Fatalf("upsert reset: %v", err)
	}
	got, err = QueryTodayCostByKey("sk-reset")
	if err != nil {
		t.Fatalf("effective after reset: %v", err)
	}
	if math.Abs(got) > 1e-12 {
		t.Fatalf("effective after reset = %v, want 0", got)
	}

	insertTodayCost(t, systemTenantID, "key-1", "sk-reset", 3)
	got, err = QueryTodayCostByKey("sk-reset")
	if err != nil {
		t.Fatalf("effective after new spend: %v", err)
	}
	if math.Abs(got-3) > 1e-12 {
		t.Fatalf("effective after new spend = %v, want 3", got)
	}
}

func TestDailySpendingResetCrossDayIgnored(t *testing.T) {
	setupDailySpendingResetDB(t)
	if err := UpsertAPIKey(APIKeyRow{ID: "key-1", Key: "sk-day", DailySpendingLimit: 50}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	insertTodayCost(t, systemTenantID, "key-1", "sk-day", 10)

	db := getDB()
	if _, err := db.Exec(
		`INSERT INTO api_key_daily_spending_resets (tenant_id, api_key_id, day_key, cost_baseline, reset_at)
		 VALUES (?, ?, ?, ?, ?)`,
		systemTenantID, "key-1", "2000-01-01", 999, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert stale reset: %v", err)
	}

	got, err := QueryTodayCostByKey("sk-day")
	if err != nil {
		t.Fatalf("QueryTodayCostByKey: %v", err)
	}
	if math.Abs(got-10) > 1e-12 {
		t.Fatalf("stale baseline should not apply: got %v want 10", got)
	}
}

func TestDailySpendingResetRepeatedUpdatesBaseline(t *testing.T) {
	setupDailySpendingResetDB(t)
	if err := UpsertAPIKey(APIKeyRow{ID: "key-1", Key: "sk-twice", DailySpendingLimit: 100}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	insertTodayCost(t, systemTenantID, "key-1", "sk-twice", 10)
	if err := UpsertDailySpendingReset(systemTenantID, "key-1", 10); err != nil {
		t.Fatalf("first reset: %v", err)
	}
	insertTodayCost(t, systemTenantID, "key-1", "sk-twice", 7)
	raw, err := QueryRawTodayCostByKeyForTenant(systemTenantID, "sk-twice")
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	if err := UpsertDailySpendingReset(systemTenantID, "key-1", raw); err != nil {
		t.Fatalf("second reset: %v", err)
	}
	got, err := QueryTodayCostByKey("sk-twice")
	if err != nil {
		t.Fatalf("effective: %v", err)
	}
	if math.Abs(got) > 1e-12 {
		t.Fatalf("after second reset = %v, want 0", got)
	}
}

func TestDailySpendingResetTenantIsolation(t *testing.T) {
	setupDailySpendingResetDB(t)
	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"
	if err := UpsertAPIKeyForTenant(tenantA, APIKeyRow{ID: "a1", Key: "sk-a", DailySpendingLimit: 100}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if err := UpsertAPIKeyForTenant(tenantB, APIKeyRow{ID: "b1", Key: "sk-b", DailySpendingLimit: 100}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	insertTodayCost(t, tenantA, "a1", "sk-a", 30)
	insertTodayCost(t, tenantB, "b1", "sk-b", 40)

	if err := UpsertDailySpendingReset(tenantA, "a1", 30); err != nil {
		t.Fatalf("reset A: %v", err)
	}

	gotA, err := QueryTodayEffectiveCostByKeyForTenant(tenantA, "sk-a")
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	gotB, err := QueryTodayEffectiveCostByKeyForTenant(tenantB, "sk-b")
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if math.Abs(gotA) > 1e-12 {
		t.Fatalf("tenant A effective = %v, want 0", gotA)
	}
	if math.Abs(gotB-40) > 1e-12 {
		t.Fatalf("tenant B must stay 40, got %v", gotB)
	}
}

func TestDailySpendingRemaining(t *testing.T) {
	if got := DailySpendingRemaining(0, 10); got != nil {
		t.Fatalf("unlimited should be nil, got %v", *got)
	}
	got := DailySpendingRemaining(100, 20)
	if got == nil || math.Abs(*got-80) > 1e-12 {
		t.Fatalf("remaining = %v, want 80", got)
	}
	got = DailySpendingRemaining(10, 50)
	if got == nil || math.Abs(*got) > 1e-12 {
		t.Fatalf("floor remaining = %v, want 0", got)
	}
}

func TestBatchRawTodayCostsAndBaselines(t *testing.T) {
	setupDailySpendingResetDB(t)
	rows := []APIKeyRow{
		{ID: "k1", Key: "sk-1"},
		{ID: "k2", Key: "sk-2"},
	}
	for _, row := range rows {
		if err := UpsertAPIKey(row); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	insertTodayCost(t, systemTenantID, "k1", "sk-1", 1.5)
	insertTodayCost(t, systemTenantID, "k2", "sk-2", 2.5)
	if err := UpsertDailySpendingReset(systemTenantID, "k1", 1.0); err != nil {
		t.Fatalf("reset: %v", err)
	}

	costs, err := QueryRawTodayCostsByKeysForTenant(systemTenantID, rows)
	if err != nil {
		t.Fatalf("batch costs: %v", err)
	}
	if math.Abs(costs["k1"]-1.5) > 1e-12 || math.Abs(costs["k2"]-2.5) > 1e-12 {
		t.Fatalf("costs = %#v", costs)
	}
	baselines, err := ListDailySpendingResetBaselines(systemTenantID, []string{"k1", "k2"})
	if err != nil {
		t.Fatalf("baselines: %v", err)
	}
	if math.Abs(baselines["k1"]-1.0) > 1e-12 {
		t.Fatalf("baseline k1 = %v", baselines["k1"])
	}
	if _, ok := baselines["k2"]; ok {
		t.Fatalf("k2 should have no baseline")
	}
}

func TestBatchRawTodayCostsSumsModernAndLegacyRows(t *testing.T) {
	setupDailySpendingResetDB(t)
	if err := UpsertAPIKey(APIKeyRow{ID: "mix-1", Key: "sk-mix"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// modern row (api_key_id set) — rollup is keyed by stable api_key_id only
	insertTodayCost(t, systemTenantID, "mix-1", "sk-mix", 10)
	// second modern increment for same key id
	insertTodayCost(t, systemTenantID, "mix-1", "sk-mix", 4)

	single, err := QueryRawTodayCostByKeyForTenant(systemTenantID, "sk-mix")
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	batch, err := QueryRawTodayCostsByKeysForTenant(systemTenantID, []APIKeyRow{{ID: "mix-1", Key: "sk-mix"}})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if math.Abs(single-14) > 1e-12 {
		t.Fatalf("single raw = %v, want 14", single)
	}
	if math.Abs(batch["mix-1"]-14) > 1e-12 {
		t.Fatalf("batch raw = %v, want 14", batch["mix-1"])
	}
}

func TestEnsureAPIKeyDailySpendingResetsTableSQLHasNoDATETIME(t *testing.T) {
	if strings.Contains(strings.ToUpper(apiKeyDailySpendingResetsTableSQL), "DATETIME") {
		t.Fatalf("bootstrap SQL must not use DATETIME (PostgreSQL rejects it): %s", apiKeyDailySpendingResetsTableSQL)
	}
	if !strings.Contains(strings.ToUpper(apiKeyDailySpendingResetsTableSQL), "TIMESTAMP") {
		t.Fatalf("bootstrap SQL should use TIMESTAMP: %s", apiKeyDailySpendingResetsTableSQL)
	}
	setupDailySpendingResetDB(t)
	if err := ensureAPIKeyDailySpendingResetsTable(getDB()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
}

func TestDailySpendingResetEventHistory(t *testing.T) {
	setupDailySpendingResetDB(t)
	if err := UpsertAPIKey(APIKeyRow{ID: "key-1", Key: "sk-hist", DailySpendingLimit: 50}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	if err := InsertDailySpendingResetEvent(APIKeyDailySpendingResetEvent{
		TenantID:            systemTenantID,
		APIKeyID:            "key-1",
		CostBaseline:        12,
		EffectiveUsedBefore: 5,
		RawTodayCost:        12,
		ActorUsername:       "bob",
		ActorKind:           "user",
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := InsertDailySpendingResetEvent(APIKeyDailySpendingResetEvent{
		TenantID:            systemTenantID,
		APIKeyID:            "key-1",
		CostBaseline:        18,
		EffectiveUsedBefore: 6,
		RawTodayCost:        18,
		ActorUsername:       "carol",
		ActorKind:           "user",
	}); err != nil {
		t.Fatalf("insert event 2: %v", err)
	}
	n, err := CountDailySpendingResetEvents(systemTenantID, "key-1")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	counts, err := ListDailySpendingResetEventCounts(systemTenantID, []string{"key-1", "missing"})
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts["key-1"] != 2 {
		t.Fatalf("counts[key-1] = %d, want 2", counts["key-1"])
	}
	events, err := ListDailySpendingResetEvents(systemTenantID, "key-1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len events = %d, want 2", len(events))
	}
	if events[0].ActorUsername != "carol" {
		t.Fatalf("newest actor = %q, want carol", events[0].ActorUsername)
	}
	if err := DeleteDailySpendingResetEvents(systemTenantID, "key-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	n, err = CountDailySpendingResetEvents(systemTenantID, "key-1")
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if n != 0 {
		t.Fatalf("count after delete = %d, want 0", n)
	}
}
