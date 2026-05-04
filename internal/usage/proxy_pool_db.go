package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

const createProxyPoolTableSQL = `
CREATE TABLE IF NOT EXISTS proxy_pool (
  id          TEXT PRIMARY KEY NOT NULL,
  name        TEXT NOT NULL DEFAULT '',
  url         TEXT NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT '',
  updated_at  TEXT NOT NULL DEFAULT ''
);
`

func initProxyPoolTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createProxyPoolTableSQL); err != nil {
		log.Errorf("usage: create proxy_pool table: %v", err)
	}
}

// ProxyPoolStoreAvailable reports whether the SQLite store is ready for proxy-pool operations.
func ProxyPoolStoreAvailable() bool {
	return getDB() != nil
}

// ListProxyPool retrieves all reusable proxies from SQLite.
func ListProxyPool() []config.ProxyPoolEntry {
	db := getDB()
	if db == nil {
		return nil
	}

	rows, err := db.Query(`SELECT id, name, url, enabled, description FROM proxy_pool ORDER BY created_at ASC, id ASC`)
	if err != nil {
		log.Errorf("usage: list proxy_pool: %v", err)
		return nil
	}
	defer rows.Close()

	entries := make([]config.ProxyPoolEntry, 0)
	for rows.Next() {
		entry, ok := scanProxyPoolEntry(rows)
		if ok {
			entries = append(entries, entry)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warnf("usage: scan proxy_pool rows: %v", err)
	}
	return entries
}

// GetProxyPoolEntry retrieves one reusable proxy by ID.
func GetProxyPoolEntry(id string) *config.ProxyPoolEntry {
	db := getDB()
	if db == nil {
		return nil
	}

	normalizedID := normalizeProxyPoolEntryID(id)
	if normalizedID == "" {
		return nil
	}
	row := db.QueryRow(`SELECT id, name, url, enabled, description FROM proxy_pool WHERE id = ?`, normalizedID)
	entry, ok := scanProxyPoolEntry(row)
	if !ok {
		return nil
	}
	return &entry
}

// ReplaceProxyPool atomically replaces all SQLite proxy entries after normalization.
func ReplaceProxyPool(entries []config.ProxyPoolEntry) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("database not initialised")
	}

	normalized := config.NormalizeProxyPool(entries)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM proxy_pool"); err != nil {
		_ = tx.Rollback()
		return err
	}
	if len(normalized) == 0 {
		return tx.Commit()
	}

	stmt, err := tx.Prepare(`INSERT INTO proxy_pool
		(id, name, url, enabled, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, entry := range normalized {
		enabledInt := 0
		if entry.Enabled {
			enabledInt = 1
		}
		if _, err := stmt.Exec(entry.ID, entry.Name, entry.URL, enabledInt, entry.Description, now, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ApplyStoredProxyPool overlays the DB-backed proxy pool onto the runtime config.
func ApplyStoredProxyPool(cfg *config.Config) bool {
	if cfg == nil || !ProxyPoolStoreAvailable() {
		return false
	}
	cfg.ProxyPool = ListProxyPool()
	return true
}

// MigrateProxyPoolFromConfig moves legacy YAML proxy-pool entries into SQLite.
func MigrateProxyPoolFromConfig(cfg *config.Config, configFilePath string) int {
	if cfg == nil || !ProxyPoolStoreAvailable() {
		return 0
	}
	if len(ListProxyPool()) > 0 {
		cfg.ProxyPool = nil
		cleanProxyPoolFromYAML(configFilePath)
		return 0
	}
	if len(cfg.ProxyPool) == 0 {
		return 0
	}

	normalized := config.NormalizeProxyPool(cfg.ProxyPool)
	if len(normalized) == 0 {
		cfg.ProxyPool = nil
		if backupConfigForMigration(configFilePath, proxyPoolMigrationBackupSuffix) {
			cleanProxyPoolFromYAML(configFilePath)
		}
		return 0
	}

	if err := ReplaceProxyPool(normalized); err != nil {
		log.Errorf("usage: migrate proxy_pool: %v", err)
		return 0
	}
	cfg.ProxyPool = nil
	if configFilePath != "" {
		if backupConfigForMigration(configFilePath, proxyPoolMigrationBackupSuffix) {
			cleanProxyPoolFromYAML(configFilePath)
		}
	}
	log.Infof("usage: migrated %d proxy_pool entries from config to SQLite", len(normalized))
	return len(normalized)
}

type proxyPoolScanner interface {
	Scan(dest ...any) error
}

func scanProxyPoolEntry(scanner proxyPoolScanner) (config.ProxyPoolEntry, bool) {
	var entry config.ProxyPoolEntry
	var enabledInt int
	if err := scanner.Scan(&entry.ID, &entry.Name, &entry.URL, &enabledInt, &entry.Description); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("usage: scan proxy_pool row: %v", err)
		}
		return config.ProxyPoolEntry{}, false
	}
	entry.Enabled = enabledInt != 0
	return entry, true
}

func normalizeProxyPoolEntryID(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
