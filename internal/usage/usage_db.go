package usage

import (
	"context"
	"database/sql"
	"encoding/json"
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
	Streaming       bool      `json:"streaming"`
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
	Page            int      // 1-based
	Size            int      // rows per page
	Days            int      // time range in days
	APIKey          string   // exact match filter (deprecated, use APIKeys)
	Model           string   // exact match filter (deprecated, use Models)
	Status          string   // "success", "failed", or "" (all) (deprecated, use Statuses)
	APIKeys         []string // multi-value API key filter
	Models          []string // multi-value model filter
	Statuses        []string // multi-value status filter
	MatchNoAPIKeys  bool     // explicit empty API key filter
	MatchNoModels   bool     // explicit empty model filter
	MatchNoStatuses bool     // explicit empty status filter
	MatchNoChannels bool     // explicit empty channel filter
	AuthIndexes     []string // optional auth_index IN (...) filter
	ChannelNames    []string // optional channel_name IN (...) filter
	// Optional precise legacy matches for renamed auth channels whose stored
	// channel_name was a shared provider/source value.
	AuthIndexChannelNames map[string][]string
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
	CacheRate   float64 `json:"cache_rate"`
}

const cacheRateEffectiveInputSQL = "CASE WHEN cached_tokens > input_tokens THEN input_tokens + cached_tokens ELSE input_tokens END"

func cacheRateFromTokenTotals(effectiveInputTokens, cachedTokens int64) float64 {
	if effectiveInputTokens <= 0 {
		return 0
	}
	return float64(cachedTokens) / float64(effectiveInputTokens) * 100
}

type ClearRequestLogsResult struct {
	DeletedLogs       int64 `json:"deleted_logs"`
	DeletedContents   int64 `json:"deleted_contents"`
	ClearedBodyRows   int64 `json:"cleared_body_rows"`
	ClearedDetailRows int64 `json:"cleared_detail_rows"`
	ClearedLegacyRows int64 `json:"cleared_legacy_rows"`
}

type ClearRequestLogsOptions struct {
	ClearBodyContent    bool `json:"clear_body_content"`
	ClearDetailContent  bool `json:"clear_detail_content"`
	ClearRequestRecords bool `json:"clear_request_records"`
}

const systemRequestLogFilterValue = "__system__"

var (
	usageDB     *sql.DB
	usageReadDB *sql.DB
	usageDBMu   sync.Mutex
	usageDBPath string
	usageLoc    *time.Location
)

