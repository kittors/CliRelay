package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

// LogRow represents a single request log entry returned by QueryLogs.
type LogRow struct {
	ID              int64     `json:"id"`
	Timestamp       time.Time `json:"timestamp"`
	APIKey          string    `json:"api_key"`
	APIKeyName      string    `json:"api_key_name"`
	Model           string    `json:"model"`
	Source          string    `json:"source"`
	ChannelName     string    `json:"channel_name"`
	AuthIndex       string    `json:"auth_index"`
	Failed          bool      `json:"failed"`
	LatencyMs       int64     `json:"latency_ms"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	CachedTokens    int64     `json:"cached_tokens"`
	TotalTokens     int64     `json:"total_tokens"`
}

// LogQueryParams holds filter/pagination parameters for QueryLogs.
type LogQueryParams struct {
	Page   int    // 1-based
	Size   int    // rows per page
	Days   int    // time range in days
	APIKey string // exact match filter
	Model  string // exact match filter
	Status string // "success", "failed", or "" (all)
}

// LogQueryResult holds the paginated query result.
type LogQueryResult struct {
	Items []LogRow `json:"items"`
	Total int64    `json:"total"`
	Page  int      `json:"page"`
	Size  int      `json:"size"`
}

// FilterOptions holds the available filter values for the UI.
type FilterOptions struct {
	APIKeys     []string          `json:"api_keys"`
	APIKeyNames map[string]string `json:"api_key_names"`
	Models      []string          `json:"models"`
}

// LogStats holds aggregated stats over the filtered result set.
type LogStats struct {
	Total       int64   `json:"total"`
	SuccessRate float64 `json:"success_rate"`
	TotalTokens int64   `json:"total_tokens"`
}

var (
	usageDB   *sql.DB
	usageDBMu sync.Mutex
)

const createTableSQL = `
CREATE TABLE IF NOT EXISTS request_logs (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  timestamp        DATETIME NOT NULL,
  api_key          TEXT NOT NULL DEFAULT '',
  model            TEXT NOT NULL DEFAULT '',
  source           TEXT NOT NULL DEFAULT '',
  channel_name     TEXT NOT NULL DEFAULT '',
  auth_index       TEXT NOT NULL DEFAULT '',
  failed           INTEGER NOT NULL DEFAULT 0,
  latency_ms       INTEGER NOT NULL DEFAULT 0,
  input_tokens     INTEGER NOT NULL DEFAULT 0,
  output_tokens    INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens    INTEGER NOT NULL DEFAULT 0,
  total_tokens     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON request_logs(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_logs_api_key ON request_logs(api_key);
CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model);
CREATE INDEX IF NOT EXISTS idx_logs_failed ON request_logs(failed);
`

// InitDB opens (or creates) the SQLite database at the given path and creates
// the request_logs table if it doesn't exist.
func InitDB(dbPath string) error {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()

	if usageDB != nil {
		return nil // already initialised
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("usage: open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1) // SQLite performs best with a single writer
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(createTableSQL); err != nil {
		_ = db.Close()
		return fmt.Errorf("usage: create table: %w", err)
	}

	usageDB = db
	log.Infof("usage: SQLite database initialised at %s", dbPath)
	return nil
}

// CloseDB closes the SQLite database gracefully.
func CloseDB() {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()

	if usageDB != nil {
		_ = usageDB.Close()
		usageDB = nil
		log.Info("usage: SQLite database closed")
	}
}

// InsertLog writes a single request log entry into the SQLite database.
// It is safe to call concurrently.
func InsertLog(apiKey, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs int64, tokens TokenStats) {

	db := getDB()
	if db == nil {
		return
	}

	failedInt := 0
	if failed {
		failedInt = 1
	}

	_, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.UTC().Format(time.RFC3339Nano),
		apiKey, model, source, channelName, authIndex,
		failedInt, latencyMs,
		tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens,
		tokens.CachedTokens, tokens.TotalTokens,
	)
	if err != nil {
		log.Errorf("usage: insert log: %v", err)
	}
}

// QueryLogs returns a paginated, filtered list of log entries.
func QueryLogs(params LogQueryParams) (LogQueryResult, error) {
	db := getDB()
	if db == nil {
		return LogQueryResult{Page: params.Page, Size: params.Size}, nil
	}

	// Normalise parameters
	if params.Page < 1 {
		params.Page = 1
	}
	if params.Size < 1 {
		params.Size = 50
	}
	if params.Size > 200 {
		params.Size = 200
	}
	if params.Days < 1 {
		params.Days = 7
	}

	where, args := buildWhereClause(params)

	// Count total
	var total int64
	countSQL := "SELECT COUNT(*) FROM request_logs" + where
	if err := db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return LogQueryResult{}, fmt.Errorf("usage: count query: %w", err)
	}

	// Fetch page
	offset := (params.Page - 1) * params.Size
	querySQL := "SELECT id, timestamp, api_key, model, source, channel_name, auth_index, " +
		"failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens " +
		"FROM request_logs" + where +
		" ORDER BY timestamp DESC LIMIT ? OFFSET ?"
	queryArgs := append(args, params.Size, offset)

	rows, err := db.Query(querySQL, queryArgs...)
	if err != nil {
		return LogQueryResult{}, fmt.Errorf("usage: query logs: %w", err)
	}
	defer rows.Close()

	items := make([]LogRow, 0, params.Size)
	for rows.Next() {
		var row LogRow
		var ts string
		var failedInt int
		if err := rows.Scan(
			&row.ID, &ts, &row.APIKey, &row.Model, &row.Source, &row.ChannelName,
			&row.AuthIndex, &failedInt, &row.LatencyMs,
			&row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
			&row.CachedTokens, &row.TotalTokens,
		); err != nil {
			return LogQueryResult{}, fmt.Errorf("usage: scan row: %w", err)
		}
		row.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		row.Failed = failedInt != 0
		items = append(items, row)
	}

	return LogQueryResult{
		Items: items,
		Total: total,
		Page:  params.Page,
		Size:  params.Size,
	}, nil
}

// QueryFilters returns the distinct API keys and models within the time range.
func QueryFilters(days int) (FilterOptions, error) {
	db := getDB()
	if db == nil {
		return FilterOptions{}, nil
	}
	if days < 1 {
		days = 7
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	keys, err := queryDistinct(db, "api_key", cutoff)
	if err != nil {
		return FilterOptions{}, err
	}
	models, err := queryDistinct(db, "model", cutoff)
	if err != nil {
		return FilterOptions{}, err
	}

	return FilterOptions{APIKeys: keys, Models: models}, nil
}

// QueryStats returns aggregated statistics over the filtered dataset.
func QueryStats(params LogQueryParams) (LogStats, error) {
	db := getDB()
	if db == nil {
		return LogStats{}, nil
	}
	if params.Days < 1 {
		params.Days = 7
	}

	where, args := buildWhereClause(params)

	var total, successCount, totalTokens int64
	statsSQL := "SELECT COUNT(*), COALESCE(SUM(CASE WHEN failed=0 THEN 1 ELSE 0 END),0), COALESCE(SUM(total_tokens),0) " +
		"FROM request_logs" + where
	if err := db.QueryRow(statsSQL, args...).Scan(&total, &successCount, &totalTokens); err != nil {
		return LogStats{}, fmt.Errorf("usage: stats query: %w", err)
	}

	var successRate float64
	if total > 0 {
		successRate = float64(successCount) / float64(total) * 100
	}

	return LogStats{
		Total:       total,
		SuccessRate: successRate,
		TotalTokens: totalTokens,
	}, nil
}

// MigrateFromSnapshot imports all request details from an existing
// StatisticsSnapshot into SQLite. It skips rows that already exist
// (based on a count check to avoid duplicating on restart).
func MigrateFromSnapshot(snapshot StatisticsSnapshot) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}

	// Check if data already exists
	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&count); err != nil {
		return 0, fmt.Errorf("usage: migration count: %w", err)
	}
	if count > 0 {
		log.Infof("usage: SQLite already has %d rows, skipping migration", count)
		return 0, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("usage: begin migration tx: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO request_logs
		(timestamp, api_key, model, source, channel_name, auth_index,
		 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("usage: prepare migration stmt: %w", err)
	}
	defer stmt.Close()

	var imported int64
	for apiKey, apiData := range snapshot.APIs {
		for model, modelData := range apiData.Models {
			for _, detail := range modelData.Details {
				failedInt := 0
				if detail.Failed {
					failedInt = 1
				}
				_, err := stmt.Exec(
					detail.Timestamp.UTC().Format(time.RFC3339Nano),
					apiKey, model, detail.Source, detail.ChannelName, detail.AuthIndex,
					failedInt, detail.LatencyMs,
					detail.Tokens.InputTokens, detail.Tokens.OutputTokens,
					detail.Tokens.ReasoningTokens, detail.Tokens.CachedTokens,
					detail.Tokens.TotalTokens,
				)
				if err != nil {
					_ = tx.Rollback()
					return imported, fmt.Errorf("usage: migration insert: %w", err)
				}
				imported++
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return imported, fmt.Errorf("usage: commit migration: %w", err)
	}

	log.Infof("usage: migrated %d request logs from snapshot to SQLite", imported)
	return imported, nil
}

// --- internal helpers ---

func getDB() *sql.DB {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	return usageDB
}

func buildWhereClause(params LogQueryParams) (string, []interface{}) {
	conditions := make([]string, 0, 4)
	args := make([]interface{}, 0, 4)

	// Time range
	cutoff := time.Now().UTC().AddDate(0, 0, -params.Days)
	// Set to start of day
	cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	conditions = append(conditions, "timestamp >= ?")
	args = append(args, cutoff.Format(time.RFC3339))

	if params.APIKey != "" {
		conditions = append(conditions, "api_key = ?")
		args = append(args, params.APIKey)
	}
	if params.Model != "" {
		conditions = append(conditions, "model = ?")
		args = append(args, params.Model)
	}
	if params.Status == "success" {
		conditions = append(conditions, "failed = 0")
	} else if params.Status == "failed" {
		conditions = append(conditions, "failed = 1")
	}

	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func queryDistinct(db *sql.DB, column, cutoff string) ([]string, error) {
	q := fmt.Sprintf("SELECT DISTINCT %s FROM request_logs WHERE timestamp >= ? ORDER BY %s", column, column)
	rows, err := db.Query(q, cutoff)
	if err != nil {
		return nil, fmt.Errorf("usage: distinct %s: %w", column, err)
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if v != "" {
			result = append(result, v)
		}
	}
	return result, nil
}
