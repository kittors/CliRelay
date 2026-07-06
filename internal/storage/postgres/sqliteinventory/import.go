package sqliteinventory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const importBatchSize = 500

type ImportOptions struct {
	SQLitePath  string
	PostgresDSN string
	DryRun      bool
	Now         time.Time
	Progress    io.Writer
}

type ImportReport struct {
	SQLitePath        string              `json:"sqlite_path"`
	ScannedAt         time.Time           `json:"scanned_at"`
	DryRun            bool                `json:"dry_run"`
	Skipped           bool                `json:"skipped,omitempty"`
	SourceFingerprint string              `json:"source_fingerprint"`
	Tables            []ImportTableReport `json:"tables"`
}

type ImportTableReport struct {
	Name           string   `json:"name"`
	Columns        []string `json:"columns"`
	SourceRows     int64    `json:"source_rows"`
	TargetRows     int64    `json:"target_rows"`
	PlannedInserts int64    `json:"planned_inserts"`
	InsertedRows   int64    `json:"inserted_rows,omitempty"`
	SourceChecksum string   `json:"source_checksum"`
	TargetChecksum string   `json:"target_checksum"`
	SequenceReset  bool     `json:"sequence_reset,omitempty"`
	DryRunOnly     bool     `json:"dry_run_only"`
}

var runtimeImportTables = []string{
	"api_key_permission_profiles",
	"api_keys",
	"model_pricing",
	"model_configs",
	"model_owner_presets",
	"auth_group_model_owner_mappings",
	"model_openrouter_sync_state",
	"proxy_pool",
	"routing_config",
	"runtime_settings",
	"identity_fingerprints",
	"auth_file_quota_snapshots",
	"auth_file_quota_snapshot_points",
	"auth_subject_quota_cycles",
	"ccswitch_import_configs",
	"request_logs",
	"request_log_content",
}

var sequenceTables = map[string]string{
	"request_logs":                    "id",
	"auth_file_quota_snapshot_points": "id",
}

func WriteImportJSON(ctx context.Context, out io.Writer, opts ImportOptions) error {
	report, err := Import(ctx, opts)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func Import(ctx context.Context, opts ImportOptions) (ImportReport, error) {
	sqlitePath := strings.TrimSpace(opts.SQLitePath)
	if sqlitePath == "" {
		return ImportReport{}, fmt.Errorf("sqlite import: sqlite path is required")
	}
	postgresDSN := strings.TrimSpace(opts.PostgresDSN)
	if postgresDSN == "" {
		return ImportReport{}, fmt.Errorf("sqlite import: postgres dsn is required")
	}
	inventory, err := Collect(ctx, Options{Path: sqlitePath, Now: opts.Now})
	if err != nil {
		return ImportReport{}, err
	}
	sourceFingerprint := importSourceFingerprint(inventory)
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	src, err := sql.Open("sqlite", "file:"+inventory.Path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return ImportReport{}, fmt.Errorf("sqlite import: open sqlite read-only: %w", err)
	}
	defer src.Close()
	dst, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: postgresDSN, MaxOpenConns: 4, MaxIdleConns: 1})
	if err != nil {
		return ImportReport{}, err
	}
	defer dst.Close()
	if !opts.DryRun {
		unlock, err := lockImport(ctx, dst)
		if err != nil {
			return ImportReport{}, err
		}
		defer unlock()
		if err := ensureImportRunsTable(ctx, dst); err != nil {
			return ImportReport{}, err
		}
		completed, err := importCompleted(ctx, dst, sourceFingerprint)
		if err != nil {
			return ImportReport{}, err
		}
		if completed {
			return ImportReport{
				SQLitePath:        inventory.Path,
				ScannedAt:         now.UTC(),
				DryRun:            opts.DryRun,
				Skipped:           true,
				SourceFingerprint: sourceFingerprint,
			}, nil
		}
	}

	sourceColumns := make(map[string][]string, len(inventory.Tables))
	for _, table := range inventory.Tables {
		sourceColumns[table.Name] = table.Columns
	}
	report := ImportReport{SQLitePath: inventory.Path, ScannedAt: now.UTC(), DryRun: opts.DryRun, SourceFingerprint: sourceFingerprint}
	for i, table := range runtimeImportTables {
		srcCols, ok := sourceColumns[table]
		if !ok {
			continue
		}
		reportImportProgress(opts.Progress, "sqlite import progress: table %d/%d %s", i+1, len(runtimeImportTables), table)
		row, err := importTable(ctx, src, dst, table, srcCols, opts.DryRun)
		if err != nil {
			return ImportReport{}, err
		}
		if opts.DryRun {
			reportImportProgress(opts.Progress, "sqlite import progress: table %s dry-run source_rows=%d target_rows=%d planned_inserts=%d", table, row.SourceRows, row.TargetRows, row.PlannedInserts)
		} else {
			reportImportProgress(opts.Progress, "sqlite import progress: table %s inserted_rows=%d target_rows=%d", table, row.InsertedRows, row.TargetRows)
		}
		report.Tables = append(report.Tables, row)
	}
	if !opts.DryRun {
		if err := markImportCompleted(ctx, dst, sourceFingerprint, inventory.Path, report); err != nil {
			return ImportReport{}, err
		}
	}
	return report, nil
}

