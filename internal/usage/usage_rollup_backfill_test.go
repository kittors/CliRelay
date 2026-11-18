package usage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestUsageRollupBackfillResolvesLegacyKeyAndEndUser(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-backfill.db")
	// Init without marker so we control order: insert legacy rows first via raw SQL
	// after Init (marker already done on empty). Force rebuild by clearing marker.
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})

	endUserID := "eu-backfill-1"
	if err := UpsertAPIKey(APIKeyRow{
		ID: "key-bf-1", Key: "sk-bf-legacy", Name: "BF", EndUserID: endUserID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	// Legacy detail: empty api_key_id, raw secret only.
	at := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	if _, err := getDB().Exec(`
		INSERT INTO request_logs
		(tenant_id, timestamp, api_key, api_key_id, model, source, failed, total_tokens, cost)
		VALUES (?, ?, ?, '', 'm', 's', 0, 10, 1.5)
	`, systemTenantID, at.Format(time.RFC3339Nano), "sk-bf-legacy"); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}

	// Simulate incomplete first backfill then repair: clear marker + rollup and re-run after key exists.
	if _, err := getDB().Exec(`DELETE FROM usage_rollup_buckets`); err != nil {
		t.Fatalf("clear rollup: %v", err)
	}
	if _, err := getDB().Exec(`DELETE FROM usage_projection_markers WHERE marker_key = ?`, usageRollupBackfillMarker); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	// Repair api_key_id like production backfill does.
	if _, err := getDB().Exec(`UPDATE request_logs SET api_key_id = 'key-bf-1' WHERE api_key = 'sk-bf-legacy'`); err != nil {
		t.Fatalf("repair key id: %v", err)
	}
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerDone); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var keyTotal, userTotal float64
	if err := getDB().QueryRow(`
		SELECT COALESCE(SUM(cost_total),0) FROM usage_rollup_buckets
		WHERE bucket_kind='lifetime' AND api_key_id='key-bf-1'
	`).Scan(&keyTotal); err != nil {
		t.Fatalf("query key: %v", err)
	}
	if keyTotal != 1.5 {
		t.Fatalf("keyed rollup total = %v, want 1.5 after legacy identity repair", keyTotal)
	}
	if err := getDB().QueryRow(`
		SELECT COALESCE(SUM(cost_total),0) FROM usage_rollup_buckets
		WHERE bucket_kind='lifetime' AND end_user_id=?
	`, endUserID).Scan(&userTotal); err != nil {
		t.Fatalf("query user: %v", err)
	}
	if userTotal != 1.5 {
		t.Fatalf("end-user rollup total = %v, want 1.5", userTotal)
	}
}

func TestUsageRollupBackfillIsReentrantWithoutDoubleCount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-reentrant.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	if err := UpsertAPIKey(APIKeyRow{ID: "key-r1", Key: "sk-r1"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	at := time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC)
	if _, err := getDB().Exec(`
		INSERT INTO request_logs
		(tenant_id, timestamp, api_key, api_key_id, model, source, failed, total_tokens, cost)
		VALUES (?, ?, 'sk-r1', 'key-r1', 'm', 's', 0, 1, 2)
	`, systemTenantID, at.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Absolute rebuild twice while still pending must not double.
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerPending); err != nil {
		t.Fatalf("backfill 1: %v", err)
	}
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerDone); err != nil {
		t.Fatalf("backfill 2: %v", err)
	}
	// Once done, further rebuilds are no-ops (would otherwise wipe lifetime history).
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerDone); err != nil {
		t.Fatalf("backfill 3: %v", err)
	}
	var total float64
	if err := getDB().QueryRow(`
		SELECT COALESCE(SUM(cost_total),0) FROM usage_rollup_buckets
		WHERE bucket_kind='lifetime' AND api_key_id='key-r1'
	`).Scan(&total); err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 2 {
		t.Fatalf("lifetime rollup total after replay = %v, want 2 (not doubled)", total)
	}
}

