package sqliteinventory

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "modernc.org/sqlite"
)

func TestImportSQLiteDryRunAndApply(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
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
			input_content BLOB,
			output_content BLOB,
			detail_content BLOB
		);
		INSERT INTO api_key_permission_profiles (id, name, allowed_models)
		VALUES ('profile-fixture', 'Fixture', '["gpt-test"]');
		INSERT INTO api_keys (key, id, name, allowed_models)
		VALUES ('fixture-key-a', 'key-a', 'Key A', '["gpt-test"]');
		INSERT INTO request_logs (id, timestamp, api_key, api_key_id, model, failed, total_tokens)
		VALUES
			(7, '2026-07-05T01:00:00Z', 'fixture-key-a', 'key-a', 'gpt-test', 0, 11),
			(8, '2026-07-05T01:01:00Z', 'fixture-key-a', 'key-a', 'gpt-test', 0, 12);
		INSERT INTO request_log_content (log_id, timestamp, input_content, output_content, detail_content)
		VALUES
			(7, '2026-07-05T01:00:00Z', X'7B7D', NULL, X'7B2264657461696C223A747275657D'),
			(8, '2026-07-05T01:01:00Z', X'7B7D', X'', X'7B2264657461696C223A747275657D');
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
	if got := findImportTable(dryRun.Tables, "request_logs"); got.SourceRows != 2 || got.TargetRows != 0 || got.SourceChecksum == "" {
		t.Fatalf("request_logs dry-run = %#v", got)
	}

	opts.DryRun = false
	applied, err := Import(ctx, opts)
	if err != nil {
		t.Fatalf("apply import: %v", err)
	}
	if got := findImportTable(applied.Tables, "request_logs"); got == nil || got.InsertedRows != 2 || !got.SequenceReset {
		t.Fatalf("request_logs applied = %#v", got)
	}
	var count int
	if err := pgDB.QueryRow("SELECT COUNT(*) FROM request_logs WHERE id = 7 AND api_key = 'fixture-key-a'").Scan(&count); err != nil {
		t.Fatalf("count imported request log: %v", err)
	}
	if count != 1 {
		t.Fatalf("imported request log count = %d", count)
	}
	rows, err := pgDB.Query("SELECT log_id, output_content FROM request_log_content WHERE log_id IN (7, 8) ORDER BY log_id")
	if err != nil {
		t.Fatalf("read imported request log content: %v", err)
	}
	defer rows.Close()
	var gotContentRows int
	for rows.Next() {
		var logID int64
		var outputContent []byte
		if err := rows.Scan(&logID, &outputContent); err != nil {
			t.Fatalf("scan imported request log content: %v", err)
		}
		if outputContent == nil || len(outputContent) != 0 {
			t.Fatalf("log_id %d output_content = %#v, want non-nil empty bytes", logID, outputContent)
		}
		gotContentRows++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate imported request log content: %v", err)
	}
	if gotContentRows != 2 {
		t.Fatalf("imported request log content rows = %d, want 2", gotContentRows)
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
	values := []any{int64(7), []byte(nil), nil, sql.RawBytes(nil), nil}

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
	postgrestest.LockSharedRuntimeDB(t, dsn)
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

func TestImportSQLiteUsesSingleSourceSnapshot(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
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
			request_logs
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
		PRAGMA journal_mode = WAL;
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
		INSERT INTO request_logs (id, timestamp, api_key, api_key_id, model, failed, total_tokens)
		VALUES (1, '2026-07-05T01:00:00Z', 'fixture-key-a', 'key-a', 'gpt-test', 0, 11);
		INSERT INTO request_log_content (log_id, timestamp, input_content, output_content, detail_content)
		VALUES (1, '2026-07-05T01:00:00Z', X'7B7D', X'7B226F6B223A747275657D', X'7B2264657461696C223A747275657D');
	`); err != nil {
		t.Fatalf("seed sqlite: %v", err)
	}
	if err := sqliteDB.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	src, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open source snapshot: %v", err)
	}
	defer src.Close()
	srcTx, err := src.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin source snapshot: %v", err)
	}
	defer func() { _ = srcTx.Rollback() }()
	inventory, err := collectFromQuerier(ctx, sqlitePath, time.Date(2026, 7, 5, 6, 0, 0, 0, time.UTC), srcTx)
	if err != nil {
		t.Fatalf("collect inventory: %v", err)
	}
	sourceColumns := make(map[string][]string, len(inventory.Tables))
	for _, table := range inventory.Tables {
		sourceColumns[table.Name] = table.Columns
	}
	var progress bytes.Buffer
	if _, err := importTable(ctx, srcTx, pgDB, "request_logs", sourceColumns["request_logs"], false, &progress); err != nil {
		t.Fatalf("import request_logs: %v", err)
	}
	if !strings.Contains(progress.String(), "sqlite import progress: table request_logs inserted_rows=1 target_rows=1") {
		t.Fatalf("progress log = %q, want request_logs row progress", progress.String())
	}

	writer, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite writer: %v", err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`
		INSERT INTO request_logs (id, timestamp, api_key, api_key_id, model, failed, total_tokens)
		VALUES (2, '2026-07-05T01:01:00Z', 'fixture-key-b', 'key-b', 'gpt-test', 0, 22);
		INSERT INTO request_log_content (log_id, timestamp, input_content, output_content, detail_content)
		VALUES (2, '2026-07-05T01:01:00Z', X'7B7D', X'7B7D', X'7B7D');
	`); err != nil {
		t.Fatalf("write concurrent sqlite rows: %v", err)
	}

	if _, err := importTable(ctx, srcTx, pgDB, "request_log_content", sourceColumns["request_log_content"], false, nil); err != nil {
		t.Fatalf("import request_log_content: %v", err)
	}
	var count int
	if err := pgDB.QueryRow("SELECT COUNT(*) FROM request_log_content").Scan(&count); err != nil {
		t.Fatalf("count imported content: %v", err)
	}
	if count != 1 {
		t.Fatalf("imported content rows = %d, want snapshot row only", count)
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
