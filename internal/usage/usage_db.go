package usage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
	FirstTokenMs    int64     `json:"first_token_ms"`
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
	Page         int      // 1-based
	Size         int      // rows per page
	Days         int      // time range in days
	APIKey       string   // exact match filter
	Model        string   // exact match filter
	Status       string   // "success", "failed", or "" (all)
	AuthIndexes  []string // optional auth_index IN (...) filter
	ChannelNames []string // optional channel_name IN (...) filter
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
	Channels    []string          `json:"channels"`
}

// LogStats holds aggregated stats over the filtered result set.
type LogStats struct {
	Total       int64   `json:"total"`
	SuccessRate float64 `json:"success_rate"`
	TotalTokens int64   `json:"total_tokens"`
	TotalCost   float64 `json:"total_cost"`
}

type DailyCountPoint struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
}

type DailyQuotaPoint struct {
	Date    string   `json:"date"`
	Percent *float64 `json:"percent"`
	Samples int64    `json:"samples"`
}

type HourlyCountPoint struct {
	Hour     string `json:"hour"`
	Requests int64  `json:"requests"`
}

type QuotaSnapshotPoint struct {
	RecordedAt    time.Time  `json:"recorded_at"`
	AuthIndex     string     `json:"auth_index"`
	Provider      string     `json:"provider"`
	QuotaKey      string     `json:"quota_key"`
	QuotaLabel    string     `json:"quota_label"`
	Percent       *float64   `json:"percent"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	WindowSeconds int64      `json:"window_seconds"`
}

type QuotaSnapshotSeriesPoint struct {
	Timestamp time.Time  `json:"timestamp"`
	Percent   *float64   `json:"percent"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

type QuotaSnapshotSeries struct {
	QuotaKey      string                     `json:"quota_key"`
	QuotaLabel    string                     `json:"quota_label"`
	WindowSeconds int64                      `json:"window_seconds"`
	Points        []QuotaSnapshotSeriesPoint `json:"points"`
}

const systemRequestLogFilterValue = "__system__"

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
  first_token_ms   INTEGER NOT NULL DEFAULT 0,
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
  detail_content   BLOB NOT NULL DEFAULT X'',
  FOREIGN KEY(log_id) REFERENCES request_logs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON request_logs(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_logs_api_key ON request_logs(api_key);
CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model);
CREATE INDEX IF NOT EXISTS idx_logs_failed ON request_logs(failed);
CREATE INDEX IF NOT EXISTS idx_logs_auth_index ON request_logs(auth_index);
CREATE INDEX IF NOT EXISTS idx_log_content_timestamp ON request_log_content(timestamp DESC);

CREATE TABLE IF NOT EXISTS auth_file_quota_snapshots (
  date_key      TEXT NOT NULL,
  auth_index    TEXT NOT NULL,
  provider      TEXT NOT NULL DEFAULT '',
  quota_key     TEXT NOT NULL,
  percent       REAL,
  recorded_at   DATETIME NOT NULL,
  PRIMARY KEY (date_key, auth_index, quota_key)
);

CREATE INDEX IF NOT EXISTS idx_quota_snapshots_date ON auth_file_quota_snapshots(date_key);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_auth ON auth_file_quota_snapshots(auth_index);

CREATE TABLE IF NOT EXISTS auth_file_quota_snapshot_points (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  recorded_at    DATETIME NOT NULL,
  auth_index     TEXT NOT NULL,
  provider       TEXT NOT NULL DEFAULT '',
  quota_key      TEXT NOT NULL,
  quota_label    TEXT NOT NULL DEFAULT '',
  percent        REAL,
  reset_at       DATETIME,
  window_seconds INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_auth_time ON auth_file_quota_snapshot_points(auth_index, recorded_at);
CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_auth_key_time ON auth_file_quota_snapshot_points(auth_index, quota_key, recorded_at);
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

// migrateApiKeyNameColumn adds api_key_name column to an existing request_logs table.
// This stores the display name of the API key at the time of the request, so that
// the name is preserved even if the key is later deleted.
func migrateApiKeyNameColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN api_key_name TEXT NOT NULL DEFAULT ''")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column api_key_name: %v", err)
		}
	}
}

// migrateFirstTokenColumn adds first_token_ms column to an existing request_logs table.
func migrateFirstTokenColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN first_token_ms INTEGER NOT NULL DEFAULT 0")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column first_token_ms: %v", err)
		}
	}
}

func migrateRequestLogDetailColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_log_content ADD COLUMN detail_content BLOB NOT NULL DEFAULT X''")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column detail_content: %v", err)
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
	// SQLite ping 属于服务启动期健康检查，不绑定请求生命周期；
	// 这里使用带超时的根 context，避免 WAL 恢复阶段无限阻塞。
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
	log.Debugf("usage: running api_key_name column migration")
	migrateApiKeyNameColumn(db)
	log.Debugf("usage: running first_token_ms column migration")
	migrateFirstTokenColumn(db)
	log.Debugf("usage: running request log detail column migration")
	migrateRequestLogDetailColumn(db)
	log.Debugf("usage: initializing pricing table")
	initPricingTable(db)
	log.Debugf("usage: initializing model config tables")
	initModelConfigTables(db)
	log.Debugf("usage: initializing api_keys table")
	initAPIKeysTable(db)
	log.Debugf("usage: initializing api_key_permission_profiles table")
	initAPIKeyPermissionProfilesTable(db)
	log.Debugf("usage: initializing routing_config table")
	initRoutingConfigTable(db)
	log.Debugf("usage: initializing proxy_pool table")
	initProxyPoolTable(db)
	log.Debugf("usage: initializing runtime_settings table")
	initRuntimeSettingsTable(db)
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
func InsertLog(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent string) {
	insertLog(apiKey, apiKeyName, model, source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, "")
}

func InsertLogWithDetails(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLog(apiKey, apiKeyName, model, source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent)
}

func insertLog(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
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

	// 插入 request log 的事务由 usage 存储层统一拥有，不从外部 HTTP 请求透传 context，
	// 以避免请求取消把已经选定要持久化的审计记录中断在半途。
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		log.Errorf("usage: begin insert tx: %v", err)
		return
	}

	result, err := tx.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_name, model, source, channel_name, auth_index,
			 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.UTC().Format(time.RFC3339Nano),
		apiKey, apiKeyName, model, source, channelName, authIndex,
		failedInt, latencyMs, firstTokenMs,
		tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens,
		tokens.CachedTokens, tokens.TotalTokens, cost,
	)
	if err != nil {
		_ = tx.Rollback()
		log.Errorf("usage: insert log: %v", err)
		return
	}

	if requestLogStorage.StoreContent && (inputContent != "" || outputContent != "" || detailContent != "") {
		logID, errLastID := result.LastInsertId()
		if errLastID != nil {
			_ = tx.Rollback()
			log.Errorf("usage: resolve inserted log id: %v", errLastID)
			return
		}
		if errStore := insertLogContentTx(tx, logID, timestamp, inputContent, outputContent, detailContent); errStore != nil {
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
	// Normalise parameters
	if params.Page < 1 {
		params.Page = 1
	}
	if params.Size < 1 {
		params.Size = 50
	}
	if params.Size > 500 {
		params.Size = 500
	}
	if params.Days < 1 {
		params.Days = 7
	}

	db := getDB()
	if db == nil {
		// Never return nil slices in JSON responses (nil slice => null in JSON).
		return LogQueryResult{
			Items: make([]LogRow, 0),
			Total: 0,
			Page:  params.Page,
			Size:  params.Size,
		}, nil
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
	querySQL := "SELECT id, timestamp, api_key, api_key_name, model, source, channel_name, auth_index, " +
		"failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, " +
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
			&row.ID, &ts, &row.APIKey, &row.APIKeyName, &row.Model, &row.Source, &row.ChannelName,
			&row.AuthIndex, &failedInt, &row.LatencyMs, &row.FirstTokenMs,
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
	if days < 1 {
		days = 7
	}
	db := getDB()
	if db == nil {
		// Ensure stable JSON shape: slices => [] (not null), maps => {} (not null).
		return FilterOptions{
			APIKeys:     make([]string, 0),
			APIKeyNames: make(map[string]string),
			Models:      make([]string, 0),
			Channels:    make([]string, 0),
		}, nil
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
	channels, err := queryDistinct(db, "channel_name", cutoff)
	if err != nil {
		return FilterOptions{}, err
	}

	return FilterOptions{
		APIKeys:     keys,
		APIKeyNames: make(map[string]string),
		Models:      models,
		Channels:    channels,
	}, nil
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

// DeleteLogsByAPIKey removes all request_logs and request_log_content entries
// for the given API key. Returns the number of deleted log rows.
func DeleteLogsByAPIKey(apiKey string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, fmt.Errorf("usage: database not initialised")
	}
	if apiKey == "" {
		return 0, fmt.Errorf("usage: empty api_key")
	}

	// Delete associated content rows first (FK cascade may handle this,
	// but be explicit to ensure cleanup even without FK enforcement).
	_, _ = db.Exec(
		`DELETE FROM request_log_content WHERE log_id IN
		 (SELECT id FROM request_logs WHERE api_key = ?)`, apiKey)

	result, err := db.Exec("DELETE FROM request_logs WHERE api_key = ?", apiKey)
	if err != nil {
		return 0, fmt.Errorf("usage: delete logs by api_key: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage: affected rows: %w", err)
	}
	if deleted > 0 {
		log.Infof("usage: deleted %d request log(s) for api_key=%s", deleted, apiKey)
	}
	return deleted, nil
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
	TotalCost       float64 `json:"total_cost"`
}

type DashboardTrendPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type DashboardThroughputPoint struct {
	Label string  `json:"label"`
	RPM   float64 `json:"rpm"`
	TPM   float64 `json:"tpm"`
}

type DashboardTrends struct {
	RequestVolume    []DashboardTrendPoint      `json:"request_volume"`
	SuccessRate      []DashboardTrendPoint      `json:"success_rate"`
	TotalTokens      []DashboardTrendPoint      `json:"total_tokens"`
	FailedRequests   []DashboardTrendPoint      `json:"failed_requests"`
	ThroughputSeries []DashboardThroughputPoint `json:"throughput_series"`
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
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cost), 0)
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
		&kpi.TotalCost,
	)
	if err != nil {
		return DashboardKPI{}, fmt.Errorf("usage: dashboard KPI query: %w", err)
	}

	if kpi.TotalRequests > 0 {
		kpi.SuccessRate = float64(kpi.SuccessRequests) / float64(kpi.TotalRequests) * 100
	}

	return kpi, nil
}

