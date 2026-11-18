package usage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v8/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/postgrestest"
)

func TestPostgresRequestLogHygieneDropsOnlyRedundantIndexesAndTunesAutovacuum(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{
		DSN:          dsn,
		MaxOpenConns: 4,
		MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatalf("OpenRuntimeDB() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_logs_api_key_id_chart_cover ON request_logs(api_key_id, timestamp DESC, model, failed, input_tokens, output_tokens, total_tokens, cost, cached_tokens)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_tenant_auth_subject_time ON request_logs(tenant_id, auth_subject_id, timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model)`,
	} {
		if _, err = db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("create redundant index: %v", err)
		}
	}

	postgresRequestLogHygieneDone.Store(false)
	if err = compactPostgresLogStorage(ctx, db); err != nil {
		t.Fatalf("compactPostgresLogStorage() error = %v", err)
	}

	for _, indexName := range redundantPostgresRequestLogIndexes {
		var exists bool
		if err = db.QueryRowContext(ctx, `SELECT to_regclass(?) IS NOT NULL`, "public."+indexName).Scan(&exists); err != nil {
			t.Fatalf("inspect %s: %v", indexName, err)
		}
		if exists {
			t.Fatalf("redundant index %s still exists", indexName)
		}
	}

	var joined string
	if err = db.QueryRowContext(ctx, `SELECT COALESCE(array_to_string(reloptions, ','), '') FROM pg_class WHERE oid = 'request_logs'::regclass`).Scan(&joined); err != nil {
		t.Fatalf("read request_logs reloptions: %v", err)
	}
	for _, option := range []string{
		"autovacuum_vacuum_scale_factor=0.02",
		"autovacuum_vacuum_threshold=1000",
		"autovacuum_analyze_scale_factor=0.01",
		"autovacuum_analyze_threshold=1000",
		"autovacuum_vacuum_insert_scale_factor=0.05",
		"autovacuum_vacuum_insert_threshold=1000",
	} {
		if !strings.Contains(joined, option) {
			t.Fatalf("request_logs reloptions %q missing %q", joined, option)
		}
	}
}