func TestUsageRollupBlueGreenCatchupIncludesOldSlotRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-bluegreen.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	if err := UpsertAPIKey(APIKeyRow{ID: "key-bg", Key: "sk-bg"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	at := time.Now().UTC()
	// Row present at first rebuild.
	if _, err := getDB().Exec(`
		INSERT INTO request_logs
		(tenant_id, timestamp, api_key, api_key_id, model, source, failed, total_tokens, cost)
		VALUES (?, ?, 'sk-bg', 'key-bg', 'm', 's', 0, 1, 1)
	`, systemTenantID, at.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerPending); err != nil {
		t.Fatalf("initial backfill: %v", err)
	}
	if projectionMarkerValue(getDB(), usageRollupBackfillMarker) != rollupMarkerPending {
		t.Fatalf("marker after init = %q, want pending", projectionMarkerValue(getDB(), usageRollupBackfillMarker))
	}
	// Simulate old slot writing a detail row without rollup while still pending.
	if _, err := getDB().Exec(`
		INSERT INTO request_logs
		(tenant_id, timestamp, api_key, api_key_id, model, source, failed, total_tokens, cost)
		VALUES (?, ?, 'sk-bg', 'key-bg', 'm', 's', 0, 1, 3)
	`, systemTenantID, at.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert old-slot: %v", err)
	}
	// Post-drain catch-up absolute rebuild includes old-slot row and marks done.
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerDone); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	var after float64
	if err := getDB().QueryRow(`
		SELECT COALESCE(SUM(cost_total),0) FROM usage_rollup_buckets
		WHERE bucket_kind='lifetime' AND api_key_id='key-bg'
	`).Scan(&after); err != nil {
		t.Fatalf("after: %v", err)
	}
	if after != 4 {
		t.Fatalf("after catch-up total = %v, want 4 (1+3)", after)
	}
	if !usageRollupBackfillCompleted(getDB()) {
		t.Fatal("marker should be done after catch-up")
	}
}

func TestUsageRollupMaintenanceDoesNotFinalizeBeforeDrainWindow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-early.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	// Simulate init pending without scheduling catch-up (earliest zero).
	if err := setProjectionMarker(getDB(), usageRollupBackfillMarker, rollupMarkerPending); err != nil {
		t.Fatalf("pending: %v", err)
	}
	rollupCatchupEarliestMu.Lock()
	rollupCatchupEarliest = time.Time{}
	rollupCatchupEarliestMu.Unlock()
	maybeFinalizeUsageRollupCatchup(getDB())
	if projectionMarkerValue(getDB(), usageRollupBackfillMarker) != rollupMarkerPending {
		t.Fatalf("marker = %q, want pending when drain window not open", projectionMarkerValue(getDB(), usageRollupBackfillMarker))
	}
	// Future earliest still blocks.
	rollupCatchupEarliestMu.Lock()
	rollupCatchupEarliest = time.Now().Add(time.Hour)
	rollupCatchupEarliestMu.Unlock()
	maybeFinalizeUsageRollupCatchup(getDB())
	if projectionMarkerValue(getDB(), usageRollupBackfillMarker) != rollupMarkerPending {
		t.Fatalf("marker = %q, want pending before earliest", projectionMarkerValue(getDB(), usageRollupBackfillMarker))
	}
	// Past earliest allows finalize.
	rollupCatchupEarliestMu.Lock()
	rollupCatchupEarliest = time.Now().Add(-time.Second)
	rollupCatchupEarliestMu.Unlock()
	maybeFinalizeUsageRollupCatchup(getDB())
	if !usageRollupBackfillCompleted(getDB()) {
		t.Fatal("marker should be done after earliest elapsed")
	}
}

