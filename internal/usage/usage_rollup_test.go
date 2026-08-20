package usage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestUsageRollupSurvivesDetailDelete(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage-rollup.db")
	cleanupEnabled := false
	if err := InitDB(dbPath, config.RequestLogStorageConfig{
		RetentionDays:          7,
		ContentRetentionDays:   3,
		CleanupEnabled:         &cleanupEnabled,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         128,
		StoreContent:           false,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})

	// Relative to now, not a fixed date: the queries below ask for the last 30
	// days, so a hard-coded timestamp turns this into a test that passes until
	// the calendar walks past it and then fails forever.
	now := time.Now().UTC().Add(-12 * time.Hour)
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := projectUsageRollupTx(tx, rollupEvent{
		TenantID: systemTenantID,
		APIKeyID: "key-1",
		Model:    "gpt-test",
		Source:   "openai",
		Tokens:   TokenStats{InputTokens: 100, OutputTokens: 50, CachedTokens: 20, TotalTokens: 150},
		Cost:     0.02,
		At:       now,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("project: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Insert a detail row then delete it; rollup must remain.
	if _, err := getDB().Exec(`
		INSERT INTO request_logs (tenant_id, timestamp, api_key, api_key_id, model, source, failed, total_tokens, cost)
		VALUES (?, ?, ?, ?, ?, ?, 0, 150, 0.02)
	`, systemTenantID, now.UTC().Format(time.RFC3339Nano), "sk-rollup-1", "key-1", "gpt-test", "openai"); err != nil {
		t.Fatalf("insert detail: %v", err)
	}

	stats, err := QueryStats(LogQueryParams{TenantID: systemTenantID, Days: 30})
	if err != nil {
		t.Fatalf("QueryStats: %v", err)
	}
	if stats.Total != 1 {
		t.Fatalf("stats.Total = %d, want 1", stats.Total)
	}

	if _, err := getDB().Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatalf("delete request_logs: %v", err)
	}

	statsAfter, err := QueryStats(LogQueryParams{TenantID: systemTenantID, Days: 30})
	if err != nil {
		t.Fatalf("QueryStats after delete: %v", err)
	}
	if statsAfter.Total != 1 {
		t.Fatalf("stats after detail delete = %d, want 1 (projection must survive)", statsAfter.Total)
	}
	kpiAfter, err := QueryDashboardKPIForTenant(systemTenantID, 30)
	if err != nil {
		t.Fatalf("kpi after delete: %v", err)
	}
	if kpiAfter.TotalRequests != 1 {
		t.Fatalf("kpi after detail delete = %d, want 1", kpiAfter.TotalRequests)
	}
}

func TestQueryStatsDoesNotRequireRequestLogs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage-rollup-guard.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(CloseDB)

	// Seed projection without going through request_logs insert path.
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := projectUsageRollupTx(tx, rollupEvent{
		TenantID: systemTenantID,
		APIKeyID: "key-guard",
		Model:    "m",
		Source:   "s",
		Tokens:   TokenStats{InputTokens: 10, TotalTokens: 10},
		Cost:     0.01,
		At:       time.Now().UTC(),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("project: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Drop detail table rows if any; stats must still work.
	_, _ = getDB().Exec(`DELETE FROM request_logs`)
	stats, err := QueryStats(LogQueryParams{TenantID: systemTenantID, Days: 30})
	if err != nil {
		t.Fatalf("QueryStats: %v", err)
	}
	if stats.Total < 1 {
		t.Fatalf("expected rollup-backed stats, got %#v", stats)
	}
}

func TestInsertLogPersistsDetailWhenRollupProjectionFails(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: false})

	if _, err := getDB().Exec(`DROP TABLE usage_rollup_buckets`); err != nil {
		t.Fatalf("drop rollup table: %v", err)
	}

	InsertLogWithDetailsIdentitySubjectUpstreamVisionStreaming(
		systemTenantID,
		"sk-rollup-fail",
		"key-rollup-fail",
		"subject-rollup-fail",
		"Rollup Fail Test",
		"gpt-rollup-fail",
		"",
		"",
		"",
		"openai-compatibility",
		"test-channel",
		"0",
		false,
		time.Now().UTC(),
		100,
		20,
		TokenStats{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		`{"model":"gpt-rollup-fail","stream":true}`,
		"",
		"",
		true,
	)

	var count int
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM request_logs WHERE api_key_id = ?`, "key-rollup-fail").Scan(&count); err != nil {
		t.Fatalf("count request log: %v", err)
	}
	if count != 1 {
		t.Fatalf("request log count = %d, want 1", count)
	}
}