func importSourceFingerprint(inventory Inventory) string {
	tables := make(map[string]TableStats, len(inventory.Tables))
	for _, table := range inventory.Tables {
		tables[table.Name] = table
	}
	hash := sha256.New()
	for _, name := range runtimeImportTables {
		table, ok := tables[name]
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(hash, "%s\t%d\t%s\n", table.Name, table.RowCount, table.Checksum)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func lockImport(ctx context.Context, db *sql.DB) (func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite import: reserve postgres lock connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext('clirelay_sqlite_import')::bigint)`); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite import: acquire postgres advisory lock: %w", err)
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext('clirelay_sqlite_import')::bigint)`)
		_ = conn.Close()
	}, nil
}

func ensureImportRunsTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS sqlite_import_runs (
			source_fingerprint TEXT PRIMARY KEY,
			sqlite_path TEXT NOT NULL,
			started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			report JSONB NOT NULL DEFAULT '{}'::jsonb
		)
	`); err != nil {
		return fmt.Errorf("sqlite import: create import runs table: %w", err)
	}
	return nil
}

func importCompleted(ctx context.Context, db *sql.DB, sourceFingerprint string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM sqlite_import_runs
		 WHERE source_fingerprint = ?
	`, sourceFingerprint).Scan(&count); err != nil {
		return false, fmt.Errorf("sqlite import: read import completion marker: %w", err)
	}
	return count > 0, nil
}

func markImportCompleted(ctx context.Context, db *sql.DB, sourceFingerprint, sqlitePath string, report ImportReport) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("sqlite import: marshal import report: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sqlite_import_runs (source_fingerprint, sqlite_path, started_at, completed_at, report)
		VALUES (?, ?, now(), now(), ?::jsonb)
		ON CONFLICT (source_fingerprint) DO UPDATE
		   SET sqlite_path = EXCLUDED.sqlite_path,
		       completed_at = now(),
		       report = EXCLUDED.report
	`, sourceFingerprint, sqlitePath, string(payload)); err != nil {
		return fmt.Errorf("sqlite import: mark import completed: %w", err)
	}
	return nil
}

func importTable(ctx context.Context, src, dst *sql.DB, table string, srcCols []string, dryRun bool) (ImportTableReport, error) {
	dstCols, err := postgresColumns(ctx, dst, table)
	if err != nil {
		return ImportTableReport{}, err
	}
	columns := intersectColumns(srcCols, dstCols)
	if len(columns) == 0 {
		return ImportTableReport{}, fmt.Errorf("sqlite import: no shared columns for %s", table)
	}
	orderBy := importOrderBy(table, columns)
	sourceRows, err := countRows(ctx, src, table)
	if err != nil {
		return ImportTableReport{}, err
	}
	targetRows, err := countRows(ctx, dst, table)
	if err != nil {
		return ImportTableReport{}, err
	}
	sourceChecksum, err := checksumRows(ctx, src, table, columns, orderBy)
	if err != nil {
		return ImportTableReport{}, err
	}
	targetChecksum, err := checksumRows(ctx, dst, table, columns, orderBy)
	if err != nil {
		return ImportTableReport{}, err
	}
	row := ImportTableReport{
		Name:           table,
		Columns:        columns,
		SourceRows:     sourceRows,
		TargetRows:     targetRows,
		PlannedInserts: sourceRows,
		SourceChecksum: sourceChecksum,
		TargetChecksum: targetChecksum,
		DryRunOnly:     dryRun,
	}
	if dryRun || sourceRows == 0 {
		return row, nil
	}
	inserted, err := copyRows(ctx, src, dst, table, columns, orderBy)
	if err != nil {
		return ImportTableReport{}, err
	}
	row.InsertedRows = inserted
	if column, ok := sequenceTables[table]; ok {
		if err := resetPostgresSequence(ctx, dst, table, column); err != nil {
			return ImportTableReport{}, err
		}
		row.SequenceReset = true
	}
	targetRows, err = countRows(ctx, dst, table)
	if err != nil {
		return ImportTableReport{}, err
	}
	targetChecksum, err = checksumRows(ctx, dst, table, columns, orderBy)
	if err != nil {
		return ImportTableReport{}, err
	}
	row.TargetRows = targetRows
	row.TargetChecksum = targetChecksum
	return row, nil
}

func postgresColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	if !validIdentifier(table) {
		return nil, fmt.Errorf("sqlite import: invalid table name %q", table)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT column_name
		  FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name = ?
		 ORDER BY ordinal_position
	`, table)
	if err != nil {
		return nil, fmt.Errorf("sqlite import: postgres columns %s: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, fmt.Errorf("sqlite import: scan postgres column %s: %w", table, err)
		}
		if validIdentifier(column) {
			columns = append(columns, column)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite import: postgres column rows %s: %w", table, err)
	}
	return columns, nil
}