type dashboardBucket struct {
	label      string
	key        string
	minutes    float64
	requests   int64
	success    int64
	failed     int64
	totalToken int64
}

const dashboardThroughputBucketCount = 7

// QueryDashboardTrends returns fixed-width trend buckets used by the dashboard.
// KPI trends follow the selected day range, while throughput always shows the
// most recent 7 one-minute buckets.
func QueryDashboardTrends(days int) (DashboardTrends, error) {
	db := getDB()
	if db == nil {
		return emptyDashboardTrends(days), nil
	}
	if days < 1 {
		days = 7
	}

	loc := getUsageLocation()
	buckets := buildDashboardBuckets(days, loc)
	byKey := make(map[string]*dashboardBucket, len(buckets))
	for i := range buckets {
		byKey[buckets[i].key] = &buckets[i]
	}

	rows, err := db.Query(`
		SELECT timestamp, failed, total_tokens
		FROM request_logs
		WHERE timestamp >= ?
	`, CutoffStartUTC(days).Format(time.RFC3339))
	if err != nil {
		return DashboardTrends{}, fmt.Errorf("usage: query dashboard trends: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts string
		var failedInt int
		var totalTokens int64
		if err := rows.Scan(&ts, &failedInt, &totalTokens); err != nil {
			return DashboardTrends{}, fmt.Errorf("usage: scan dashboard trend row: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
		}
		key := dashboardBucketKey(parsed.In(loc), days)
		bucket := byKey[key]
		if bucket == nil {
			continue
		}
		bucket.requests++
		bucket.totalToken += totalTokens
		if failedInt != 0 {
			bucket.failed++
		} else {
			bucket.success++
		}
	}
	if err := rows.Err(); err != nil {
		return DashboardTrends{}, fmt.Errorf("usage: iterate dashboard trends: %w", err)
	}

	throughputSeries, err := queryDashboardThroughputSeriesAt(time.Now(), loc)
	if err != nil {
		return DashboardTrends{}, err
	}

	trends := dashboardTrendsFromBuckets(buckets)
	trends.ThroughputSeries = throughputSeries
	return trends, nil
}

func emptyDashboardTrends(days int) DashboardTrends {
	if days < 1 {
		days = 7
	}
	loc := getUsageLocation()
	trends := dashboardTrendsFromBuckets(buildDashboardBuckets(days, loc))
	trends.ThroughputSeries = throughputSeriesFromBuckets(buildRecentThroughputBucketsAt(time.Now(), loc))
	return trends
}

func buildDashboardBuckets(days int, loc *time.Location) []dashboardBucket {
	if loc == nil {
		loc = time.Local
	}
	start := CutoffStartUTC(days).In(loc)
	if days == 1 {
		buckets := make([]dashboardBucket, 0, 24)
		for i := 0; i < 24; i++ {
			at := start.Add(time.Duration(i) * time.Hour)
			buckets = append(buckets, dashboardBucket{
				label:   at.Format("15:04"),
				key:     dashboardBucketKey(at, days),
				minutes: 60,
			})
		}
		return buckets
	}

	buckets := make([]dashboardBucket, 0, days)
	for i := 0; i < days; i++ {
		at := start.AddDate(0, 0, i)
		buckets = append(buckets, dashboardBucket{
			label:   at.Format("2006-01-02"),
			key:     dashboardBucketKey(at, days),
			minutes: 24 * 60,
		})
	}
	return buckets
}

func dashboardBucketKey(t time.Time, days int) string {
	if days == 1 {
		return t.Format("2006-01-02 15")
	}
	return t.Format("2006-01-02")
}

func buildRecentThroughputBucketsAt(now time.Time, loc *time.Location) []dashboardBucket {
	if loc == nil {
		loc = time.Local
	}
	currentMinute := now.In(loc).Truncate(time.Minute)
	start := currentMinute.Add(-time.Duration(dashboardThroughputBucketCount-1) * time.Minute)
	buckets := make([]dashboardBucket, 0, dashboardThroughputBucketCount)
	for i := 0; i < dashboardThroughputBucketCount; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		buckets = append(buckets, dashboardBucket{
			label:   at.Format("15:04"),
			key:     at.Format("2006-01-02 15:04"),
			minutes: 1,
		})
	}
	return buckets
}

func queryDashboardThroughputSeriesAt(now time.Time, loc *time.Location) ([]DashboardThroughputPoint, error) {
	db := getDB()
	if db == nil {
		return throughputSeriesFromBuckets(buildRecentThroughputBucketsAt(now, loc)), nil
	}
	if loc == nil {
		loc = time.Local
	}

	buckets := buildRecentThroughputBucketsAt(now, loc)
	byKey := make(map[string]*dashboardBucket, len(buckets))
	for i := range buckets {
		byKey[buckets[i].key] = &buckets[i]
	}

	start := now.In(loc).Truncate(time.Minute).Add(-time.Duration(dashboardThroughputBucketCount-1) * time.Minute)
	rows, err := db.Query(`
		SELECT timestamp, total_tokens
		FROM request_logs
		WHERE timestamp >= ?
	`, start.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("usage: query dashboard throughput trends: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts string
		var totalTokens int64
		if err := rows.Scan(&ts, &totalTokens); err != nil {
			return nil, fmt.Errorf("usage: scan dashboard throughput row: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
		}
		key := parsed.In(loc).Truncate(time.Minute).Format("2006-01-02 15:04")
		bucket := byKey[key]
		if bucket == nil {
			continue
		}
		bucket.requests++
		bucket.totalToken += totalTokens
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate dashboard throughput rows: %w", err)
	}

	return throughputSeriesFromBuckets(buckets), nil
}

func dashboardTrendsFromBuckets(buckets []dashboardBucket) DashboardTrends {
	trends := DashboardTrends{
		RequestVolume:    make([]DashboardTrendPoint, 0, len(buckets)),
		SuccessRate:      make([]DashboardTrendPoint, 0, len(buckets)),
		TotalTokens:      make([]DashboardTrendPoint, 0, len(buckets)),
		FailedRequests:   make([]DashboardTrendPoint, 0, len(buckets)),
		ThroughputSeries: make([]DashboardThroughputPoint, 0),
	}

	for _, bucket := range buckets {
		successRate := 0.0
		if bucket.requests > 0 {
			successRate = float64(bucket.success) / float64(bucket.requests) * 100
		}

		trends.RequestVolume = append(trends.RequestVolume, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.requests)})
		trends.SuccessRate = append(trends.SuccessRate, DashboardTrendPoint{Label: bucket.label, Value: successRate})
		trends.TotalTokens = append(trends.TotalTokens, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.totalToken)})
		trends.FailedRequests = append(trends.FailedRequests, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.failed)})
	}

	return trends
}

