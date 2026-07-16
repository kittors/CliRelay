package postgrestest

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

const sharedRuntimeLockSQL = `SELECT pg_advisory_lock(hashtext('clirelay_shared_runtime_test_db')::bigint)`
const sharedRuntimeUnlockSQL = `SELECT pg_advisory_unlock(hashtext('clirelay_shared_runtime_test_db')::bigint)`

// LockSharedRuntimeDB serializes tests that mutate the shared
// CLIRELAY_POSTGRES_TEST_DSN runtime catalog. CI runs packages in parallel,
// so tests that TRUNCATE the same runtime tables must coordinate explicitly.
func LockSharedRuntimeDB(t *testing.T, dsn string) {
	t.Helper()

	if strings.TrimSpace(dsn) == "" {
		t.Fatal("postgres test dsn is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	db, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		cancel()
		t.Fatalf("open postgres test lock db: %v", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		cancel()
		t.Fatalf("open postgres test lock conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, sharedRuntimeLockSQL); err != nil {
		_ = conn.Close()
		_ = db.Close()
		cancel()
		t.Fatalf("acquire postgres shared runtime test lock: %v", err)
	}

	t.Cleanup(func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer unlockCancel()
		_, _ = conn.ExecContext(unlockCtx, sharedRuntimeUnlockSQL)
		_ = conn.Close()
		_ = db.Close()
		cancel()
	})
}
