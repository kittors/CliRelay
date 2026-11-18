package usage_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func setupEndUserKeyLifecycleDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "enduser-key-lifecycle-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	dbPath := tmpFile.Name()
	_ = tmpFile.Close()
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := usage.RuntimeDB()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			must_change_password INTEGER NOT NULL DEFAULT 0,
			password_changed_at TEXT,
			last_login_at TEXT,
			failed_login_count INTEGER NOT NULL DEFAULT 0,
			lock_stage INTEGER NOT NULL DEFAULT 0,
			locked_until TEXT,
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL DEFAULT 1
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	if err := usage.EnsureEndUserQuotaColumns(db); err != nil {
		t.Fatalf("EnsureEndUserQuotaColumns: %v", err)
	}
	return db
}

func insertLifecycleUser(t *testing.T, db *sql.DB, tenantID, userID string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO end_users (
			id, tenant_id, username, username_normalized, display_name, password_hash,
			status, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'Lifecycle User', 'x', 'active', ?, ?)
	`, userID, tenantID, userID, userID, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert end user: %v", err)
	}
}

func insertLifecycleLog(t *testing.T, db *sql.DB, tenantID, apiKey, apiKeyID string, cost float64) {
	t.Helper()
	at := usage.CutoffStartUTC(1).Add(time.Hour)
	endUserID := ""
	if row := usage.GetAPIKey(apiKey); row != nil {
		endUserID = strings.TrimSpace(row.EndUserID)
	}
	if _, err := db.Exec(`
		INSERT INTO request_logs (
			tenant_id, timestamp, api_key, api_key_id, api_key_name, model, source,
			failed, total_tokens, cost
		) VALUES (?, ?, ?, ?, 'Lifecycle Key', 'gpt-test', 'test', 0, 1, ?)
	`, tenantID, at.Format(time.RFC3339), apiKey, apiKeyID, cost); err != nil {
		t.Fatalf("insert request log: %v", err)
	}
	// Mirror production rollup UPSERT (external package cannot call unexported helpers).
	for _, kindStart := range []struct{ kind, start string }{
		{"minute", at.UTC().Format("2006-01-02T15:04")},
		{"hour", at.UTC().Format("2006-01-02T15")},
		{"day", at.UTC().Format("2006-01-02")},
		{"lifetime", "1970-01-01"},
	} {
		if _, err := db.Exec(`
			INSERT INTO usage_rollup_buckets (
				tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id,
				model, source, channel_name,
				request_count, success_count, failure_count, streaming_count,
				input_tokens, output_tokens, reasoning_tokens, cached_tokens,
				effective_input_tokens, total_tokens, cost_total,
				latency_sum_ms, latency_count, first_token_sum_ms, first_token_count, updated_at
			) VALUES (?, ?, ?, ?, ?, '', 'gpt-test', 'test', '', 1, 1, 0, 0, 0, 0, 0, 0, 0, 1, ?, 0, 0, 0, 0, ?)
			ON CONFLICT(
				tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id,
				model, source, channel_name
			) DO UPDATE SET
				request_count = usage_rollup_buckets.request_count + 1,
				success_count = usage_rollup_buckets.success_count + 1,
				total_tokens = usage_rollup_buckets.total_tokens + 1,
				cost_total = usage_rollup_buckets.cost_total + excluded.cost_total,
				updated_at = excluded.updated_at
		`, tenantID, kindStart.kind, kindStart.start, apiKeyID, endUserID, cost, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert rollup %s: %v", kindStart.kind, err)
		}
	}
}

func TestSoftDeletedOwnedKeyKeepsAccountHistory(t *testing.T) {
	db := setupEndUserKeyLifecycleDB(t)
	tenantID := uuid.NewString()
	userID := uuid.NewString()
	insertLifecycleUser(t, db, tenantID, userID)

	svc := enduser.NewService(db)
	first, err := svc.CreateKey(context.Background(), tenantID, userID, "first")
	if err != nil {
		t.Fatalf("CreateKey first: %v", err)
	}
	second, err := svc.CreateKey(context.Background(), tenantID, userID, "second")
	if err != nil {
		t.Fatalf("CreateKey second: %v", err)
	}
	insertLifecycleLog(t, db, tenantID, first.PlaintextKey, first.APIKey.ID, 1)
	insertLifecycleLog(t, db, tenantID, second.PlaintextKey, second.APIKey.ID, 2)

	if err := svc.DeleteKey(context.Background(), tenantID, userID, first.APIKey.ID); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	keys, err := svc.ListKeys(context.Background(), tenantID, userID)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != second.APIKey.ID {
		t.Fatalf("active keys after delete = %#v, want only %s", keys, second.APIKey.ID)
	}
	if err := svc.DeleteKey(context.Background(), tenantID, userID, second.APIKey.ID); !errors.Is(err, enduser.ErrLastKey) {
		t.Fatalf("delete last active key err = %v, want ErrLastKey", err)
	}

	var disabled int
	var ownerID, storedSecret string
	if err := db.QueryRow(`SELECT disabled, end_user_id, key FROM api_keys WHERE tenant_id = ? AND id = ?`, tenantID, first.APIKey.ID).Scan(&disabled, &ownerID, &storedSecret); err != nil {
		t.Fatalf("query soft-deleted key: %v", err)
	}
	if disabled != 1 || ownerID != userID {
		t.Fatalf("soft-deleted row = disabled:%d owner:%q", disabled, ownerID)
	}
	// Deleted secret must be permanently invalidated (not the original plaintext).
	if storedSecret == first.PlaintextKey {
		t.Fatalf("soft-deleted secret still equals original plaintext")
	}
	if !strings.HasPrefix(storedSecret, "sk-deleted-") {
		t.Fatalf("soft-deleted secret = %q, want sk-deleted- prefix", storedSecret)
	}

	params := usage.LogQueryParams{TenantID: tenantID, EndUserID: userID, Page: 1, Size: 20, Days: 1}
	logs, err := usage.QueryLogs(params)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if logs.Total != 2 {
		t.Fatalf("account logs after soft delete = %d, want 2", logs.Total)
	}
	stats, err := usage.QueryStats(params)
	if err != nil {
		t.Fatalf("QueryStats: %v", err)
	}
	if stats.Total != 2 || stats.TotalCost != 3 {
		t.Fatalf("account stats after soft delete = %+v, want total=2 cost=3", stats)
	}
	chart, err := usage.QueryPublicChartData(second.PlaintextKey, 7)
	if err != nil {
		t.Fatalf("QueryPublicChartData: %v", err)
	}
	if chart.Stats.Total != 2 || chart.Stats.TotalCost != 3 {
		t.Fatalf("public chart after soft delete = %+v, want total=2 cost=3", chart.Stats)
	}
}

func TestRotateBackfillsLegacyRequestLogIdentity(t *testing.T) {
	db := setupEndUserKeyLifecycleDB(t)
	tenantID := uuid.NewString()
	userID := uuid.NewString()
	insertLifecycleUser(t, db, tenantID, userID)

	svc := enduser.NewService(db)
	created, err := svc.CreateKey(context.Background(), tenantID, userID, "legacy")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	insertLifecycleLog(t, db, tenantID, created.PlaintextKey, "", 4)

	rotated, err := svc.RotateKey(context.Background(), tenantID, userID, created.APIKey.ID)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	var backfilledID string
	if err := db.QueryRow(`SELECT api_key_id FROM request_logs WHERE tenant_id = ? AND api_key = ?`, tenantID, created.PlaintextKey).Scan(&backfilledID); err != nil {
		t.Fatalf("query legacy request log: %v", err)
	}
	if backfilledID != created.APIKey.ID {
		t.Fatalf("legacy api_key_id after rotate = %q, want %q", backfilledID, created.APIKey.ID)
	}

	params := usage.LogQueryParams{TenantID: tenantID, EndUserID: userID, Page: 1, Size: 20, Days: 1}
	logs, err := usage.QueryLogs(params)
	if err != nil {
		t.Fatalf("QueryLogs after rotate: %v", err)
	}
	if logs.Total != 1 {
		t.Fatalf("account logs after rotate = %d, want 1", logs.Total)
	}
	chart, err := usage.QueryPublicChartData(rotated.PlaintextKey, 7)
	if err != nil {
		t.Fatalf("QueryPublicChartData after rotate: %v", err)
	}
	if chart.Stats.Total != 1 || chart.Stats.TotalCost != 4 {
		t.Fatalf("public chart after rotate = %+v, want total=1 cost=4", chart.Stats)
	}
}
