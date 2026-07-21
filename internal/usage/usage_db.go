package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const cacheRateEffectiveInputSQL = "CASE WHEN cached_tokens > input_tokens THEN input_tokens + cached_tokens ELSE input_tokens END"

func cacheRateFromTokenTotals(effectiveInputTokens, cachedTokens int64) float64 {
	if effectiveInputTokens <= 0 {
		return 0
	}
	return float64(cachedTokens) / float64(effectiveInputTokens) * 100
}

type ClearRequestLogsOptions struct {
	TenantID            string `json:"-"`
	ClearBodyContent    bool   `json:"clear_body_content"`
	ClearDetailContent  bool   `json:"clear_detail_content"`
	ClearRequestRecords bool   `json:"clear_request_records"`
}

const systemRequestLogFilterValue = "__system__"

var (
	usageDBLifecycleMu sync.Mutex
	usageDB            *sql.DB
	usageReadDB        *sql.DB
	usageDBMu          sync.Mutex
	usageDBPath        string
	usageDriver        string
	usageLoc           *time.Location
)

// DatabaseStats summarizes the active runtime database for management telemetry.
type DatabaseStats struct {
	Driver    string
	SizeBytes int64
}

const createTableSQL = `
CREATE TABLE IF NOT EXISTS request_logs (
  tenant_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  timestamp        DATETIME NOT NULL,
  api_key          TEXT NOT NULL DEFAULT '',
  api_key_id       TEXT NOT NULL DEFAULT '',
  auth_subject_id  TEXT NOT NULL DEFAULT '',
  model            TEXT NOT NULL DEFAULT '',
  upstream_model   TEXT NOT NULL DEFAULT '',
  vision_fallback_model TEXT NOT NULL DEFAULT '',
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
  tenant_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  log_id           INTEGER PRIMARY KEY,
  timestamp        DATETIME NOT NULL,
  compression      TEXT NOT NULL DEFAULT 'zstd',
  input_content    BLOB NOT NULL DEFAULT X'',
  output_content   BLOB NOT NULL DEFAULT X'',
  detail_content   BLOB NOT NULL DEFAULT X'',
  session_id       TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(log_id) REFERENCES request_logs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON request_logs(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_logs_api_key ON request_logs(api_key);
CREATE INDEX IF NOT EXISTS idx_logs_api_key_timestamp ON request_logs(api_key, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(model);
CREATE INDEX IF NOT EXISTS idx_logs_failed ON request_logs(failed);
CREATE INDEX IF NOT EXISTS idx_logs_auth_index ON request_logs(auth_index);
CREATE INDEX IF NOT EXISTS idx_log_content_timestamp ON request_log_content(timestamp DESC);

CREATE TABLE IF NOT EXISTS auth_file_quota_snapshots (
	  tenant_id    TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  date_key      TEXT NOT NULL,
  auth_index    TEXT NOT NULL,
  auth_subject_id TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT '',
  quota_key     TEXT NOT NULL,
  percent       REAL,
  recorded_at   DATETIME NOT NULL,
	  PRIMARY KEY (tenant_id, date_key, auth_index, quota_key)
);

CREATE INDEX IF NOT EXISTS idx_quota_snapshots_date ON auth_file_quota_snapshots(date_key);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_auth ON auth_file_quota_snapshots(auth_index);

CREATE TABLE IF NOT EXISTS auth_file_quota_snapshot_points (
	  tenant_id      TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
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
	  tenant_id       TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  subject_id       TEXT NOT NULL,
  auth_index       TEXT NOT NULL DEFAULT '',
  provider         TEXT NOT NULL DEFAULT '',
  quota_key        TEXT NOT NULL,
  cycle_start_at   DATETIME NOT NULL,
  reset_at         DATETIME NOT NULL,
  window_seconds   INTEGER NOT NULL DEFAULT 0,
  last_verified_at DATETIME NOT NULL,
	  PRIMARY KEY (tenant_id, subject_id, quota_key)
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

func migrateUpstreamModelColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN upstream_model TEXT NOT NULL DEFAULT ''")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column upstream_model: %v", err)
		}
	}
}

func migrateVisionFallbackModelColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_logs ADD COLUMN vision_fallback_model TEXT NOT NULL DEFAULT ''")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column vision_fallback_model: %v", err)
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

func ensureRequestLogLookupIndexes(db *sql.DB) {
	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS idx_logs_api_key_timestamp ON request_logs(api_key, timestamp DESC)",
		"CREATE INDEX IF NOT EXISTS idx_logs_api_key_id_timestamp ON request_logs(api_key_id, timestamp DESC)",
		"CREATE INDEX IF NOT EXISTS idx_logs_api_key_chart_cover ON request_logs(api_key, api_key_id, timestamp DESC, model, failed, input_tokens, output_tokens, total_tokens, cost, cached_tokens)",
		"CREATE INDEX IF NOT EXISTS idx_logs_api_key_id_chart_cover ON request_logs(api_key_id, timestamp DESC, model, failed, input_tokens, output_tokens, total_tokens, cost, cached_tokens)",
		// AI Accounts entity-stats / auth-file-trend: tenant + auth identity + time (SQLite-only path).
		"CREATE INDEX IF NOT EXISTS idx_logs_tenant_auth_index_timestamp ON request_logs(tenant_id, auth_index, timestamp DESC)",
		"CREATE INDEX IF NOT EXISTS idx_logs_tenant_auth_subject_timestamp ON request_logs(tenant_id, auth_subject_id, timestamp DESC)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			log.Warnf("usage: create request log lookup index: %v", err)
		}
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
		  tenant_id       TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
		  subject_id       TEXT NOT NULL,
		  auth_index       TEXT NOT NULL DEFAULT '',
		  provider         TEXT NOT NULL DEFAULT '',
		  quota_key        TEXT NOT NULL,
		  cycle_start_at   DATETIME NOT NULL,
		  reset_at         DATETIME NOT NULL,
		  window_seconds   INTEGER NOT NULL DEFAULT 0,
		  last_verified_at DATETIME NOT NULL,
		  PRIMARY KEY (tenant_id, subject_id, quota_key)
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

func migrateRequestLogTenantColumns(db *sql.DB) {
	for _, table := range []string{"request_logs", "request_log_content"} {
		_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'")
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			log.Warnf("usage: migrate %s tenant_id: %v", table, err)
		}
	}
	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS idx_logs_tenant_timestamp ON request_logs(tenant_id, timestamp DESC)",
		"CREATE INDEX IF NOT EXISTS idx_logs_tenant_api_key_timestamp ON request_logs(tenant_id, api_key_id, timestamp DESC)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			log.Warnf("usage: create tenant request log index: %v", err)
		}
	}
}

func migrateQuotaTenantColumns(db *sql.DB) {
	for _, table := range []string{"auth_file_quota_snapshots", "auth_file_quota_snapshot_points", "auth_subject_quota_cycles"} {
		_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'")
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			log.Warnf("usage: migrate %s tenant_id: %v", table, err)
		}
	}
	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS idx_quota_snapshots_tenant_date ON auth_file_quota_snapshots(tenant_id, date_key)",
		"CREATE INDEX IF NOT EXISTS idx_quota_points_tenant_auth_time ON auth_file_quota_snapshot_points(tenant_id, auth_index, recorded_at)",
		"CREATE INDEX IF NOT EXISTS idx_quota_cycles_tenant_subject ON auth_subject_quota_cycles(tenant_id, subject_id, last_verified_at)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			log.Warnf("usage: create tenant quota index: %v", err)
		}
	}
}

func sqlitePrimaryKeyColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type column struct {
		name string
		pk   int
	}
	var columns []column
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		if pk > 0 {
			columns = append(columns, column{name: name, pk: pk})
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(columns, func(i, j int) bool { return columns[i].pk < columns[j].pk })
	result := make([]string, 0, len(columns))
	for _, item := range columns {
		result = append(result, item.name)
	}
	return result, nil
}

func migrateQuotaTenantPrimaryKeys(db *sql.DB) {
	for _, migration := range []struct {
		table   string
		wantPK  []string
		create  string
		columns string
		indexes []string
	}{
		{
			table:  "auth_file_quota_snapshots",
			wantPK: []string{"tenant_id", "date_key", "auth_index", "quota_key"},
			create: `CREATE TABLE auth_file_quota_snapshots_tenant_migration (
				tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
				date_key TEXT NOT NULL, auth_index TEXT NOT NULL, auth_subject_id TEXT NOT NULL DEFAULT '',
				provider TEXT NOT NULL DEFAULT '', quota_key TEXT NOT NULL, percent REAL,
				recorded_at DATETIME NOT NULL, PRIMARY KEY (tenant_id, date_key, auth_index, quota_key))`,
			columns: "tenant_id,date_key,auth_index,auth_subject_id,provider,quota_key,percent,recorded_at",
			indexes: []string{
				"CREATE INDEX IF NOT EXISTS idx_quota_snapshots_date ON auth_file_quota_snapshots(date_key)",
				"CREATE INDEX IF NOT EXISTS idx_quota_snapshots_auth ON auth_file_quota_snapshots(auth_index)",
				"CREATE INDEX IF NOT EXISTS idx_quota_snapshots_subject ON auth_file_quota_snapshots(auth_subject_id)",
				"CREATE INDEX IF NOT EXISTS idx_quota_snapshots_tenant_date ON auth_file_quota_snapshots(tenant_id, date_key)",
			},
		},
		{
			table:  "auth_subject_quota_cycles",
			wantPK: []string{"tenant_id", "subject_id", "quota_key"},
			create: `CREATE TABLE auth_subject_quota_cycles_tenant_migration (
				tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
				subject_id TEXT NOT NULL, auth_index TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
				quota_key TEXT NOT NULL, cycle_start_at DATETIME NOT NULL, reset_at DATETIME NOT NULL,
				window_seconds INTEGER NOT NULL DEFAULT 0, last_verified_at DATETIME NOT NULL,
				PRIMARY KEY (tenant_id, subject_id, quota_key))`,
			columns: "tenant_id,subject_id,auth_index,provider,quota_key,cycle_start_at,reset_at,window_seconds,last_verified_at",
			indexes: []string{
				"CREATE INDEX IF NOT EXISTS idx_auth_subject_quota_cycles_subject_window ON auth_subject_quota_cycles(subject_id, window_seconds, last_verified_at)",
				"CREATE INDEX IF NOT EXISTS idx_quota_cycles_tenant_subject ON auth_subject_quota_cycles(tenant_id, subject_id, last_verified_at)",
			},
		},
	} {
		currentPK, err := sqlitePrimaryKeyColumns(db, migration.table)
		if err != nil {
			log.Warnf("usage: inspect %s primary key: %v", migration.table, err)
			continue
		}
		if slices.Equal(currentPK, migration.wantPK) {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			log.Warnf("usage: begin %s tenant primary key migration: %v", migration.table, err)
			continue
		}
		migrationTable := migration.table + "_tenant_migration"
		if _, err = tx.Exec("DROP TABLE IF EXISTS " + migrationTable); err == nil {
			_, err = tx.Exec(migration.create)
		}
		if err == nil {
			_, err = tx.Exec("INSERT INTO " + migrationTable + " (" + migration.columns + ") SELECT " + migration.columns + " FROM " + migration.table)
		}
		if err == nil {
			_, err = tx.Exec("DROP TABLE " + migration.table)
		}
		if err == nil {
			_, err = tx.Exec("ALTER TABLE " + migrationTable + " RENAME TO " + migration.table)
		}
		for _, indexSQL := range migration.indexes {
			if err == nil {
				_, err = tx.Exec(indexSQL)
			}
		}
		if err != nil {
			_ = tx.Rollback()
			log.Warnf("usage: migrate %s tenant primary key: %v", migration.table, err)
			continue
		}
		if err = tx.Commit(); err != nil {
			log.Warnf("usage: commit %s tenant primary key migration: %v", migration.table, err)
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

func migrateRequestLogContentSessionIDColumn(db *sql.DB) {
	_, err := db.Exec("ALTER TABLE request_log_content ADD COLUMN session_id TEXT NOT NULL DEFAULT ''")
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate") {
			log.Warnf("usage: migrate column session_id: %v", err)
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
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_log_content_session_timestamp
		ON request_log_content(session_id, timestamp DESC)
		WHERE session_id <> ''
	`); err != nil {
		log.Warnf("usage: create idx_log_content_session_timestamp: %v", err)
	}
}

