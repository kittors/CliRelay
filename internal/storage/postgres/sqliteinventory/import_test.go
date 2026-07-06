package sqliteinventory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "modernc.org/sqlite"
)

func TestImportSQLiteDryRunAndApply(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	pgDB, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer pgDB.Close()
	if _, err := pgDB.Exec(`
		DROP TABLE IF EXISTS sqlite_import_runs;
		TRUNCATE
			request_log_content,
			request_logs,
			api_keys,
			api_key_permission_profiles
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatalf("truncate postgres: %v", err)
	}

	sqlitePath := filepath.Join(t.TempDir(), "usage.db")
	sqliteDB, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := sqliteDB.Exec(`
		CREATE TABLE api_key_permission_profiles (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			allowed_models TEXT NOT NULL DEFAULT '[]'
		);
		CREATE TABLE api_keys (
			key TEXT PRIMARY KEY,
			id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			allowed_models TEXT NOT NULL DEFAULT '[]'
		);
		CREATE TABLE request_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			api_key TEXT NOT NULL,
			api_key_id TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			failed INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE request_log_content (
			log_id INTEGER PRIMARY KEY,
			timestamp DATETIME NOT NULL,
			compression TEXT NOT NULL DEFAULT 'zstd',
			input_content BLOB NOT NULL DEFAULT X'',
			output_content BLOB NOT NULL DEFAULT X'',
			detail_content BLOB NOT NULL DEFAULT X''
		);
		INSERT INTO api_key_permission_profiles (id, name, allowed_models)
		VALUES ('profile-fixture', 'Fixture', '["gpt-test"]');
		INSERT INTO api_keys (key, id, name, allowed_models)
		VALUES ('fixture-key-a', 'key-a', 'Key A', '["gpt-test"]');
		INSERT INTO request_logs (id, timestamp, api_key, api_key_id, model, failed, total_tokens)
		VALUES (7, '2026-07-05T01:00:00Z', 'fixture-key-a', 'key-a', 'gpt-test', 0, 11);
		INSERT INTO request_log_content (log_id, timestamp, input_content, output_content, detail_content)
		VALUES (7, '2026-07-05T01:00:00Z', X'7B7D', X'7B226F6B223A747275657D', X'7B2264657461696C223A747275657D');
	`); err != nil {
		t.Fatalf("seed sqlite: %v", err)
	}
	if err := sqliteDB.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	opts := ImportOptions{
		SQLitePath:  sqlitePath,
		PostgresDSN: dsn,
		DryRun:      true,
		Now:         time.Date(2026, 7, 5, 4, 0, 0, 0, time.UTC),
	}
	dryRun, err := Import(ctx, opts)
	if err != nil {
		t.Fatalf("dry-run import: %v", err)
	}
	if !dryRun.DryRun || findImportTable(dryRun.Tables, "request_logs") == nil {
		t.Fatalf("dry-run report = %#v", dryRun)
	}
	if got := findImportTable(dryRun.Tables, "request_logs"); got.SourceRows != 1 || got.TargetRows != 0 || got.SourceChecksum == "" {
		t.Fatalf("request_logs dry-run = %#v", got)
	}

	opts.DryRun = false
	applied, err := Import(ctx, opts)
	if err != nil {
		t.Fatalf("apply import: %v", err)
	}
	if got := findImportTable(applied.Tables, "request_logs"); got == nil || got.InsertedRows != 1 || !got.SequenceReset {
		t.Fatalf("request_logs applied = %#v", got)
	}
	var count int
	if err := pgDB.QueryRow("SELECT COUNT(*) FROM request_logs WHERE id = 7 AND api_key = 'fixture-key-a'").Scan(&count); err != nil {
		t.Fatalf("count imported request log: %v", err)
	}
	if count != 1 {
		t.Fatalf("imported request log count = %d", count)
	}
	second, err := Import(ctx, opts)
	if err != nil {
		t.Fatalf("second apply import: %v", err)
	}
	if !second.Skipped {
		t.Fatalf("second apply should use completion marker, got %#v", second)
	}
}

func TestNormalizeImportValuesCoalescesLegacyNullContent(t *testing.T) {
	columns := []string{"log_id", "input_content", "output_content", "detail_content", "session_id"}
	values := []any{int64(7), nil, nil, nil, nil}

	normalizeImportValues("request_log_content", columns, values)

	for _, idx := range []int{1, 2, 3} {
		got, ok := values[idx].([]byte)
		if !ok || len(got) != 0 {
			t.Fatalf("values[%d] = %#v, want empty []byte", idx, values[idx])
		}
	}
	if values[4] != "" {
		t.Fatalf("session_id = %#v, want empty string", values[4])
	}
}

func TestImportSQLiteApplyUsesPostgresLockAndCompletionMarker(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	pgDB, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer pgDB.Close()
	if _, err := pgDB.Exec(`
		DROP TABLE IF EXISTS sqlite_import_runs;
		TRUNCATE
			request_log_content,
			request_logs,
			api_keys
		RESTART IDENTITY CASCADE
	`); err != nil {
		t.Fatalf("reset postgres: %v", err)
	}

	sqlitePath := filepath.Join(t.TempDir(), "usage.db")
	sqliteDB, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := sqliteDB.Exec(`
		CREATE TABLE api_keys (
			key TEXT PRIMARY KEY,
			id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			allowed_models TEXT NOT NULL DEFAULT '[]'
		);
		CREATE TABLE request_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			api_key TEXT NOT NULL,
			api_key_id TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			failed INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO api_keys (key, id, name, allowed_models)
		VALUES ('fixture-key-lock', 'key-lock', 'Key Lock', '["gpt-test"]');
		INSERT INTO request_logs (id, timestamp, api_key, api_key_id, model, failed, total_tokens)
		VALUES (11, '2026-07-05T02:00:00Z', 'fixture-key-lock', 'key-lock', 'gpt-test', 0, 22);
	`); err != nil {
		t.Fatalf("seed sqlite: %v", err)
	}
	if err := sqliteDB.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	opts := ImportOptions{
		SQLitePath:  sqlitePath,
		PostgresDSN: dsn,
		DryRun:      false,
		Now:         time.Date(2026, 7, 5, 5, 0, 0, 0, time.UTC),
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	reports := make(chan ImportReport, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report, err := Import(ctx, opts)
			if err != nil {
				errs <- err
				return
			}
			reports <- report
		}()
	}
	wg.Wait()
	close(errs)
	close(reports)
	for err := range errs {
		t.Fatalf("concurrent import: %v", err)
	}
	var applied, skipped int
	for report := range reports {
		if report.Skipped {
			skipped++
		} else {
			applied++
		}
	}
	if applied != 1 || skipped != 1 {
		t.Fatalf("applied=%d skipped=%d, want 1/1", applied, skipped)
	}
	var count int
	if err := pgDB.QueryRow("SELECT COUNT(*) FROM request_logs WHERE id = 11 AND api_key = 'fixture-key-lock'").Scan(&count); err != nil {
		t.Fatalf("count imported request log: %v", err)
	}
	if count != 1 {
		t.Fatalf("imported request log count = %d", count)
	}
	if err := pgDB.QueryRow("SELECT COUNT(*) FROM sqlite_import_runs").Scan(&count); err != nil {
		t.Fatalf("count import markers: %v", err)
	}
	if count != 1 {
		t.Fatalf("import marker count = %d", count)
	}
}

func findImportTable(rows []ImportTableReport, name string) *ImportTableReport {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}
