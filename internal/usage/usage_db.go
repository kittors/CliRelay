package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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
	Cost            float64   `json:"cost"`
	HasContent      bool      `json:"has_content"`
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
	TotalCost   float64 `json:"total_cost"`
}

var (
	usageDB     *sql.DB
	usageDBMu   sync.Mutex
	usageDBPath string
	usageLoc    *time.Location
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
  total_tokens     INTEGER NOT NULL DEFAULT 0,
  input_content    TEXT NOT NULL DEFAULT '',
  output_content   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS request_log_content (
  log_id           INTEGER PRIMARY KEY,
  timestamp        DATETIME NOT NULL,
  compression      TEXT NOT NULL DEFAULT 'zstd',
  input_content    BLOB NOT NULL DEFAULT X'',
  output_content   BLOB NOT NULL DEFAULT X'',
  FOREIGN KEY(log_id) REFERENCES request_logs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON request_logs(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_logs_api_key ON request_logs(api_key);
CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model);
CREATE INDEX IF NOT EXISTS idx_logs_failed ON request_logs(failed);
CREATE INDEX IF NOT EXISTS idx_log_content_timestamp ON request_log_content(timestamp DESC);
`

// migrateContentColumns adds input_content/output_content columns to an
// existing request_logs table that was created before this feature.
func migrateContentColumns(db *sql.DB) {
	for _, col := range []string{"input_content", "output_content"} {
		_, err := db.Exec(fmt.Sprintf("ALTER TABLE request_logs ADD COLUMN %s TEXT NOT NULL DEFAULT ''", col))
		if err != nil {
			// "duplicate column name" is expected when already migrated
			if !strings.Contains(err.Error(), "duplicate") {
				log.Warnf("usage: migrate column %s: %v", col, err)
			}
		}
	}
}

// migrateCostColumn adds cost column to an existing request_logs table.
func migrateCostColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN cost REAL NOT NULL DEFAULT 0")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column cost: %v", err)
		}
	}
}

// InitDB opens (or creates) the SQLite database at the given path and creates
// the request_logs table if it doesn't exist.
func InitDB(dbPath string, storageCfg config.RequestLogStorageConfig, loc *time.Location) error {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()

	if usageDB != nil {
		return nil // already initialised
	}

	if loc == nil {
		loc = time.Local
	}
	usageLoc = loc

	log.Debugf("usage: opening SQLite database at %s", dbPath)
	// NOTE: Do NOT use _journal_mode or _busy_timeout in the connection string.
	// Those are mattn/go-sqlite3 (CGO) conventions. modernc.org/sqlite ignores them,
	// causing data to stay in-memory without flushing to disk.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("usage: open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1) // SQLite performs best with a single writer
	db.SetMaxIdleConns(1)

	// Verify connectivity with a timeout to avoid hanging on WAL recovery
	log.Debugf("usage: pinging database to verify connectivity")
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return fmt.Errorf("usage: ping sqlite: %w", err)
	}

	// Set PRAGMAs explicitly via Exec because modernc.org/sqlite does NOT
	// support the _pragma=value connection-string syntax used by mattn/go-sqlite3.
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return fmt.Errorf("usage: set busy_timeout: %w", err)
	}
	if res, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		log.Warnf("usage: failed to enable WAL journal mode: %v (data may not persist correctly)", err)
	} else {
		log.Debugf("usage: journal_mode set (result: %v)", res)
	}

	log.Debugf("usage: creating tables")
	if _, err := db.Exec(createTableSQL); err != nil {
		_ = db.Close()
		return fmt.Errorf("usage: create table: %w", err)
	}

	usageDB = db
	usageDBPath = dbPath
	requestLogStorage = normalizeRequestLogStorageConfig(storageCfg)
	log.Debugf("usage: running content column migration")
	migrateContentColumns(db)
	log.Debugf("usage: running cost column migration")
	migrateCostColumn(db)
	log.Debugf("usage: initializing pricing table")
	initPricingTable(db)
	log.Debugf("usage: initializing api_keys table")
	initAPIKeysTable(db)
	startRequestLogMaintenance(db)
	log.Infof("usage: SQLite database initialised at %s", dbPath)
	return nil
}

// CloseDB closes the SQLite database gracefully.
func CloseDB() {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()

	stopRequestLogMaintenance()
	if usageDB != nil {
		_ = usageDB.Close()
		usageDB = nil
		usageLoc = nil
		log.Info("usage: SQLite database closed")
	}
}

// InsertLog writes a single request log entry into the SQLite database.
// It is safe to call concurrently.
func InsertLog(apiKey, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs int64, tokens TokenStats,
	inputContent, outputContent string) {

	db := getDB()
	if db == nil {
		return
	}

	failedInt := 0
	if failed {
		failedInt = 1
	}

	// Calculate cost based on model pricing
	cost := CalculateCost(model, tokens.InputTokens, tokens.OutputTokens, tokens.CachedTokens)

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		log.Errorf("usage: begin insert tx: %v", err)
		return
	}

	result, err := tx.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.UTC().Format(time.RFC3339Nano),
		apiKey, model, source, channelName, authIndex,
		failedInt, latencyMs,
		tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens,
		tokens.CachedTokens, tokens.TotalTokens, cost,
	)
	if err != nil {
		_ = tx.Rollback()
		log.Errorf("usage: insert log: %v", err)
		return
	}

	if requestLogStorage.StoreContent && (inputContent != "" || outputContent != "") {
		logID, errLastID := result.LastInsertId()
		if errLastID != nil {
			_ = tx.Rollback()
			log.Errorf("usage: resolve inserted log id: %v", errLastID)
			return
		}
		if errStore := insertLogContentTx(tx, logID, timestamp, inputContent, outputContent); errStore != nil {
			_ = tx.Rollback()
			log.Errorf("usage: insert log content: %v", errStore)
			return
		}
	}

	if errCommit := tx.Commit(); errCommit != nil {
		log.Errorf("usage: commit log insert: %v", errCommit)
		return
	}

	// Notify TPM tracker about token usage
	if tokenUsageCallback != nil && tokens.TotalTokens > 0 {
		tokenUsageCallback(apiKey, tokens.TotalTokens)
	}
}

// tokenUsageCallback is set by SetTokenUsageCallback to notify external
// rate limiters (e.g. quota middleware) of token consumption.
var tokenUsageCallback func(apiKey string, totalTokens int64)

// SetTokenUsageCallback registers a function to be called after each
// request's tokens are recorded. Used by the quota middleware for TPM tracking.
func SetTokenUsageCallback(fn func(apiKey string, totalTokens int64)) {
	tokenUsageCallback = fn
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
		"failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, " +
		"cost, " +
		"(CASE WHEN EXISTS (SELECT 1 FROM request_log_content content WHERE content.log_id = request_logs.id) " +
		"OR length(input_content) > 0 OR length(output_content) > 0 THEN 1 ELSE 0 END) as has_content " +
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
		var failedInt, hasContentInt int
		if err := rows.Scan(
			&row.ID, &ts, &row.APIKey, &row.Model, &row.Source, &row.ChannelName,
			&row.AuthIndex, &failedInt, &row.LatencyMs,
			&row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
			&row.CachedTokens, &row.TotalTokens, &row.Cost, &hasContentInt,
		); err != nil {
			return LogQueryResult{}, fmt.Errorf("usage: scan row: %w", err)
		}
		row.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		row.Failed = failedInt != 0
		row.HasContent = hasContentInt != 0
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

	cutoff := CutoffStartUTC(days).Format(time.RFC3339)

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
	var totalCost float64
	statsSQL := "SELECT COUNT(*), COALESCE(SUM(CASE WHEN failed=0 THEN 1 ELSE 0 END),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost),0) " +
		"FROM request_logs" + where
	if err := db.QueryRow(statsSQL, args...).Scan(&total, &successCount, &totalTokens, &totalCost); err != nil {
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
		TotalCost:   totalCost,
	}, nil
}

// DashboardKPI holds the aggregated KPI data needed by the dashboard page.
type DashboardKPI struct {
	TotalRequests   int64   `json:"total_requests"`
	SuccessRequests int64   `json:"success_requests"`
	FailedRequests  int64   `json:"failed_requests"`
	SuccessRate     float64 `json:"success_rate"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
}

// QueryDashboardKPI returns aggregated KPI data from SQLite for the dashboard.
// This replaces the old in-memory snapshot-based counting which lost data on restart.
func QueryDashboardKPI(days int) (DashboardKPI, error) {
	db := getDB()
	if db == nil {
		return DashboardKPI{}, nil
	}
	if days < 1 {
		days = 7
	}

	cutoff := CutoffStartUTC(days).Format(time.RFC3339)

	var kpi DashboardKPI
	err := db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN failed=0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN failed=1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(total_tokens), 0)
		FROM request_logs
		WHERE timestamp >= ?
	`, cutoff).Scan(
		&kpi.TotalRequests,
		&kpi.SuccessRequests,
		&kpi.FailedRequests,
		&kpi.InputTokens,
		&kpi.OutputTokens,
		&kpi.ReasoningTokens,
		&kpi.CachedTokens,
		&kpi.TotalTokens,
	)
	if err != nil {
		return DashboardKPI{}, fmt.Errorf("usage: dashboard KPI query: %w", err)
	}

	if kpi.TotalRequests > 0 {
		kpi.SuccessRate = float64(kpi.SuccessRequests) / float64(kpi.TotalRequests) * 100
	}

	return kpi, nil
}

// MigrateFromSnapshot imports all request details from an existing
// MigrateFromSnapshot is retained for API compatibility but no longer
// migrates individual request details as they are no longer stored in memory.
func MigrateFromSnapshot(snapshot StatisticsSnapshot) (int64, error) {
	// Re-enable this to logic to parse aggregates if needed.
	// We no longer migrate Details since we no longer keep track of them in memory
	// and they are persisted real-time.
	return 0, nil
}

// --- internal helpers ---

func getDB() *sql.DB {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	return usageDB
}

func getUsageLocation() *time.Location {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	if usageLoc == nil {
		return time.Local
	}
	return usageLoc
}

func cutoffStartUTCAt(now time.Time, days int) time.Time {
	if days < 1 {
		days = 7
	}
	loc := getUsageLocation()
	now = now.In(loc)
	todayStartLocal := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return todayStartLocal.AddDate(0, 0, -(days - 1)).UTC()
}

// CutoffStartUTC returns the start-of-day cutoff for the given number of days
// in the project-configured timezone, converted to UTC. Exported so that
// dashboard and other callers can reuse the same time-range semantics.
func CutoffStartUTC(days int) time.Time {
	return cutoffStartUTCAt(time.Now(), days)
}

func buildWhereClause(params LogQueryParams) (string, []interface{}) {
	conditions := make([]string, 0, 4)
	args := make([]interface{}, 0, 4)

	// Time range: days=1 means "today", days=7 means "last 7 days", etc.
	conditions = append(conditions, "timestamp >= ?")
	args = append(args, CutoffStartUTC(params.Days).Format(time.RFC3339))

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

// QueryModelsForKey returns the distinct models used by a specific API key within the time range.
func QueryModelsForKey(apiKey string, days int) ([]string, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}
	cutoff := CutoffStartUTC(days).Format(time.RFC3339)

	rows, err := db.Query(
		"SELECT DISTINCT model FROM request_logs WHERE api_key = ? AND timestamp >= ? AND model != '' ORDER BY model",
		apiKey, cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("usage: distinct models for key: %w", err)
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}

// LogContentResult holds the content detail for a single log entry.
type LogContentResult struct {
	ID            int64  `json:"id"`
	InputContent  string `json:"input_content"`
	OutputContent string `json:"output_content"`
	Model         string `json:"model"`
}

// QueryLogContent retrieves the stored request/response content for a single log entry.
func QueryLogContent(id int64) (LogContentResult, error) {
	db := getDB()
	if db == nil {
		return LogContentResult{}, fmt.Errorf("usage: database not initialised")
	}

	result, err := queryCompressedLogContent(
		db,
		`SELECT logs.id, logs.model, content.compression, content.input_content, content.output_content
		 FROM request_logs logs
		 JOIN request_log_content content ON content.log_id = logs.id
		 WHERE logs.id = ?`,
		id,
	)
	if err == nil {
		return result, nil
	}

	var fallback LogContentResult
	err = db.QueryRow(
		"SELECT id, model, input_content, output_content FROM request_logs WHERE id = ?", id,
	).Scan(&fallback.ID, &fallback.Model, &fallback.InputContent, &fallback.OutputContent)
	if err != nil {
		return LogContentResult{}, fmt.Errorf("usage: query log content: %w", err)
	}
	return fallback, nil
}

// QueryLogContentForKey retrieves log content for a single entry, but only if it belongs to the given API key.
// This is used by the public endpoint to ensure users can only access their own logs.
func QueryLogContentForKey(id int64, apiKey string) (LogContentResult, error) {
	db := getDB()
	if db == nil {
		return LogContentResult{}, fmt.Errorf("usage: database not initialised")
	}

	result, err := queryCompressedLogContent(
		db,
		`SELECT logs.id, logs.model, content.compression, content.input_content, content.output_content
		 FROM request_logs logs
		 JOIN request_log_content content ON content.log_id = logs.id
		 WHERE logs.id = ? AND logs.api_key = ?`,
		id, apiKey,
	)
	if err == nil {
		return result, nil
	}

	var fallback LogContentResult
	err = db.QueryRow(
		"SELECT id, model, input_content, output_content FROM request_logs WHERE id = ? AND api_key = ?", id, apiKey,
	).Scan(&fallback.ID, &fallback.Model, &fallback.InputContent, &fallback.OutputContent)
	if err != nil {
		return LogContentResult{}, fmt.Errorf("usage: query log content: %w", err)
	}
	return fallback, nil
}

// DailySeriesPoint holds one day of aggregated usage data.
type DailySeriesPoint struct {
	Date         string `json:"date"`
	Requests     int    `json:"requests"`
	FailedReq    int    `json:"failed_requests"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// ModelDistributionPoint holds aggregated usage data for a single model.
type ModelDistributionPoint struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// QueryDailySeries returns per-day aggregated request count and token usage for a given API key.
func QueryDailySeries(apiKey string, days int) ([]DailySeriesPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	params := LogQueryParams{APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)

	// NOTE: timestamps are stored as UTC RFC3339 strings; localtime converts them to the process timezone
	// (configured via TZ/time.Local) for correct day bucketing.
	q := `SELECT date(timestamp, 'localtime') as d,
	             COUNT(*) as reqs,
	             SUM(CASE WHEN failed = 1 OR failed = 'true' THEN 1 ELSE 0 END) as failed_reqs,
	             COALESCE(SUM(input_tokens),0),
	             COALESCE(SUM(output_tokens),0)
	      FROM request_logs` + where + `
	      GROUP BY d ORDER BY d`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: daily series query: %w", err)
	}
	defer rows.Close()

	var result []DailySeriesPoint
	for rows.Next() {
		var p DailySeriesPoint
		if err := rows.Scan(&p.Date, &p.Requests, &p.FailedReq, &p.InputTokens, &p.OutputTokens); err != nil {
			return nil, fmt.Errorf("usage: daily series scan: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// QueryModelDistribution returns request count and token usage grouped by model for a given API key.
func QueryModelDistribution(apiKey string, days int) ([]ModelDistributionPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	params := LogQueryParams{APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)

	q := `SELECT model,
	             COUNT(*) as reqs,
	             COALESCE(SUM(total_tokens),0)
	      FROM request_logs` + where + `
	      GROUP BY model ORDER BY reqs DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: model distribution query: %w", err)
	}
	defer rows.Close()

	var result []ModelDistributionPoint
	for rows.Next() {
		var p ModelDistributionPoint
		if err := rows.Scan(&p.Model, &p.Requests, &p.Tokens); err != nil {
			return nil, fmt.Errorf("usage: model distribution scan: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// GetDBPath returns the file path of the SQLite database, or empty if not initialised.
func GetDBPath() string {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	return usageDBPath
}

// HourlyTokenPoint holds token usage per hour for the last N hours.
type HourlyTokenPoint struct {
	Hour            string `json:"hour"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
}

// HourlyModelPoint holds model request counts per hour.
type HourlyModelPoint struct {
	Hour     string `json:"hour"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

// QueryHourlySeries returns per-hour token and model aggregates for the last N hours.
func QueryHourlySeries(apiKey string, hours int) ([]HourlyTokenPoint, []HourlyModelPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil, nil
	}
	if hours < 1 {
		hours = 24
	}

	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)
	params := LogQueryParams{APIKey: apiKey, Days: 1} // dummy Days, not used since we override 'where'
	where, args := buildWhereClause(params)
	// Replace timestamp >= CutoffStartUTC with our hourly cutoff
	where = strings.Replace(where, CutoffStartUTC(params.Days).Format(time.RFC3339), cutoff, 1)

	// query tokens by hour
	tokenQuery := `SELECT strftime('%Y-%m-%d %H:00', timestamp, 'localtime') as h,
	                      COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
	                      COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0), COALESCE(SUM(total_tokens),0)
	               FROM request_logs` + where + ` GROUP BY h ORDER BY h`
	tokenRows, err := db.Query(tokenQuery, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: hourly token query: %w", err)
	}
	defer tokenRows.Close()

	var tokens []HourlyTokenPoint
	for tokenRows.Next() {
		var p HourlyTokenPoint
		if err := tokenRows.Scan(&p.Hour, &p.InputTokens, &p.OutputTokens, &p.ReasoningTokens, &p.CachedTokens, &p.TotalTokens); err != nil {
			return nil, nil, fmt.Errorf("usage: hourly token scan: %w", err)
		}
		tokens = append(tokens, p)
	}

	// query models by hour
	modelQuery := `SELECT strftime('%Y-%m-%d %H:00', timestamp, 'localtime') as h, model, COUNT(*) as reqs
	               FROM request_logs` + where + ` AND model != '' GROUP BY h, model ORDER BY h`
	modelRows, err := db.Query(modelQuery, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: hourly model query: %w", err)
	}
	defer modelRows.Close()

	var models []HourlyModelPoint
	for modelRows.Next() {
		var p HourlyModelPoint
		if err := modelRows.Scan(&p.Hour, &p.Model, &p.Requests); err != nil {
			return nil, nil, fmt.Errorf("usage: hourly model scan: %w", err)
		}
		models = append(models, p)
	}

	return tokens, models, nil
}

// EntityStatPoint holds aggregated usage data for a single entity (source or auth_index).
type EntityStatPoint struct {
	EntityName  string  `json:"entity_name"`
	Requests    int64   `json:"requests"`
	Failed      int64   `json:"failed"`
	AvgLatency  float64 `json:"avg_latency"`
	TotalTokens int64   `json:"total_tokens"`
}

// QueryEntityStats returns aggregates grouped by a given column (e.g. "source" or "auth_index").
// Time range is derived from days logic.
func QueryEntityStats(apiKey string, days int, groupColumn string) ([]EntityStatPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}
	if groupColumn != "source" && groupColumn != "auth_index" {
		return nil, fmt.Errorf("usage: invalid group column")
	}

	params := LogQueryParams{APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)

	q := fmt.Sprintf(`
		SELECT %s, COUNT(*), COALESCE(SUM(failed),0), COALESCE(AVG(latency_ms),0), COALESCE(SUM(total_tokens),0)
		FROM request_logs%s AND %s != ''
		GROUP BY %s ORDER BY COUNT(*) DESC
	`, groupColumn, where, groupColumn, groupColumn)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: entity stats query: %w", err)
	}
	defer rows.Close()

	var result []EntityStatPoint
	for rows.Next() {
		var p EntityStatPoint
		if err := rows.Scan(&p.EntityName, &p.Requests, &p.Failed, &p.AvgLatency, &p.TotalTokens); err != nil {
			return nil, fmt.Errorf("usage: entity stats scan: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// GetRequestLogStorageBytes returns the approximate bytes currently occupied by
// stored request/response bodies. It includes compressed rows in
// request_log_content and any legacy inline content not yet migrated out of
// request_logs.
func GetRequestLogStorageBytes() (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}

	var totalBytes sql.NullInt64
	err := db.QueryRow(`
		SELECT
			COALESCE((
				SELECT SUM(CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER))
				FROM request_log_content
			), 0) +
			COALESCE((
				SELECT SUM(CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER))
				FROM request_logs
				WHERE length(input_content) > 0 OR length(output_content) > 0
			), 0)
	`).Scan(&totalBytes)
	if err != nil {
		return 0, fmt.Errorf("usage: query request log storage bytes: %w", err)
	}
	if !totalBytes.Valid {
		return 0, nil
	}
	return totalBytes.Int64, nil
}

// ChannelLatency holds the average latency stats for a single channel (source).
type ChannelLatency struct {
	Source string  `json:"source"`
	Count  int64   `json:"count"`
	AvgMs  float64 `json:"avg_ms"`
}

// GetChannelAvgLatency returns average request latency grouped by source (channel)
// for the last N days.
func GetChannelAvgLatency(days int) ([]ChannelLatency, error) {
	db := getDB()
	if db == nil {
		return nil, fmt.Errorf("usage: database not initialised")
	}

	cutoff := CutoffStartUTC(days)
	rows, err := db.Query(`
		SELECT source, COUNT(*) as cnt, AVG(latency_ms) as avg_lat
		FROM request_logs
		WHERE timestamp > ? AND source != ''
		GROUP BY source
		ORDER BY avg_lat DESC
		LIMIT 5
	`, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("usage: query channel latency: %w", err)
	}
	defer rows.Close()

	var result []ChannelLatency
	for rows.Next() {
		var cl ChannelLatency
		if err := rows.Scan(&cl.Source, &cl.Count, &cl.AvgMs); err != nil {
			return nil, fmt.Errorf("usage: scan channel latency: %w", err)
		}
		result = append(result, cl)
	}
	return result, rows.Err()
}

// CountTodayByKey returns the number of requests made by the given API key today (project timezone).
func CountTodayByKey(apiKey string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	var count int64
	err := db.QueryRow(
		"SELECT COUNT(*) FROM request_logs WHERE api_key = ? AND timestamp >= ?",
		apiKey, CutoffStartUTC(1).Format(time.RFC3339),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: count today: %w", err)
	}
	return count, nil
}

// CountTotalByKey returns the total number of requests made by the given API key.
func CountTotalByKey(apiKey string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	var count int64
	err := db.QueryRow(
		"SELECT COUNT(*) FROM request_logs WHERE api_key = ?",
		apiKey,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: count total: %w", err)
	}
	return count, nil
}