const createTableSQL = `
CREATE TABLE IF NOT EXISTS request_logs (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  timestamp        DATETIME NOT NULL,
  api_key          TEXT NOT NULL DEFAULT '',
  api_key_id       TEXT NOT NULL DEFAULT '',
  auth_subject_id  TEXT NOT NULL DEFAULT '',
  model            TEXT NOT NULL DEFAULT '',
  source           TEXT NOT NULL DEFAULT '',
  channel_name     TEXT NOT NULL DEFAULT '',
  auth_index       TEXT NOT NULL DEFAULT '',
  failed           INTEGER NOT NULL DEFAULT 0,
  streaming        INTEGER NOT NULL DEFAULT 0,
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
  auth_subject_id TEXT NOT NULL DEFAULT '',
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
  auth_subject_id TEXT NOT NULL DEFAULT '',
  provider       TEXT NOT NULL DEFAULT '',
  quota_key      TEXT NOT NULL,
  quota_label    TEXT NOT NULL DEFAULT '',
  percent        REAL,
  reset_at       DATETIME,
  window_seconds INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_auth_time ON auth_file_quota_snapshot_points(auth_index, recorded_at);
CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_auth_key_time ON auth_file_quota_snapshot_points(auth_index, quota_key, recorded_at);

CREATE TABLE IF NOT EXISTS auth_subject_quota_cycles (
  subject_id       TEXT NOT NULL,
  auth_index       TEXT NOT NULL DEFAULT '',
  provider         TEXT NOT NULL DEFAULT '',
  quota_key        TEXT NOT NULL,
  cycle_start_at   DATETIME NOT NULL,
  reset_at         DATETIME NOT NULL,
  window_seconds   INTEGER NOT NULL DEFAULT 0,
  last_verified_at DATETIME NOT NULL,
  PRIMARY KEY (subject_id, quota_key)
);

CREATE INDEX IF NOT EXISTS idx_auth_subject_quota_cycles_subject_window
  ON auth_subject_quota_cycles(subject_id, window_seconds, last_verified_at);
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

func migrateAPIKeyIDColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN api_key_id TEXT NOT NULL DEFAULT ''")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column api_key_id: %v", err)
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_logs_api_key_id ON request_logs(api_key_id)"); err != nil {
		log.Warnf("usage: create idx_logs_api_key_id: %v", err)
	}
}

func migrateAuthSubjectIDColumns(db *sql.DB) {
	for _, stmt := range []string{
		"ALTER TABLE request_logs ADD COLUMN auth_subject_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE auth_file_quota_snapshots ADD COLUMN auth_subject_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE auth_file_quota_snapshot_points ADD COLUMN auth_subject_id TEXT NOT NULL DEFAULT ''",
	} {
		if _, err := db.Exec(stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate") {
				log.Warnf("usage: migrate auth subject column: %v", err)
			}
		}
	}
	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS idx_logs_auth_subject_id ON request_logs(auth_subject_id)",
		"CREATE INDEX IF NOT EXISTS idx_quota_snapshots_subject ON auth_file_quota_snapshots(auth_subject_id)",
		"CREATE INDEX IF NOT EXISTS idx_quota_snapshot_points_subject_time ON auth_file_quota_snapshot_points(auth_subject_id, recorded_at)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			log.Warnf("usage: create auth subject index: %v", err)
		}
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS auth_subject_quota_cycles (
		  subject_id       TEXT NOT NULL,
		  auth_index       TEXT NOT NULL DEFAULT '',
		  provider         TEXT NOT NULL DEFAULT '',
		  quota_key        TEXT NOT NULL,
		  cycle_start_at   DATETIME NOT NULL,
		  reset_at         DATETIME NOT NULL,
		  window_seconds   INTEGER NOT NULL DEFAULT 0,
		  last_verified_at DATETIME NOT NULL,
		  PRIMARY KEY (subject_id, quota_key)
		)
	`); err != nil {
		log.Warnf("usage: create auth_subject_quota_cycles table: %v", err)
	}
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_auth_subject_quota_cycles_subject_window
		ON auth_subject_quota_cycles(subject_id, window_seconds, last_verified_at)
	`); err != nil {
		log.Warnf("usage: create idx_auth_subject_quota_cycles_subject_window: %v", err)
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

func migrateStreamingColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN streaming INTEGER NOT NULL DEFAULT 0")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column streaming: %v", err)
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

func ensureRequestLogDetailIndexes(db *sql.DB) {
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_log_content_detail_timestamp
		ON request_log_content(timestamp DESC)
		WHERE length(detail_content) > 0
	`); err != nil {
		log.Warnf("usage: create idx_log_content_detail_timestamp: %v", err)
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
	// synchronous=NORMAL under WAL is safe (no corruption on power loss for
	// committed txns) and reduces fsync contention between the writer and readers.
	if _, err := db.Exec("PRAGMA synchronous = NORMAL"); err != nil {
		log.Warnf("usage: failed to set synchronous=NORMAL: %v", err)
	}

	// Open a separate read-only connection pool so management reads (QueryLogs,
	// QueryStats, QueryFilters, content queries) do not serialize behind the
	// single writer or maintenance ops (wal_checkpoint/VACUUM). WAL mode allows
	// concurrent readers alongside a writer, so reads stay responsive while inserts
	// or maintenance hold the write connection. WAL is persisted on the DB file by
	// the writer above, so the read-only handle opens in WAL mode automatically.
	readDB, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("usage: open sqlite read-only handle: %w", err)
	}
	// Readers can run concurrently with each other and with the writer under WAL.
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(2)
	readDB.SetConnMaxLifetime(30 * time.Minute)
	if err := readDB.PingContext(pingCtx); err != nil {
		_ = db.Close()
		_ = readDB.Close()
		return fmt.Errorf("usage: ping sqlite read-only handle: %w", err)
	}

	log.Debugf("usage: creating tables")
	if _, err := db.Exec(createTableSQL); err != nil {
		_ = db.Close()
		return fmt.Errorf("usage: create table: %w", err)
	}

	usageDB = db
	usageReadDB = readDB
	usageDBPath = dbPath
	requestLogStorage = normalizeRequestLogStorageConfig(storageCfg)
	log.Debugf("usage: running content column migration")
	migrateContentColumns(db)
	log.Debugf("usage: running cost column migration")
	migrateCostColumn(db)
	log.Debugf("usage: running api_key_name column migration")
	migrateApiKeyNameColumn(db)
	log.Debugf("usage: running api_key_id column migration")
	migrateAPIKeyIDColumn(db)
	log.Debugf("usage: running auth_subject_id column migration")
	migrateAuthSubjectIDColumns(db)
	log.Debugf("usage: running first_token_ms column migration")
	migrateFirstTokenColumn(db)
	log.Debugf("usage: running streaming column migration")
	migrateStreamingColumn(db)
	log.Debugf("usage: running request log detail column migration")
	migrateRequestLogDetailColumn(db)
	log.Debugf("usage: ensuring request log detail indexes")
	ensureRequestLogDetailIndexes(db)
	log.Debugf("usage: initializing pricing table")
	initPricingTable(db)
	log.Debugf("usage: initializing model config tables")
	initModelConfigTables(db)
	log.Debugf("usage: initializing api_keys table")
	initAPIKeysTable(db)
	log.Debugf("usage: backfilling request log api_key_id values")
	backfillRequestLogAPIKeyIDs(db)
	log.Debugf("usage: initializing api_key_permission_profiles table")
	initAPIKeyPermissionProfilesTable(db)
	log.Debugf("usage: initializing ccswitch_import_configs table")
	initCcSwitchImportConfigsTable(db)
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
	}
	if usageReadDB != nil {
		_ = usageReadDB.Close()
		usageReadDB = nil
	}
	usageLoc = nil
	log.Info("usage: SQLite database closed")
}

