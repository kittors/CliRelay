package clusterstate

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

// openTestDB migrates a disposable database, so these tests neither see nor
// disturb rows written by other packages on the shared test server.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := openTestDBWithDSN(t)
	return db
}

// openTestDBWithDSN is openTestDB that also returns the DSN of the disposable
// database, for a bus listener that must LISTEN on that same database.
func openTestDBWithDSN(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	adminDB, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("clusterstate_%d", time.Now().UnixNano())
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create disposable db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			 WHERE datname = $1 AND pid <> pg_backend_pid()
		`, dbName)
		if _, err := adminDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Errorf("drop disposable db %s: %v", dbName, err)
		}
		_ = adminDB.Close()
	})
	testDSN, err := withDatabase(dsn, dbName)
	if err != nil {
		t.Fatal(err)
	}
	db, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: testDSN, MaxOpenConns: 8, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("open migrated runtime db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, testDSN
}

func withDatabase(dsn, dbName string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + dbName
		return u.String(), nil
	}
	parts := strings.Fields(dsn)
	out := make([]string, 0, len(parts)+1)
	for _, part := range parts {
		if !strings.HasPrefix(part, "dbname=") {
			out = append(out, part)
		}
	}
	return strings.Join(append(out, "dbname="+dbName), " "), nil
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
