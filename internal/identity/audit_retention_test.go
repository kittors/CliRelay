package identity

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

func TestAuditRetentionPolicyFromConfig(t *testing.T) {
	if got := AuditRetentionPolicyFromConfig(nil); got != DefaultAuditRetentionPolicy() {
		t.Fatalf("nil config = %+v, want defaults %+v", got, DefaultAuditRetentionPolicy())
	}

	cfg := &config.Config{}
	cfg.RemoteManagement.Auth.AuditRetentionDays = 30
	policy := AuditRetentionPolicyFromConfig(cfg)
	if policy.MaxAge != 30*24*time.Hour {
		t.Fatalf("MaxAge = %v, want 720h", policy.MaxAge)
	}
	// Configuring one limit must not silently drop the other.
	if policy.MaxRows != defaultAuditMaxRows {
		t.Fatalf("MaxRows = %d, want default %d", policy.MaxRows, defaultAuditMaxRows)
	}

	cfg.RemoteManagement.Auth.AuditRetentionDays = -1
	cfg.RemoteManagement.Auth.AuditMaxRows = -1
	disabled := AuditRetentionPolicyFromConfig(cfg)
	if disabled.MaxAge != 0 || disabled.MaxRows != 0 {
		t.Fatalf("negative config = %+v, want both limits disabled", disabled)
	}
}

func TestPruneAuditLogsNilService(t *testing.T) {
	var service *Service
	deleted, err := service.PruneAuditLogs(context.Background(), DefaultAuditRetentionPolicy())
	if err != nil || deleted != 0 {
		t.Fatalf("nil service prune = (%d, %v), want (0, nil)", deleted, err)
	}
}

// TestPostgresPruneAuditLogs covers the two limits that keep audit_logs bounded.
// Before retention existed the table only ever grew; the panel's "clear all" was
// the sole way to shrink it, and it discards the entire trail.
func TestPostgresPruneAuditLogs(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	ctx := context.Background()
	db, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Leave the shared catalog clean for the next test, then close: cleanups run
	// after the test body, so a deferred close would beat the truncate to it.
	t.Cleanup(func() {
		if _, cleanupErr := db.ExecContext(context.Background(), `TRUNCATE audit_logs`); cleanupErr != nil {
			t.Errorf("truncate audit_logs: %v", cleanupErr)
		}
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close runtime db: %v", closeErr)
		}
	})
	if _, err = db.ExecContext(ctx, `TRUNCATE audit_logs`); err != nil {
		t.Fatal(err)
	}

	service := NewService(db)
	insert := func(age time.Duration, action string) {
		t.Helper()
		if _, err = db.ExecContext(ctx, `
			INSERT INTO audit_logs (tenant_id, actor_kind, action, resource_type, resource_id, result, request_id, changes, created_at)
			VALUES (NULL, 'system', ?, 'test', '', 'success', '', '{}'::jsonb, ?)
		`, action, time.Now().UTC().Add(-age)); err != nil {
			t.Fatalf("insert audit row: %v", err)
		}
	}

	for i := 0; i < 5; i++ {
		insert(90*24*time.Hour, "test.old")
	}
	for i := 0; i < 7; i++ {
		insert(time.Hour, "test.recent")
	}

	deleted, err := service.PruneAuditLogs(ctx, AuditRetentionPolicy{MaxAge: 30 * 24 * time.Hour, BatchSize: 2})
	if err != nil {
		t.Fatalf("prune by age: %v", err)
	}
	if deleted != 5 {
		t.Fatalf("age prune deleted %d rows, want 5", deleted)
	}
	var remaining int64
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 7 {
		t.Fatalf("rows after age prune = %d, want 7", remaining)
	}

	deleted, err = service.PruneAuditLogs(ctx, AuditRetentionPolicy{MaxRows: 3, BatchSize: 2})
	if err != nil {
		t.Fatalf("prune by row cap: %v", err)
	}
	if deleted != 4 {
		t.Fatalf("row cap prune deleted %d rows, want 4", deleted)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 3 {
		t.Fatalf("rows after cap prune = %d, want 3", remaining)
	}

	// Both limits satisfied: a further pass must be a no-op rather than looping.
	if deleted, err = service.PruneAuditLogs(ctx, AuditRetentionPolicy{MaxAge: 30 * 24 * time.Hour, MaxRows: 3}); err != nil || deleted != 0 {
		t.Fatalf("idempotent pass = (%d, %v), want (0, nil)", deleted, err)
	}
}