func throughputSeriesFromBuckets(buckets []dashboardBucket) []DashboardThroughputPoint {
	points := make([]DashboardThroughputPoint, 0, len(buckets))
	for _, bucket := range buckets {
		rpm := 0.0
		tpm := 0.0
		if bucket.minutes > 0 {
			rpm = float64(bucket.requests) / bucket.minutes
			tpm = float64(bucket.totalToken) / bucket.minutes
		}
		points = append(points, DashboardThroughputPoint{
			Label: bucket.label,
			RPM:   rpm,
			TPM:   tpm,
		})
	}
	return points
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

func parseStoredTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

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

func localDayKeyAt(t time.Time) string {
	loc := getUsageLocation()
	return t.In(loc).Format("2006-01-02")
}

// LocalDayKeyAt returns the YYYY-MM-DD day key in the project-configured timezone.
func LocalDayKeyAt(t time.Time) string {
	return localDayKeyAt(t)
}

func cutoffDayKey(days int) string {
	return localDayKeyAt(CutoffStartUTC(days))
}

func buildWhereClause(params LogQueryParams) (string, []interface{}) {
	conditions := make([]string, 0, 4)
	args := make([]interface{}, 0, 4)

	// Time range: days=1 means "today", days=7 means "last 7 days", etc.
	conditions = append(conditions, "timestamp >= ?")
	args = append(args, CutoffStartUTC(params.Days).Format(time.RFC3339))

	if params.APIKey != "" {
		if params.APIKey == systemRequestLogFilterValue {
			conditions = append(conditions, `(
				trim(coalesce(api_key_name, '')) = ''
				AND (
					trim(coalesce(api_key, '')) = ''
					OR trim(coalesce(api_key, '')) LIKE '/%'
					OR upper(trim(coalesce(api_key, ''))) LIKE 'GET /%'
					OR upper(trim(coalesce(api_key, ''))) LIKE 'POST /%'
					OR upper(trim(coalesce(api_key, ''))) LIKE 'PUT /%'
					OR upper(trim(coalesce(api_key, ''))) LIKE 'PATCH /%'
					OR upper(trim(coalesce(api_key, ''))) LIKE 'DELETE /%'
					OR upper(trim(coalesce(api_key, ''))) LIKE 'OPTIONS /%'
					OR upper(trim(coalesce(api_key, ''))) LIKE 'HEAD /%'
				)
			)`)
		} else {
			conditions = append(conditions, "api_key = ?")
			args = append(args, params.APIKey)
		}
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
	if len(params.AuthIndexes) > 0 || len(params.ChannelNames) > 0 {
		filterConditions := make([]string, 0, 2)

		authPlaceholders := make([]string, 0, len(params.AuthIndexes))
		for _, idx := range params.AuthIndexes {
			trimmed := strings.TrimSpace(idx)
			if trimmed == "" {
				continue
			}
			authPlaceholders = append(authPlaceholders, "?")
			args = append(args, trimmed)
		}
		if len(authPlaceholders) > 0 {
			filterConditions = append(filterConditions, "(auth_index IN ("+strings.Join(authPlaceholders, ",")+") AND trim(coalesce(channel_name, '')) = '')")
		}

		channelPlaceholders := make([]string, 0, len(params.ChannelNames))
		for _, name := range params.ChannelNames {
			trimmed := strings.ToLower(strings.TrimSpace(name))
			if trimmed == "" {
				continue
			}
			channelPlaceholders = append(channelPlaceholders, "?")
			args = append(args, trimmed)
		}
		if len(channelPlaceholders) > 0 {
			filterConditions = append(filterConditions, "lower(trim(channel_name)) IN ("+strings.Join(channelPlaceholders, ",")+")")
		}

		if len(filterConditions) > 0 {
			conditions = append(conditions, "("+strings.Join(filterConditions, " OR ")+")")
		} else {
			// If caller attempted to filter but provided no usable channel selectors, match nothing.
			conditions = append(conditions, "1 = 0")
		}
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

	result := make([]string, 0)
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
		return make([]string, 0), nil
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

	result := make([]string, 0)
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

// LogContentPartResult holds one side (input/output) of the content detail for a single log entry.
// It is used to avoid decompressing/transferring both large blobs when the UI only needs one tab.
type LogContentPartResult struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
	Model   string `json:"model"`
	Part    string `json:"part"`
}

func normalizeLogContentPart(part string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(part)) {
	case "input":
		return "input", nil
	case "output":
		return "output", nil
	case "details":
		return "details", nil
	default:
		return "", fmt.Errorf("usage: invalid content part %q", part)
	}
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

// QueryLogContentPart retrieves only one side (input/output) of the stored request/response content
// for a single log entry. This avoids decompressing/transferring both blobs for the UI.
func QueryLogContentPart(id int64, part string) (LogContentPartResult, error) {
	db := getDB()
	if db == nil {
		return LogContentPartResult{}, fmt.Errorf("usage: database not initialised")
	}

	part, err := normalizeLogContentPart(part)
	if err != nil {
		return LogContentPartResult{}, err
	}

	column := "input_content"
	if part == "output" {
		column = "output_content"
	} else if part == "details" {
		column = "detail_content"
	}

	result, err := queryCompressedLogContentPart(
		db,
		part,
		fmt.Sprintf(
			`SELECT logs.id, logs.model, content.compression, content.%s
			 FROM request_logs logs
			 JOIN request_log_content content ON content.log_id = logs.id
			 WHERE logs.id = ?`,
			column,
		),
		id,
	)
	if err == nil {
		return result, nil
	}
	if part == "details" {
		var fallback LogContentPartResult
		fallback.Part = part
		err = db.QueryRow("SELECT id, model FROM request_logs WHERE id = ?", id).Scan(&fallback.ID, &fallback.Model)
		if err != nil {
			return LogContentPartResult{}, fmt.Errorf("usage: query log content part: %w", err)
		}
		return fallback, nil
	}

	var fallback LogContentPartResult
	fallback.Part = part
	err = db.QueryRow(
		fmt.Sprintf("SELECT id, model, %s FROM request_logs WHERE id = ?", column),
		id,
	).Scan(&fallback.ID, &fallback.Model, &fallback.Content)
	if err != nil {
		return LogContentPartResult{}, fmt.Errorf("usage: query log content part: %w", err)
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

// QueryLogContentPartForKey retrieves only one side (input/output) of the stored request/response content
// for a single entry, but only if it belongs to the given API key.
func QueryLogContentPartForKey(id int64, apiKey string, part string) (LogContentPartResult, error) {
	db := getDB()
	if db == nil {
		return LogContentPartResult{}, fmt.Errorf("usage: database not initialised")
	}

	part, err := normalizeLogContentPart(part)
	if err != nil {
		return LogContentPartResult{}, err
	}

	column := "input_content"
	if part == "output" {
		column = "output_content"
	} else if part == "details" {
		column = "detail_content"
	}

	result, err := queryCompressedLogContentPart(
		db,
		part,
		fmt.Sprintf(
			`SELECT logs.id, logs.model, content.compression, content.%s
			 FROM request_logs logs
			 JOIN request_log_content content ON content.log_id = logs.id
			 WHERE logs.id = ? AND logs.api_key = ?`,
			column,
		),
		id, apiKey,
	)
	if err == nil {
		return result, nil
	}
	if part == "details" {
		var fallback LogContentPartResult
		fallback.Part = part
		err = db.QueryRow("SELECT id, model FROM request_logs WHERE id = ? AND api_key = ?", id, apiKey).Scan(&fallback.ID, &fallback.Model)
		if err != nil {
			return LogContentPartResult{}, fmt.Errorf("usage: query log content part: %w", err)
		}
		return fallback, nil
	}

	var fallback LogContentPartResult
	fallback.Part = part
	err = db.QueryRow(
		fmt.Sprintf("SELECT id, model, %s FROM request_logs WHERE id = ? AND api_key = ?", column),
		id, apiKey,
	).Scan(&fallback.ID, &fallback.Model, &fallback.Content)
	if err != nil {
		return LogContentPartResult{}, fmt.Errorf("usage: query log content part: %w", err)
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

// APIKeyDistributionPoint holds aggregated usage data for a single API key.
type APIKeyDistributionPoint struct {
	APIKey   string `json:"api_key"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// QueryAPIKeyDistribution returns request count and token usage grouped by api_key.
func QueryAPIKeyDistribution(days int) ([]APIKeyDistributionPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	params := LogQueryParams{Days: days}
	where, args := buildWhereClause(params)

	q := `SELECT api_key,
	             COALESCE(NULLIF(MAX(api_key_name),''), '') as name,
	             COUNT(*) as reqs,
	             COALESCE(SUM(total_tokens),0)
	      FROM request_logs` + where + `
	      AND api_key != ''
	      GROUP BY api_key ORDER BY reqs DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: apikey distribution query: %w", err)
	}
	defer rows.Close()

	var result []APIKeyDistributionPoint
	for rows.Next() {
		var p APIKeyDistributionPoint
		if err := rows.Scan(&p.APIKey, &p.Name, &p.Requests, &p.Tokens); err != nil {
			return nil, fmt.Errorf("usage: apikey distribution scan: %w", err)
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

	// Build WHERE clause directly with the correct hourly cutoff.
	// Previously this used buildWhereClause + strings.Replace, but that failed
	// because buildWhereClause uses parameterised queries (? placeholders)
	// so the time value lives in args, not in the where string.
	conditions := []string{"timestamp >= ?"}
	args := []interface{}{cutoff}
	if apiKey != "" {
		conditions = append(conditions, "api_key = ?")
		args = append(args, apiKey)
	}
	where := " WHERE " + strings.Join(conditions, " AND ")

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

func QueryDailyCallsByAuthIndexes(authIndexes []string, days int) ([]DailyCountPoint, error) {
	db := getDB()
	if db == nil {
		return []DailyCountPoint{}, nil
	}
	if days < 1 {
		days = 7
	}
	if len(authIndexes) == 0 {
		return []DailyCountPoint{}, nil
	}

	seen := make(map[string]struct{}, len(authIndexes))
	normalized := make([]string, 0, len(authIndexes))
	for _, idx := range authIndexes {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			continue
		}
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		normalized = append(normalized, idx)
	}
	if len(normalized) == 0 {
		return []DailyCountPoint{}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	args := make([]interface{}, 0, len(normalized)+1)
	args = append(args, CutoffStartUTC(days).Format(time.RFC3339))
	for _, idx := range normalized {
		args = append(args, idx)
	}

	q := fmt.Sprintf(`
		SELECT timestamp
		FROM request_logs
		WHERE timestamp >= ? AND auth_index IN (%s)
		ORDER BY timestamp ASC
	`, placeholders)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: daily calls by auth indexes query: %w", err)
	}
	defer rows.Close()

	byDate := make(map[string]int64, days)
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("usage: daily calls by auth indexes scan: %w", err)
		}
		parsed, ok := parseStoredTime(ts)
		if !ok {
			continue
		}
		byDate[localDayKeyAt(parsed)]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]DailyCountPoint, 0, len(byDate))
	for date, requests := range byDate {
		point := DailyCountPoint{Date: date, Requests: requests}
		result = append(result, point)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Date < result[j].Date
	})
	return result, nil
}

func QueryHourlyCallsByAuthIndex(authIndex string, hours int) ([]HourlyCountPoint, error) {
	db := getDB()
	if db == nil {
		return []HourlyCountPoint{}, nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return []HourlyCountPoint{}, nil
	}
	if hours < 1 {
		hours = 5
	}
	if hours > 24 {
		hours = 24
	}

	loc := getUsageLocation()
	now := time.Now().In(loc).Truncate(time.Hour)
	start := now.Add(-time.Duration(hours-1) * time.Hour)
	buckets := make([]HourlyCountPoint, 0, hours)
	byKey := make(map[string]*HourlyCountPoint, hours)
	for i := 0; i < hours; i++ {
		key := start.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:00")
		buckets = append(buckets, HourlyCountPoint{Hour: key, Requests: 0})
		byKey[key] = &buckets[len(buckets)-1]
	}

	rows, err := db.Query(`
		SELECT timestamp
		FROM request_logs
		WHERE timestamp >= ? AND auth_index = ?
	`, start.UTC().Format(time.RFC3339), authIndex)
	if err != nil {
		return nil, fmt.Errorf("usage: hourly calls by auth index query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("usage: hourly calls by auth index scan: %w", err)
		}
		parsed, ok := parseStoredTime(ts)
		if !ok {
			continue
		}
		key := parsed.In(loc).Truncate(time.Hour).Format("2006-01-02 15:00")
		if bucket := byKey[key]; bucket != nil {
			bucket.Requests++
		}
	}
	return buckets, rows.Err()
}

func QueryRequestCountByAuthIndexSince(authIndex string, since time.Time) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return 0, nil
	}
	var count int64
	err := db.QueryRow(`
		SELECT COUNT(*)
		FROM request_logs
		WHERE timestamp >= ? AND auth_index = ?
	`, since.UTC().Format(time.RFC3339), authIndex).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: request count by auth index query: %w", err)
	}
	return count, nil
}

func RecordDailyQuotaSnapshot(authIndex, provider string, quotas map[string]*float64) error {
	db := getDB()
	if db == nil {
		return nil
	}

	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || len(quotas) == 0 {
		return nil
	}
	provider = strings.TrimSpace(provider)
	now := time.Now()
	dateKey := localDayKeyAt(now)
	recordedAt := now.UTC().Format(time.RFC3339Nano)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: quota snapshot begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.Prepare(`
		INSERT INTO auth_file_quota_snapshots (date_key, auth_index, provider, quota_key, percent, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(date_key, auth_index, quota_key) DO UPDATE SET
			provider = excluded.provider,
			percent = excluded.percent,
			recorded_at = excluded.recorded_at
	`)
	if err != nil {
		return fmt.Errorf("usage: quota snapshot prepare: %w", err)
	}
	defer stmt.Close()

	for key, rawPercent := range quotas {
		quotaKey := strings.TrimSpace(key)
		if quotaKey == "" {
			continue
		}
		var value any
		if rawPercent == nil {
			value = nil
		} else {
			percent := *rawPercent
			if percent < 0 {
				percent = 0
			}
			if percent > 100 {
				percent = 100
			}
			value = percent
		}
		if _, err = stmt.Exec(dateKey, authIndex, provider, quotaKey, value, recordedAt); err != nil {
			return fmt.Errorf("usage: quota snapshot upsert: %w", err)
		}
	}

	retentionCutoff := cutoffDayKey(7)
	if _, err = tx.Exec(`DELETE FROM auth_file_quota_snapshots WHERE date_key < ?`, retentionCutoff); err != nil {
		return fmt.Errorf("usage: quota snapshot prune: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usage: quota snapshot commit: %w", err)
	}
	return nil
}

func RecordQuotaSnapshotPoints(authIndex, provider string, points []QuotaSnapshotPoint) error {
	db := getDB()
	if db == nil {
		return nil
	}

	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || len(points) == 0 {
		return nil
	}
	provider = strings.TrimSpace(provider)
	now := time.Now()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: quota snapshot points begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.Prepare(`
		INSERT INTO auth_file_quota_snapshot_points
			(recorded_at, auth_index, provider, quota_key, quota_label, percent, reset_at, window_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("usage: quota snapshot points prepare: %w", err)
	}
	defer stmt.Close()

	for _, point := range points {
		quotaKey := strings.TrimSpace(point.QuotaKey)
		if quotaKey == "" {
			continue
		}
		quotaLabel := strings.TrimSpace(point.QuotaLabel)
		if quotaLabel == "" {
			quotaLabel = quotaKey
		}
		recordedAt := point.RecordedAt
		if recordedAt.IsZero() {
			recordedAt = now
		}
		pointProvider := strings.TrimSpace(point.Provider)
		if pointProvider == "" {
			pointProvider = provider
		}
		var value any
		if point.Percent == nil {
			value = nil
		} else {
			percent := *point.Percent
			if percent < 0 {
				percent = 0
			}
			if percent > 100 {
				percent = 100
			}
			value = percent
		}
		var resetValue any
		if point.ResetAt != nil && !point.ResetAt.IsZero() {
			resetValue = point.ResetAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err = stmt.Exec(
			recordedAt.UTC().Format(time.RFC3339Nano),
			authIndex,
			pointProvider,
			quotaKey,
			quotaLabel,
			value,
			resetValue,
			point.WindowSeconds,
		); err != nil {
			return fmt.Errorf("usage: quota snapshot points insert: %w", err)
		}
	}

	retentionCutoff := now.AddDate(0, 0, -8).UTC().Format(time.RFC3339Nano)
	if _, err = tx.Exec(`DELETE FROM auth_file_quota_snapshot_points WHERE recorded_at < ?`, retentionCutoff); err != nil {
		return fmt.Errorf("usage: quota snapshot points prune: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usage: quota snapshot points commit: %w", err)
	}
	return nil
}

func QueryDailyQuotaByAuthIndexes(authIndexes []string, quotaKey string, days int) ([]DailyQuotaPoint, error) {
	db := getDB()
	if db == nil {
		return []DailyQuotaPoint{}, nil
	}
	if days < 1 {
		days = 7
	}
	if len(authIndexes) == 0 {
		return []DailyQuotaPoint{}, nil
	}
	quotaKey = strings.TrimSpace(quotaKey)
	if quotaKey == "" {
		return []DailyQuotaPoint{}, nil
	}

	seen := make(map[string]struct{}, len(authIndexes))
	normalized := make([]string, 0, len(authIndexes))
	for _, idx := range authIndexes {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			continue
		}
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		normalized = append(normalized, idx)
	}
	if len(normalized) == 0 {
		return []DailyQuotaPoint{}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	args := make([]interface{}, 0, len(normalized)+2)
	args = append(args, cutoffDayKey(days), quotaKey)
	for _, idx := range normalized {
		args = append(args, idx)
	}

	q := fmt.Sprintf(`
		SELECT date_key, AVG(percent) AS avg_percent, COUNT(percent) AS samples
		FROM auth_file_quota_snapshots
		WHERE date_key >= ? AND quota_key = ? AND auth_index IN (%s) AND percent IS NOT NULL
		GROUP BY date_key
		ORDER BY date_key ASC
	`, placeholders)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: daily quota by auth indexes query: %w", err)
	}
	defer rows.Close()

	result := make([]DailyQuotaPoint, 0, days)
	for rows.Next() {
		var point DailyQuotaPoint
		var percent sql.NullFloat64
		if err := rows.Scan(&point.Date, &percent, &point.Samples); err != nil {
			return nil, fmt.Errorf("usage: daily quota by auth indexes scan: %w", err)
		}
		if percent.Valid {
			v := percent.Float64
			point.Percent = &v
		}
		result = append(result, point)
	}
	return result, rows.Err()
}

func QueryQuotaSnapshotPoints(authIndex string, start, end time.Time) ([]QuotaSnapshotPoint, error) {
	db := getDB()
	if db == nil {
		return []QuotaSnapshotPoint{}, nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return []QuotaSnapshotPoint{}, nil
	}
	if start.IsZero() {
		start = time.Now().AddDate(0, 0, -7)
	}
	if end.IsZero() {
		end = time.Now()
	}

	rows, err := db.Query(`
		SELECT recorded_at, auth_index, provider, quota_key, quota_label, percent, reset_at, window_seconds
		FROM auth_file_quota_snapshot_points
		WHERE auth_index = ? AND recorded_at >= ? AND recorded_at <= ?
		ORDER BY recorded_at ASC, quota_key ASC
	`, authIndex, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("usage: quota snapshot points query: %w", err)
	}
	defer rows.Close()

	result := make([]QuotaSnapshotPoint, 0)
	for rows.Next() {
		var point QuotaSnapshotPoint
		var recordedAt string
		var resetAt sql.NullString
		var percent sql.NullFloat64
		if err := rows.Scan(
			&recordedAt,
			&point.AuthIndex,
			&point.Provider,
			&point.QuotaKey,
			&point.QuotaLabel,
			&percent,
			&resetAt,
			&point.WindowSeconds,
		); err != nil {
			return nil, fmt.Errorf("usage: quota snapshot points scan: %w", err)
		}
		if parsed, ok := parseStoredTime(recordedAt); ok {
			point.RecordedAt = parsed
		}
		if percent.Valid {
			v := percent.Float64
			point.Percent = &v
		}
		if resetAt.Valid {
			if parsed, ok := parseStoredTime(resetAt.String); ok {
				point.ResetAt = &parsed
			}
		}
		result = append(result, point)
	}
	return result, rows.Err()
}

func QueryQuotaSnapshotSeries(authIndex string, start, end time.Time) ([]QuotaSnapshotSeries, error) {
	points, err := QueryQuotaSnapshotPoints(authIndex, start, end)
	if err != nil {
		return nil, err
	}
	series := make([]QuotaSnapshotSeries, 0)
	indexByKey := make(map[string]int)
	for _, point := range points {
		seriesKey := fmt.Sprintf("%s\x00%d", point.QuotaKey, point.WindowSeconds)
		idx, ok := indexByKey[seriesKey]
		if !ok {
			idx = len(series)
			indexByKey[seriesKey] = idx
			series = append(series, QuotaSnapshotSeries{
				QuotaKey:      point.QuotaKey,
				QuotaLabel:    point.QuotaLabel,
				WindowSeconds: point.WindowSeconds,
				Points:        []QuotaSnapshotSeriesPoint{},
			})
		}
		series[idx].Points = append(series[idx].Points, QuotaSnapshotSeriesPoint{
			Timestamp: point.RecordedAt,
			Percent:   point.Percent,
			ResetAt:   point.ResetAt,
		})
	}
	return series, nil
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
				SELECT SUM(CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER) + CAST(length(detail_content) AS INTEGER))
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
