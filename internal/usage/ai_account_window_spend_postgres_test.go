package usage

import (
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// Production runs on PostgreSQL: the spend reads go through the compat driver
// there, and the subject-first index they rely on comes from a runtime
// migration rather than the SQLite schema.
func TestPostgresQueryAIAccountWindowSpend(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, config.RequestLogStorageConfig{}, windowSpendTestZone); err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	stopRequestLogMaintenance()
	truncatePostgresRuntimeTables(t, getDB())
	t.Cleanup(func() { truncatePostgresRuntimeTables(t, getDB()) })

	var indexes int
	if err := getDB().QueryRow(`
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'usage_rollup_buckets' AND indexname = 'idx_usage_rollup_subject_kind_start'
	`).Scan(&indexes); err != nil {
		t.Fatalf("read rollup indexes: %v", err)
	}
	if indexes != 1 {
		t.Fatalf("subject-first rollup index after migration = %d, want 1", indexes)
	}
	assertWindowSpendAcrossTenantsAndBuckets(t)
}
