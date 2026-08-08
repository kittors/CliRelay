package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

// TestAuditLogReadNoiseCleanupMigration covers the one-time cleanup of rows the
// pre-fix audit policy wrote for read traffic. It replays the migration SQL under
// a throwaway version, because the shipped version has already been applied by
// the time the fixture has rows to clean.
func TestAuditLogReadNoiseCleanupMigration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	db := newDisposableMigratedDB(t, ctx, dsn, "audit_noise")

	// Bucket boundaries are absolute (epoch/300), and midnight UTC is aligned, so
	// these offsets land in exactly three five-minute buckets.
	base := "2026-08-07 00:00:00+00"
	seed := func(offsetSeconds int, actorKind, action, resourceType, resourceID, result string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO audit_logs (tenant_id, actor_kind, action, resource_type, resource_id, result, request_id, changes, created_at)
			VALUES (NULL, $1, $2, $3, $4, $5, '', '{}'::jsonb, TIMESTAMPTZ '`+base+`' + make_interval(secs => $6))
		`, actorKind, action, resourceType, resourceID, result, offsetSeconds); err != nil {
			t.Fatalf("seed audit row: %v", err)
		}
	}

	// The observed flood: a panel polling one read endpoint it may not call.
	for _, offset := range []int{0, 60, 299, 300, 599, 600} {
		seed(offset, "user_session", "management.get", "update", "progress", "denied")
	}
	// Reads that merely errored are no longer audit events at all.
	for _, offset := range []int{0, 120, 240} {
		seed(offset, "user_session", "management.get", "update", "progress", "failed")
	}
	// A different route is a different signature and keeps its own row per bucket.
	seed(60, "user_session", "management.get", "update", "check", "denied")
	// Sensitive reads stay audited under the new policy, successful or not.
	seed(60, "user_session", "management.get", "usage", "logs/1/content", "success")
	seed(61, "user_session", "management.get", "usage", "logs/2/content", "failed")
	// Writes are untouched.
	seed(60, "user_session", "management.post", "users", "u1", "success")
	seed(61, "user_session", "management.delete", "users", "u1", "failed")

	if err := ApplyMigrations(ctx, db, []Migration{
		{Version: "test_replay_audit_log_read_noise_cleanup", SQL: auditLogReadNoiseCleanupSQL},
	}); err != nil {
		t.Fatalf("replay cleanup migration: %v", err)
	}

	count := func(query string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("count query: %v", err)
		}
		return n
	}

	if got := count(`SELECT COUNT(*) FROM audit_logs WHERE resource_type = 'update' AND resource_id = 'progress' AND result = 'denied'`); got != 3 {
		t.Fatalf("collapsed refusals = %d rows, want 3 (one per five-minute bucket)", got)
	}
	if got := count(`SELECT COUNT(*) FROM audit_logs WHERE resource_type = 'update' AND result = 'failed'`); got != 0 {
		t.Fatalf("errored reads = %d rows, want 0", got)
	}
	if got := count(`SELECT COUNT(*) FROM audit_logs WHERE resource_type = 'update' AND resource_id = 'check'`); got != 1 {
		t.Fatalf("other refused route = %d rows, want 1", got)
	}
	if got := count(`SELECT COUNT(*) FROM audit_logs WHERE resource_type = 'usage'`); got != 2 {
		t.Fatalf("sensitive reads = %d rows, want 2", got)
	}
	if got := count(`SELECT COUNT(*) FROM audit_logs WHERE action IN ('management.post', 'management.delete')`); got != 2 {
		t.Fatalf("writes = %d rows, want 2", got)
	}
	// The surviving row of each bucket is its first, so the trail still starts
	// where the refusals started.
	var earliest time.Time
	if err := db.QueryRowContext(ctx, `
		SELECT MIN(created_at) FROM audit_logs WHERE resource_type = 'update' AND resource_id = 'progress'
	`).Scan(&earliest); err != nil {
		t.Fatalf("read earliest surviving row: %v", err)
	}
	if offset := earliest.UTC().Sub(time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)); offset != 0 {
		t.Fatalf("earliest surviving row is %v past the base, want the first refusal", offset)
	}
}

// newDisposableMigratedDB creates a throwaway database with the full schema so
// migration tests cannot disturb the shared catalog.
func newDisposableMigratedDB(t *testing.T, ctx context.Context, dsn, prefix string) *sql.DB {
	t.Helper()
	adminDB, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = adminDB.PingContext(ctx); err != nil {
		_ = adminDB.Close()
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err = adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create disposable db: %v", err)
	}
	// Registered first so LIFO cleanup runs: runtime close → drop → admin close.
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := adminDB.ExecContext(cleanupCtx, `
			SELECT pg_terminate_backend(pid)
			  FROM pg_stat_activity
			 WHERE datname = $1 AND pid <> pg_backend_pid()
		`, dbName); err != nil {
			t.Errorf("terminate connections on disposable db %s: %v", dbName, err)
		}
		if _, err := adminDB.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Errorf("drop disposable db %s: %v", dbName, err)
		}
		if err := adminDB.Close(); err != nil {
			t.Errorf("close admin db: %v", err)
		}
	})
	testDSN, err := replacePostgresDatabase(dsn, dbName)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close runtime db: %v", err)
		}
	})
	if err = db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err = ApplyMigrations(ctx, db, RuntimeMigrations()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}