const (
	requestLogSessionBackfillMaxRows        = 50
	requestLogSessionBackfillMaxContentSize = 1 << 20
)

func backfillRequestLogContentSessionIDs(db *sql.DB) {
	rows, err := db.Query(`
		SELECT log_id, compression, detail_content
		  FROM request_log_content
		 WHERE session_id = ''
		   AND length(detail_content) > 0
		   AND length(detail_content) <= ?
		 ORDER BY timestamp DESC
		 LIMIT ?
	`, requestLogSessionBackfillMaxContentSize, requestLogSessionBackfillMaxRows)
	if err != nil {
		log.Warnf("usage: query request log session_id backfill rows: %v", err)
		return
	}

	type update struct {
		logID     int64
		sessionID string
	}
	updates := make([]update, 0)
	var scanned int64
	for rows.Next() {
		var logID int64
		var compression string
		var compressed []byte
		if err := rows.Scan(&logID, &compression, &compressed); err != nil {
			_ = rows.Close()
			log.Warnf("usage: scan request log session_id backfill row: %v", err)
			return
		}
		scanned++
		detail, err := decompressLogContent(compression, compressed)
		if err != nil {
			log.Warnf("usage: decompress request log session_id backfill row %d: %v", logID, err)
			continue
		}
		if sessionID := extractSessionIDFromDetails(detail); sessionID != "" {
			updates = append(updates, update{logID: logID, sessionID: sessionID})
		}
	}
	if err := rows.Close(); err != nil {
		log.Warnf("usage: close request log session_id backfill rows: %v", err)
		return
	}
	if err := rows.Err(); err != nil {
		log.Warnf("usage: iterate request log session_id backfill rows: %v", err)
		return
	}
	if len(updates) == 0 {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Warnf("usage: begin request log session_id backfill: %v", err)
		return
	}
	stmt, err := tx.Prepare("UPDATE request_log_content SET session_id = ? WHERE log_id = ?")
	if err != nil {
		_ = tx.Rollback()
		log.Warnf("usage: prepare request log session_id backfill: %v", err)
		return
	}
	for _, update := range updates {
		if _, err := stmt.Exec(update.sessionID, update.logID); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			log.Warnf("usage: update request log session_id backfill row %d: %v", update.logID, err)
			return
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		log.Warnf("usage: close request log session_id backfill statement: %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Warnf("usage: commit request log session_id backfill: %v", err)
		return
	}
	log.Infof("usage: backfilled request log session_id values: %d/%d", len(updates), scanned)
}

func startRequestLogContentSessionIDBackfill(db *sql.DB) {
	if db == nil {
		return
	}
	// Historical detail_content can be very large; production databases have
	// reached gigabytes here. New writes already populate session_id inline, so
	// startup must not read/decompress old rows on the request-serving process.
}

// InitDB opens (or creates) the SQLite database at the given path and creates
// the request_logs table if it doesn't exist.
func InitDB(dbPath string, storageCfg config.RequestLogStorageConfig, loc *time.Location) error {
	usageDBLifecycleMu.Lock()
	defer usageDBLifecycleMu.Unlock()

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

	return initOpenedDBLocked(db, readDB, dbPath, "sqlite", storageCfg, loc, true)
}

const (
	postgresMigrationTimeoutEnv     = "CLIRELAY_MIGRATION_TIMEOUT"
	defaultPostgresMigrationTimeout = 30 * time.Second
)

func postgresMigrationTimeout() (time.Duration, error) {
	raw, ok := os.LookupEnv(postgresMigrationTimeoutEnv)
	if !ok {
		return defaultPostgresMigrationTimeout, nil
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("usage: invalid %s %q: %w", postgresMigrationTimeoutEnv, raw, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("usage: invalid %s %q: duration must be greater than zero", postgresMigrationTimeoutEnv, raw)
	}
	return timeout, nil
}

func InitPostgres(pgCfg config.PostgresConfig, storageCfg config.RequestLogStorageConfig, loc *time.Location) error {
	migrationTimeout, err := postgresMigrationTimeout()
	if err != nil {
		return err
	}

	usageDBLifecycleMu.Lock()
	defer usageDBLifecycleMu.Unlock()

	usageDBMu.Lock()
	defer usageDBMu.Unlock()

	if usageDB != nil {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}
	db, err := postgresstore.OpenRuntimeDBWithMigrationTimeout(context.Background(), pgCfg, migrationTimeout)
	if err != nil {
		return err
	}
	return initOpenedDBLocked(db, db, "postgres", "postgres", storageCfg, loc, false)
}

func initOpenedDBLocked(db, readDB *sql.DB, dbPath, driver string, storageCfg config.RequestLogStorageConfig, loc *time.Location, runSQLiteBootstrap bool) error {
	if loc == nil {
		loc = time.Local
	}
	usageLoc = loc

	log.Debugf("usage: creating tables")
	if runSQLiteBootstrap {
		if _, err := db.Exec(createTableSQL); err != nil {
			_ = db.Close()
			return fmt.Errorf("usage: create table: %w", err)
		}
	}
	usageDB = db
	usageReadDB = readDB
	usageDBPath = dbPath
	usageDriver = driver
	ApplyRequestLogStorageConfig(storageCfg)
	if runSQLiteBootstrap {
		log.Debugf("usage: running tenant scope column migration")
		migrateRequestLogTenantColumns(db)
		log.Debugf("usage: running quota tenant scope column migration")
		migrateQuotaTenantColumns(db)
		log.Debugf("usage: running content column migration")
		migrateContentColumns(db)
		log.Debugf("usage: running cost column migration")
		migrateCostColumn(db)
		log.Debugf("usage: running api_key_name column migration")
		migrateApiKeyNameColumn(db)
		log.Debugf("usage: running upstream_model column migration")
		migrateUpstreamModelColumn(db)
		log.Debugf("usage: running vision_fallback_model column migration")
		migrateVisionFallbackModelColumn(db)
		log.Debugf("usage: running api_key_id column migration")
		migrateAPIKeyIDColumn(db)
		log.Debugf("usage: ensuring request log lookup indexes")
		ensureRequestLogLookupIndexes(db)
		log.Debugf("usage: running auth_subject_id column migration")
		migrateAuthSubjectIDColumns(db)
		log.Debugf("usage: rebuilding quota tenant primary keys")
		migrateQuotaTenantPrimaryKeys(db)
		log.Debugf("usage: running first_token_ms column migration")
		migrateFirstTokenColumn(db)
		log.Debugf("usage: running streaming column migration")
		migrateStreamingColumn(db)
		log.Debugf("usage: running request log detail column migration")
		migrateRequestLogDetailColumn(db)
		log.Debugf("usage: running request log content session_id column migration")
		migrateRequestLogContentSessionIDColumn(db)
		log.Debugf("usage: ensuring request log detail indexes")
		ensureRequestLogDetailIndexes(db)
	}
	bootstrapAIAccountStatusReadModels(db, loc)
	if err := ensureAIAccountSharedSubjectTables(db); err != nil {
		_ = db.Close()
		usageDB, usageReadDB = nil, nil
		return err
	}
	if err := loadAIAccountSubjectCycleCache(db); err != nil {
		_ = db.Close()
		usageDB, usageReadDB = nil, nil
		return fmt.Errorf("usage: load shared ai account cycles: %w", err)
	}
	if err := bootstrapAPIKeyDailySpendingResets(db); err != nil {
		_ = db.Close()
		usageDB, usageReadDB = nil, nil
		return err
	}
	if err := bootstrapAPIKeyDailySpendingResetEvents(db); err != nil {
		_ = db.Close()
		usageDB, usageReadDB = nil, nil
		return err
	}
	if err := bootstrapEndUserDailySpendingResets(db); err != nil {
		_ = db.Close()
		usageDB, usageReadDB = nil, nil
		return err
	}
	if err := bootstrapEndUserDailySpendingResetEvents(db); err != nil {
		_ = db.Close()
		usageDB, usageReadDB = nil, nil
		return err
	}
	log.Debugf("usage: initializing pricing table")
	initPricingTable(db)
	log.Debugf("usage: initializing model config tables")
	initModelConfigTables(db)
	log.Debugf("usage: initializing api_keys table")
	initAPIKeysTable(db)
	log.Debugf("usage: backfilling request log api_key_id values")
	backfillRequestLogAPIKeyIDs(db)
	// Rollup backfill after api_keys + api_key_id repair so historical rows resolve
	// stable key/end-user dimensions before marker is set.
	if err := bootstrapUsageRollup(db, loc); err != nil {
		_ = db.Close()
		usageDB, usageReadDB = nil, nil
		return fmt.Errorf("usage: bootstrap usage rollup: %w", err)
	}
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
	log.Debugf("usage: initializing identity_fingerprints table")
	initIdentityFingerprintsTable(db)
	startRequestLogMaintenance(db, driver)
	startRequestLogContentSessionIDBackfill(db)
	log.Infof("usage: %s database initialised at %s", driver, dbPath)
	return nil
}

// CloseDB closes the runtime database gracefully.
func CloseDB() {
	usageDBLifecycleMu.Lock()
	defer usageDBLifecycleMu.Unlock()

	// Maintenance and in-flight SQLite writes can read usageLoc under usageDBMu.
	// Stop the worker, then detach the handles under the mutex before waiting for
	// database/sql to drain them, otherwise shutdown can deadlock on that mutex.
	stopRequestLogMaintenance()

	usageDBMu.Lock()
	db := usageDB
	readDB := usageReadDB
	usageDB = nil
	usageReadDB = nil
	usageDBMu.Unlock()

	if db != nil {
		_ = db.Close()
	}
	if readDB != nil && readDB != db {
		_ = readDB.Close()
	}

	usageDBMu.Lock()
	usageLoc = nil
	usageDriver = ""
	usageDBMu.Unlock()
	resetAIAccountSubjectCycleCache()
	log.Info("usage: database closed")
}

// InsertLog writes a single request log entry into the runtime database.
// It is safe to call concurrently.
func InsertLog(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent string) {
	insertLogIdentity(apiKey, "", "", apiKeyName, model, "", "", source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, "", isStreamingRequestContent(inputContent))
}

func InsertLogWithDetails(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLogIdentity(apiKey, "", "", apiKeyName, model, "", "", source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent, isStreamingRequestContent(inputContent))
}

func InsertLogWithDetailsIdentity(apiKey, apiKeyID, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLogIdentity(apiKey, apiKeyID, "", apiKeyName, model, "", "", source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent, isStreamingRequestContent(inputContent))
}

func InsertLogWithDetailsIdentitySubject(apiKey, apiKeyID, authSubjectID, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLogIdentity(apiKey, apiKeyID, authSubjectID, apiKeyName, model, "", "", source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent, isStreamingRequestContent(inputContent))
}

func InsertLogWithDetailsIdentitySubjectUpstream(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLogIdentity(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, "", source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent, isStreamingRequestContent(inputContent))
}

func InsertLogWithDetailsIdentitySubjectUpstreamVision(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	insertLogIdentity(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel, source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent, isStreamingRequestContent(inputContent))
}

// InsertLogWithDetailsIdentitySubjectUpstreamVisionStreaming persists an
// explicit streaming classification even when request body storage is disabled.
func InsertLogWithDetailsIdentitySubjectUpstreamVisionStreaming(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string, streaming bool) {
	insertLogIdentity(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel, source, channelName, authIndex, failed, timestamp, latencyMs, firstTokenMs, tokens, inputContent, outputContent, detailContent, streaming)
}

func insertLogIdentity(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string, streaming bool) {
	db := getDB()
	if db == nil {
		return
	}

	tenantID := normalizeTenantID(ResolveAPIKeyTenant(apiKey))

	// Calculate cost from the authenticated API key's tenant-scoped pricing catalog.
	cost := CalculateCostV2ForTenant(tenantID, model, tokens)

	apiKeyID = strings.TrimSpace(apiKeyID)
	authSubjectID = strings.TrimSpace(authSubjectID)
	apiKeyName = strings.TrimSpace(apiKeyName)
	upstreamModel = strings.TrimSpace(upstreamModel)
	visionFallbackModel = strings.TrimSpace(visionFallbackModel)
	// Resolve identity before opening the write tx: SQLite single-writer + maintenance
	// would deadlock if we query api_keys while this connection already holds a tx.
	endUserID := ""
	if row := GetAPIKey(apiKey); row != nil {
		if apiKeyID == "" {
			apiKeyID = strings.TrimSpace(row.ID)
		}
		if name := strings.TrimSpace(row.Name); name != "" {
			apiKeyName = name
		} else if apiKeyName == "" {
			apiKeyName = strings.TrimSpace(row.Name)
		}
		endUserID = strings.TrimSpace(row.EndUserID)
	}

	// Failed requests always keep a compact error payload in output_content so the
	// management UI error modal can show the upstream failure even when full body
	// storage is disabled. Successful request/response bodies still follow the
	// store-content toggle.
	shouldStoreContent := detailContent != "" ||
		(RequestLogBodyStorageEnabled() && (inputContent != "" || outputContent != "")) ||
		(failed && strings.TrimSpace(outputContent) != "")

	// Retry on Postgres rollup UPSERT deadlocks under concurrent same-key traffic.
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * time.Millisecond)
		}
		lastErr = insertLogIdentityOnce(
			db, tenantID, apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel,
			source, channelName, authIndex, endUserID, failed, streaming, timestamp, latencyMs, firstTokenMs,
			tokens, cost, inputContent, outputContent, detailContent, shouldStoreContent,
		)
		if lastErr == nil {
			break
		}
		if !isRetryableUsageWriteErr(lastErr) {
			log.Errorf("usage: insert log: %v", lastErr)
			return
		}
	}
	if lastErr != nil {
		log.Errorf("usage: insert log after retries: %v", lastErr)
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

type storedTime struct {
	Time  time.Time
	Valid bool
}

func (t *storedTime) Scan(value any) error {
	parsed, ok := parseStoredTimeValue(value)
	t.Time = parsed
	t.Valid = ok
	return nil
}

func parseStoredTime(value string) (time.Time, bool) {
	return parseStoredTimeValue(value)
}

func parseStoredTimeValue(value any) (time.Time, bool) {
	switch v := value.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		return v.UTC(), true
	case string:
		return parseStoredTimeString(v)
	case []byte:
		return parseStoredTimeString(string(v))
	default:
		return time.Time{}, false
	}
}

