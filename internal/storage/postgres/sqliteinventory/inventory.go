package sqliteinventory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Inventory struct {
	Path      string       `json:"path"`
	ScannedAt time.Time    `json:"scanned_at"`
	Tables    []TableStats `json:"tables"`
}

type TableStats struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	RowCount   int64    `json:"row_count"`
	MinID      *int64   `json:"min_id,omitempty"`
	MaxID      *int64   `json:"max_id,omitempty"`
	MinTime    string   `json:"min_time,omitempty"`
	MaxTime    string   `json:"max_time,omitempty"`
	Checksum   string   `json:"checksum"`
	DryRunOnly bool     `json:"dry_run_only"`
}

type Options struct {
	Path string
	Now  time.Time
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func WriteJSON(ctx context.Context, out io.Writer, opts Options) error {
	inventory, err := Collect(ctx, opts)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(inventory)
}

func Collect(ctx context.Context, opts Options) (Inventory, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return Inventory{}, fmt.Errorf("sqlite inventory: path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Inventory{}, fmt.Errorf("sqlite inventory: absolute path: %w", err)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	db, err := sql.Open("sqlite", "file:"+abs+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return Inventory{}, fmt.Errorf("sqlite inventory: open read-only: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return Inventory{}, fmt.Errorf("sqlite inventory: ping read-only: %w", err)
	}
	return collectFromQuerier(ctx, abs, now, db)
}

func collectFromQuerier(ctx context.Context, path string, now time.Time, q queryer) (Inventory, error) {
	tables, err := listTables(ctx, q)
	if err != nil {
		return Inventory{}, err
	}
	stats := make([]TableStats, 0, len(tables))
	for _, table := range tables {
		row, err := tableStats(ctx, q, table)
		if err != nil {
			return Inventory{}, err
		}
		stats = append(stats, row)
	}
	return Inventory{Path: path, ScannedAt: now.UTC(), Tables: stats}, nil
}

func listTables(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT name
		  FROM sqlite_master
		 WHERE type = 'table'
		   AND name NOT LIKE 'sqlite_%'
		 ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("sqlite inventory: list tables: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite inventory: scan table: %w", err)
		}
		if validIdentifier(name) {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite inventory: table rows: %w", err)
	}
	return tables, nil
}

func tableStats(ctx context.Context, q queryer, table string) (TableStats, error) {
	columns, err := tableColumns(ctx, q, table)
	if err != nil {
		return TableStats{}, err
	}
	stat := TableStats{
		Name:       table,
		Columns:    columns,
		DryRunOnly: true,
	}
	quotedTable := quoteIdent(table)
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quotedTable).Scan(&stat.RowCount); err != nil {
		return TableStats{}, fmt.Errorf("sqlite inventory: count %s: %w", table, err)
	}
	if hasColumn(columns, "id") && numericIDColumn(ctx, q, quotedTable) {
		var minID, maxID sql.NullInt64
		if err := q.QueryRowContext(ctx, "SELECT MIN(id), MAX(id) FROM "+quotedTable).Scan(&minID, &maxID); err != nil {
			return TableStats{}, fmt.Errorf("sqlite inventory: id range %s: %w", table, err)
		}
		if minID.Valid {
			stat.MinID = &minID.Int64
		}
		if maxID.Valid {
			stat.MaxID = &maxID.Int64
		}
	}
	timeColumn := firstColumn(columns, "timestamp", "recorded_at", "updated_at", "created_at")
	if timeColumn != "" {
		var minTime, maxTime sql.NullString
		query := fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s", quoteIdent(timeColumn), quoteIdent(timeColumn), quotedTable)
		if err := q.QueryRowContext(ctx, query).Scan(&minTime, &maxTime); err != nil {
			return TableStats{}, fmt.Errorf("sqlite inventory: time range %s: %w", table, err)
		}
		stat.MinTime = minTime.String
		stat.MaxTime = maxTime.String
	}
	checksum, err := tableChecksum(ctx, q, table, columns)
	if err != nil {
		return TableStats{}, err
	}
	stat.Checksum = checksum
	return stat, nil
}

func tableColumns(ctx context.Context, q queryer, table string) ([]string, error) {
	if !validIdentifier(table) {
		return nil, fmt.Errorf("sqlite inventory: invalid table name %q", table)
	}
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+quoteIdent(table)+")")
	if err != nil {
		return nil, fmt.Errorf("sqlite inventory: columns %s: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, fmt.Errorf("sqlite inventory: scan column %s: %w", table, err)
		}
		if validIdentifier(name) {
			columns = append(columns, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite inventory: column rows %s: %w", table, err)
	}
	sort.Strings(columns)
	return columns, nil
}

func tableChecksum(ctx context.Context, q queryer, table string, columns []string) (string, error) {
	if len(columns) == 0 {
		return "", nil
	}
	quotedColumns := make([]string, 0, len(columns))
	for _, column := range columns {
		quotedColumns = append(quotedColumns, quoteIdent(column))
	}
	query := fmt.Sprintf("SELECT %s FROM %s ORDER BY rowid", strings.Join(quotedColumns, ","), quoteIdent(table))
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return "", fmt.Errorf("sqlite inventory: checksum query %s: %w", table, err)
	}
	defer rows.Close()
	hash := sha256.New()
	values := make([]sql.NullString, len(columns))
	scans := make([]any, len(columns))
	for i := range values {
		scans[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(scans...); err != nil {
			return "", fmt.Errorf("sqlite inventory: checksum scan %s: %w", table, err)
		}
		for _, value := range values {
			if value.Valid {
				_, _ = hash.Write([]byte(value.String))
			}
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("sqlite inventory: checksum rows %s: %w", table, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func firstColumn(columns []string, names ...string) string {
	for _, name := range names {
		if hasColumn(columns, name) {
			return name
		}
	}
	return ""
}

func hasColumn(columns []string, name string) bool {
	for _, column := range columns {
		if column == name {
			return true
		}
	}
	return false
}

func numericIDColumn(ctx context.Context, q queryer, quotedTable string) bool {
	var typ sql.NullString
	query := "SELECT typeof(id) FROM " + quotedTable + " WHERE id IS NOT NULL LIMIT 1"
	if err := q.QueryRowContext(ctx, query).Scan(&typ); err != nil {
		return false
	}
	return typ.String == "integer"
}

func validIdentifier(value string) bool {
	return identifierRE.MatchString(value)
}

func quoteIdent(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