func intersectColumns(source, target []string) []string {
	targetSet := make(map[string]struct{}, len(target))
	for _, column := range target {
		targetSet[column] = struct{}{}
	}
	var columns []string
	for _, column := range source {
		if _, ok := targetSet[column]; ok {
			columns = append(columns, column)
		}
	}
	sort.Strings(columns)
	return columns
}

func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(table)).Scan(&count); err != nil {
		return 0, fmt.Errorf("sqlite import: count %s: %w", table, err)
	}
	return count, nil
}

func checksumRows(ctx context.Context, db *sql.DB, table string, columns []string, orderBy string) (string, error) {
	query := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", quotedList(columns), quoteIdent(table), orderBy)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return "", fmt.Errorf("sqlite import: checksum query %s: %w", table, err)
	}
	defer rows.Close()
	hash := sha256.New()
	values := make([]any, len(columns))
	scans := make([]any, len(columns))
	for i := range values {
		scans[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(scans...); err != nil {
			return "", fmt.Errorf("sqlite import: checksum scan %s: %w", table, err)
		}
		for _, value := range values {
			_, _ = hash.Write([]byte(normalizeChecksumValue(value)))
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("sqlite import: checksum rows %s: %w", table, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyRows(ctx context.Context, src, dst *sql.DB, table string, columns []string, orderBy string) (int64, error) {
	query := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", quotedList(columns), quoteIdent(table), orderBy)
	rows, err := src.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("sqlite import: read %s: %w", table, err)
	}
	defer rows.Close()
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING", quoteIdent(table), quotedList(columns), placeholders(len(columns)))
	values := make([]any, len(columns))
	scans := make([]any, len(columns))
	for i := range values {
		scans[i] = &values[i]
	}
	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite import: begin %s: %w", table, err)
	}
	var inserted int64
	var batch int
	commit := func() error {
		if tx == nil {
			return nil
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		tx = nil
		return nil
	}
	for rows.Next() {
		if err := rows.Scan(scans...); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("sqlite import: scan %s: %w", table, err)
		}
		normalizeImportValues(table, columns, values)
		result, err := tx.ExecContext(ctx, insertSQL, values...)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("sqlite import: insert %s: %w", table, err)
		}
		if n, err := result.RowsAffected(); err == nil {
			inserted += n
		}
		batch++
		if batch >= importBatchSize {
			if err := commit(); err != nil {
				return 0, fmt.Errorf("sqlite import: commit %s: %w", table, err)
			}
			tx, err = dst.BeginTx(ctx, nil)
			if err != nil {
				return 0, fmt.Errorf("sqlite import: begin %s: %w", table, err)
			}
			batch = 0
		}
	}
	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("sqlite import: rows %s: %w", table, err)
	}
	if err := commit(); err != nil {
		return 0, fmt.Errorf("sqlite import: commit %s: %w", table, err)
	}
	return inserted, nil
}

var importNullDefaults = map[string]map[string]any{
	"request_logs": {
		"input_content":  "",
		"output_content": "",
	},
	"request_log_content": {
		"input_content":  []byte{},
		"output_content": []byte{},
		"detail_content": []byte{},
		"session_id":     "",
	},
}

func normalizeImportValues(table string, columns []string, values []any) {
	defaults := importNullDefaults[table]
	if len(defaults) == 0 {
		return
	}
	for i, column := range columns {
		if values[i] == nil {
			if value, ok := defaults[column]; ok {
				values[i] = value
			}
		}
	}
}

func reportImportProgress(out io.Writer, format string, args ...any) {
	if out == nil {
		return
	}
	_, _ = fmt.Fprintf(out, format+"\n", args...)
}

func resetPostgresSequence(ctx context.Context, db *sql.DB, table, column string) error {
	query := fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE((SELECT MAX(%s) FROM %s), 1), true)", table, column, quoteIdent(column), quoteIdent(table))
	if _, err := db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("sqlite import: reset sequence %s.%s: %w", table, column, err)
	}
	return nil
}

func importOrderBy(table string, columns []string) string {
	for _, column := range []string{"id", "log_id", "key", "model_id", "value", "setting_key", "route_path"} {
		if hasColumn(columns, column) {
			return quoteIdent(column)
		}
	}
	if table == "identity_fingerprints" && hasColumn(columns, "provider") && hasColumn(columns, "account_key") {
		return quoteIdent("provider") + ", " + quoteIdent("account_key")
	}
	if table == "auth_file_quota_snapshots" && hasColumn(columns, "date_key") {
		return quoteIdent("date_key") + ", " + quoteIdent("auth_index") + ", " + quoteIdent("quota_key")
	}
	if len(columns) == 0 {
		return "1"
	}
	return quotedList(columns)
}

func quotedList(columns []string) string {
	quoted := make([]string, 0, len(columns))
	for _, column := range columns {
		quoted = append(quoted, quoteIdent(column))
	}
	return strings.Join(quoted, ", ")
}

func placeholders(count int) string {
	values := make([]string, count)
	for i := range values {
		values[i] = "?"
	}
	return strings.Join(values, ", ")
}

func normalizeChecksumValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return hex.EncodeToString(v)
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(v)
	}
}