func parseStoredTimeString(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	// Include Postgres text forms of timestamptz (space separator, +00 / -07 offsets).
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
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

func currentUsageDriver() string {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	return usageDriver
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

// GetDBPath returns the SQLite database file path, or empty for non-file databases.
func GetDBPath() string {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	if usageDriver != "sqlite" {
		return ""
	}
	return usageDBPath
}

// GetDatabaseStats returns runtime database engine and size telemetry.
func GetDatabaseStats() (DatabaseStats, error) {
	usageDBMu.Lock()
	driver := usageDriver
	path := usageDBPath
	db := usageReadDB
	if db == nil {
		db = usageDB
	}
	usageDBMu.Unlock()

	stats := DatabaseStats{Driver: driver}
	switch driver {
	case "postgres":
		if db == nil {
			return stats, nil
		}
		var size sql.NullInt64
		if err := db.QueryRow("SELECT pg_database_size(current_database())").Scan(&size); err != nil {
			return stats, fmt.Errorf("usage: query postgres database size: %w", err)
		}
		if size.Valid {
			stats.SizeBytes = size.Int64
		}
	case "sqlite":
		size, err := sqliteDatabaseSizeBytes(path)
		if err != nil {
			return stats, err
		}
		stats.SizeBytes = size
	}
	return stats, nil
}

func sqliteDatabaseSizeBytes(path string) (int64, error) {
	if strings.TrimSpace(path) == "" {
		return 0, nil
	}
	var size int64
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(candidate)
		if err == nil {
			size += info.Size()
			continue
		}
		if !os.IsNotExist(err) {
			return 0, fmt.Errorf("usage: stat sqlite database file %s: %w", candidate, err)
		}
	}
	return size, nil
}

// RuntimeDB returns the initialized runtime database handle for internal services
// that share PostgreSQL as the source of truth.
func RuntimeDB() *sql.DB {
	return getDB()
}
