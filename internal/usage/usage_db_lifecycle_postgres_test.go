package usage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// CI supplies the same disposable Postgres service used by the runtime-stack
// integration tests. No production database is needed for this regression.
func TestPostgresCloseDBDrainsUsageWithStoreMetadata(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)
	cfg := config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}
	loc := time.FixedZone("project-zone", 8*3600)
	open := func() {
		t.Helper()
		if err := InitPostgres(cfg, config.RequestLogStorageConfig{StoreContent: true}, loc); err != nil {
			t.Fatal(err)
		}
		stopRequestLogMaintenance()
	}
	open()
	truncatePostgresRuntimeTables(t, getDB())
	for i := 0; i < 5; i++ {
		coreusage.PublishRecord(context.Background(), coreusage.Record{Model: "lifecycle-pg", RequestedAt: time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC), InputContent: "preserve input", OutputContent: "preserve output", Detail: coreusage.Detail{InputTokens: 3, OutputTokens: 4, TotalTokens: 7}})
	}
	CloseDB()
	open()
	var rows, content int
	if err := getDB().QueryRow("SELECT COUNT(*) FROM request_logs WHERE model = ?", "lifecycle-pg").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := getDB().QueryRow("SELECT COUNT(*) FROM request_log_content").Scan(&content); err != nil {
		t.Fatal(err)
	}
	if rows != 5 || content != 5 {
		t.Fatalf("rows=%d content=%d want 5 each", rows, content)
	}
	var requests, tokens int64
	if err := getDB().QueryRow("SELECT COALESCE(SUM(request_count),0), COALESCE(SUM(total_tokens),0) FROM usage_rollup_buckets WHERE bucket_kind = ? AND bucket_start = ? AND model = ?", rollupBucketDay, "2026-01-02", "lifecycle-pg").Scan(&requests, &tokens); err != nil {
		t.Fatal(err)
	}
	if requests != 5 || tokens != 35 {
		t.Fatalf("project-zone rollup requests=%d tokens=%d", requests, tokens)
	}
	// Reopening must not replay records already drained by CloseDB.
	coreusage.WithDrainedDefault(func() {})
	if n := countRequestLogRows(t); n != 5 {
		t.Fatalf("reopen duplicated logs: %d", n)
	}
}