func TestUsageRollupConcurrentFinalizeDoesNotWipeAfterDone(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-concurrent.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	if err := UpsertAPIKey(APIKeyRow{ID: "key-c", Key: "sk-c"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	// Lifetime-only history + done marker.
	if _, err := getDB().Exec(`
		INSERT INTO usage_rollup_buckets (
			tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id,
			model, source, channel_name, request_count, success_count, failure_count, streaming_count,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens, effective_input_tokens,
			total_tokens, cost_total, latency_sum_ms, latency_count, first_token_sum_ms, first_token_count, updated_at
		) VALUES (?, 'lifetime', ?, 'key-c', '', '', '', '', '', 10, 10, 0, 0, 0, 0, 0, 0, 0, 10, 99, 0, 0, 0, 0, ?)
	`, systemTenantID, rollupLifetimeStart, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := setProjectionMarker(getDB(), usageRollupBackfillMarker, rollupMarkerDone); err != nil {
		t.Fatalf("done: %v", err)
	}
	// Concurrent "finalize" attempts must no-op under lock re-check.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerDone)
		}()
	}
	wg.Wait()
	var total float64
	if err := getDB().QueryRow(`
		SELECT COALESCE(SUM(cost_total),0) FROM usage_rollup_buckets
		WHERE bucket_kind='lifetime' AND api_key_id='key-c'
	`).Scan(&total); err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 99 {
		t.Fatalf("lifetime cost after concurrent finalize = %v, want 99", total)
	}
}

func TestUsageRollupCatchupDoesNotWipeLifetimeAfterDetailPurged(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-no-wipe.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	if err := UpsertAPIKey(APIKeyRow{ID: "key-keep", Key: "sk-keep"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	// Seed lifetime-only history that no longer has detail rows.
	if _, err := getDB().Exec(`
		INSERT INTO usage_rollup_buckets (
			tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id,
			model, source, channel_name, request_count, success_count, failure_count, streaming_count,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens, effective_input_tokens,
			total_tokens, cost_total, latency_sum_ms, latency_count, first_token_sum_ms, first_token_count, updated_at
		) VALUES (?, 'lifetime', ?, 'key-keep', '', '', '', '', '', 100, 100, 0, 0, 0, 0, 0, 0, 0, 100, 50, 0, 0, 0, 0, ?)
	`, systemTenantID, rollupLifetimeStart, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed lifetime: %v", err)
	}
	if err := setProjectionMarker(getDB(), usageRollupBackfillMarker, rollupMarkerDone); err != nil {
		t.Fatalf("marker done: %v", err)
	}
	// Catch-up after done must no-op and preserve lifetime history.
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerDone); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	var total float64
	if err := getDB().QueryRow(`
		SELECT COALESCE(SUM(cost_total),0) FROM usage_rollup_buckets
		WHERE bucket_kind='lifetime' AND api_key_id='key-keep'
	`).Scan(&total); err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 50 {
		t.Fatalf("lifetime cost after catch-up = %v, want 50 preserved", total)
	}
}

func TestMetadataCleanupWaitsForRollupBackfillMarker(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cleanup-gate.db")
	enabled := true
	if err := InitDB(dbPath, config.RequestLogStorageConfig{
		RetentionDays:            1,
		CleanupEnabled:           &enabled,
		CleanupBatchSize:         100,
		CleanupMaxRuntimeSeconds: 10,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	// Clear marker to simulate upgrade before backfill.
	if _, err := getDB().Exec(`DELETE FROM usage_projection_markers WHERE marker_key = ?`, usageRollupBackfillMarker); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	oldTS := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339Nano)
	if _, err := getDB().Exec(`
		INSERT INTO request_logs (tenant_id, timestamp, api_key, model, source, failed, total_tokens, cost)
		VALUES (?, ?, 'sk-old', 'm', 's', 0, 1, 1)
	`, systemTenantID, oldTS); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	n, err := cleanupExpiredRequestLogMetadata(context.Background(), getDB())
	if err != nil {
		t.Fatalf("cleanup before marker: %v", err)
	}
	if n != 0 {
		t.Fatalf("deleted %d rows before backfill marker, want 0", n)
	}
	var count int64
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("request_logs count = %d, want 1 preserved until backfill", count)
	}
	// After marker, retention cleanup may delete the old row.
	if err := setProjectionMarker(getDB(), usageRollupBackfillMarker, rollupMarkerDone); err != nil {
		t.Fatalf("set marker: %v", err)
	}
	n, err = cleanupExpiredRequestLogMetadata(context.Background(), getDB())
	if err != nil {
		t.Fatalf("cleanup after marker: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d after marker, want 1", n)
	}
}

func TestUsageRollupBackfillDoesNotMarkDoneOnReadFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-fail.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	// Drop request_logs to force query failure after marker cleared.
	if _, err := getDB().Exec(`DROP TABLE request_logs`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := getDB().Exec(`DELETE FROM usage_projection_markers WHERE marker_key = ?`, usageRollupBackfillMarker); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerDone); err == nil {
		t.Fatal("expected backfill error when request_logs missing")
	}
	if projectionMarkerValue(getDB(), usageRollupBackfillMarker) == rollupMarkerDone {
		t.Fatal("marker must not be done after failed backfill")
	}
}

func TestUsageRollupRetentionRunsWhenDetailCleanupDisabled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-cleanup-flag.db")
	disabled := false
	if err := InitDB(dbPath, config.RequestLogStorageConfig{
		RetentionDays:  7,
		CleanupEnabled: &disabled,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	now := time.Now().UTC()
	if _, err := getDB().Exec(`
		INSERT INTO usage_rollup_buckets (
			tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id,
			model, source, channel_name, request_count, success_count, failure_count, streaming_count,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens, effective_input_tokens,
			total_tokens, cost_total, latency_sum_ms, latency_count, first_token_sum_ms, first_token_count, updated_at
		) VALUES (?, 'minute', ?, 'k', '', '', 'm', 's', '', 1, 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0, 0, 0, 0, ?)
	`, systemTenantID, now.Add(-48*time.Hour).Format("2006-01-02T15:04"), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Simulate maintenance pass with detail cleanup disabled.
	runRequestLogMaintenancePass(context.Background(), getDB(), "sqlite")
	var n int64
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM usage_rollup_buckets WHERE bucket_kind='minute'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("stale minute buckets remaining = %d, want 0 even when cleanup-enabled=false", n)
	}
}

func TestUsageRollupBucketRetentionPrunesMinuteHourDay(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup-retention.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{RetentionDays: 7}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		stopRequestLogMaintenance()
		CloseDB()
	})
	now := time.Now().UTC()
	// Insert stale minute/hour/day and a lifetime row.
	for _, row := range []struct {
		kind, start string
	}{
		{rollupBucketMinute, now.Add(-48 * time.Hour).Format("2006-01-02T15:04")},
		{rollupBucketHour, now.Add(-40 * 24 * time.Hour).Format("2006-01-02T15")},
		{rollupBucketDay, now.Add(-500 * 24 * time.Hour).Format("2006-01-02")},
		{rollupBucketLifetime, rollupLifetimeStart},
	} {
		if _, err := getDB().Exec(`
			INSERT INTO usage_rollup_buckets (
				tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id,
				model, source, channel_name, request_count, success_count, failure_count, streaming_count,
				input_tokens, output_tokens, reasoning_tokens, cached_tokens, effective_input_tokens,
				total_tokens, cost_total, latency_sum_ms, latency_count, first_token_sum_ms, first_token_count, updated_at
			) VALUES (?, ?, ?, 'k', '', '', 'm', 's', '', 1, 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0, 0, 0, 0, ?)
		`, systemTenantID, row.kind, row.start, now.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert %s: %v", row.kind, err)
		}
	}
	n, err := cleanupExpiredUsageRollupBuckets(getDB())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n < 3 {
		t.Fatalf("pruned = %d, want at least 3", n)
	}
	var life int64
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM usage_rollup_buckets WHERE bucket_kind='lifetime'`).Scan(&life); err != nil {
		t.Fatalf("count lifetime: %v", err)
	}
	if life != 1 {
		t.Fatalf("lifetime rows = %d, want 1", life)
	}
}