// InsertLog writes a single request log entry into the SQLite database.
// It is safe to call concurrently.
func InsertLog(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent string) {
	insertLogIdentity(apiKey, "", "", apiKeyName, model, source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, "")
}

func InsertLogWithDetails(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLogIdentity(apiKey, "", "", apiKeyName, model, source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent)
}

func InsertLogWithDetailsIdentity(apiKey, apiKeyID, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLogIdentity(apiKey, apiKeyID, "", apiKeyName, model, source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent)
}

func InsertLogWithDetailsIdentitySubject(apiKey, apiKeyID, authSubjectID, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLogIdentity(apiKey, apiKeyID, authSubjectID, apiKeyName, model, source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent)
}

func insertLogIdentity(apiKey, apiKeyID, authSubjectID, apiKeyName, model, source, channelName, authIndex string,
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
	streamingInt := 0
	if isStreamingRequestContent(inputContent) {
		streamingInt = 1
	}

	// Calculate cost based on model pricing using semantic cache read/write
	cost := CalculateCostV2(model, tokens)

	apiKeyID = strings.TrimSpace(apiKeyID)
	authSubjectID = strings.TrimSpace(authSubjectID)
	apiKeyName = strings.TrimSpace(apiKeyName)
	if identity := ResolveAPIKeyIdentity(apiKey); identity != nil {
		if apiKeyID == "" {
			apiKeyID = identity.ID
		}
		if apiKeyName == "" {
			apiKeyName = identity.Name
		}
	}

	// 插入 request log 的事务由 usage 存储层统一拥有，不从外部 HTTP 请求透传 context，
	// 以避免请求取消把已经选定要持久化的审计记录中断在半途。
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		log.Errorf("usage: begin insert tx: %v", err)
		return
	}

	result, err := tx.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_id, auth_subject_id, api_key_name, model, source, channel_name, auth_index,
			 failed, streaming, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.UTC().Format(time.RFC3339Nano),
		apiKey, apiKeyID, authSubjectID, apiKeyName, model, source, channelName, authIndex,
		failedInt, streamingInt, latencyMs, firstTokenMs,
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

func isStreamingRequestContent(content string) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return false
	}
	return payload.Stream
}

// tokenUsageCallback is set by SetTokenUsageCallback to notify external
// rate limiters (e.g. quota middleware) of token consumption.
var tokenUsageCallback func(apiKey string, totalTokens int64)

// SetTokenUsageCallback registers a function to be called after each
// request's tokens are recorded. Used by the quota middleware for TPM tracking.
func SetTokenUsageCallback(fn func(apiKey string, totalTokens int64)) {
	tokenUsageCallback = fn
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

// getReadDB returns the dedicated read-only connection pool used by management
// read paths (QueryLogs/QueryStats/QueryFilters/content queries) so they do not
// block on the single writer or maintenance ops. It falls back to the write
// handle when no read pool is available, preserving correctness.
func getReadDB() *sql.DB {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	if usageReadDB != nil {
		return usageReadDB
	}
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

// GetDBPath returns the file path of the SQLite database, or empty if not initialised.
func GetDBPath() string {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	return usageDBPath
}
