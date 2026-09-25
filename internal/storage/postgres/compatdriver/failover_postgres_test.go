package compatdriver

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// These tests make one pooled session read-only, which is what a demoted
// primary looks like to connections that survive a switchover.

func openFailoverTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS compatdriver_failover_probe (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open(DriverName, dsn)
		if err == nil {
			_, _ = cleanup.Exec(`DROP TABLE IF EXISTS compatdriver_failover_probe`)
			_ = cleanup.Close()
		}
	})
	return db
}

func backendPID(t *testing.T, db *sql.DB) int {
	t.Helper()
	var pid int
	if err := db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func probeRows(t *testing.T, db *sql.DB, note string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM compatdriver_failover_probe WHERE note = $1`, note).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPostgresReadOnlySessionIsReplacedAndWriteRetried(t *testing.T) {
	db := openFailoverTestDB(t)
	before := backendPID(t, db)
	if _, err := db.Exec(`SET default_transaction_read_only = on`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO compatdriver_failover_probe (note) VALUES ('autocommit')`); err != nil {
		t.Fatalf("write after the session went read-only must be retried on a new connection: %v", err)
	}
	if after := backendPID(t, db); after == before {
		t.Fatalf("read-only session %d was reused", before)
	}
	if n := probeRows(t, db, "autocommit"); n != 1 {
		t.Fatalf("retried write applied %d times, want exactly once", n)
	}
}

func TestPostgresReadOnlyInsideTransactionSurfacesAndDiscards(t *testing.T) {
	db := openFailoverTestDB(t)
	before := backendPID(t, db)
	if _, err := db.Exec(`SET default_transaction_read_only = on`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`INSERT INTO compatdriver_failover_probe (note) VALUES ('in-tx')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstateReadOnlyTransaction {
		t.Fatalf("expected the 25006 error inside the transaction, got %v", err)
	}
	_ = tx.Rollback()
	if after := backendPID(t, db); after == before {
		t.Fatalf("read-only session %d went back into the pool", before)
	}
	if n := probeRows(t, db, "in-tx"); n != 0 {
		t.Fatalf("rolled back write is visible (%d rows)", n)
	}
}

func TestPostgresTerminatedSessionIsNotReused(t *testing.T) {
	db := openFailoverTestDB(t)
	victim := backendPID(t, db)
	admin, err := sql.Open(DriverName, os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`SELECT pg_terminate_backend($1)`, victim); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The first statement may surface the termination; the pool must not
	// keep handing out the dead session after that.
	_, _ = db.ExecContext(ctx, `SELECT 1`)
	if _, err := db.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatalf("pool did not recover from a terminated session: %v", err)
	}
	if now := backendPID(t, db); now == victim {
		t.Fatal("terminated session is still in use")
	}
}
