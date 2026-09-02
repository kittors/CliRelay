package proxypool

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

const createProxyPoolTableSQL = `
CREATE TABLE IF NOT EXISTS proxy_pool (
  tenant_id   TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  id          TEXT NOT NULL,
  name        TEXT NOT NULL DEFAULT '',
  url         TEXT NOT NULL,
  enabled     INTEGER NOT NULL DEFAULT 1,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT '',
  updated_at  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id)
);
`

type Store struct {
	db       *sql.DB
	tenantID string
}

var ErrEntryNotFound = errors.New("proxy pool entry not found")

type scanner interface {
	Scan(dest ...any) error
}

func NewStore(db *sql.DB) Store {
	return NewTenantStore(db, "")
}

func NewTenantStore(db *sql.DB, tenantID string) Store {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = "00000000-0000-0000-0000-000000000001"
	}
	return Store{db: db, tenantID: tenantID}
}

func InitTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createProxyPoolTableSQL); err != nil {
		log.Errorf("sqlite/proxypool: create proxy_pool table: %v", err)
	}
	migrateTenantSchema(db)
}

func (s Store) Available() bool {
	return s.db != nil
}

func (s Store) List() []config.ProxyPoolEntry {
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query(`SELECT id, name, url, enabled, description FROM proxy_pool WHERE tenant_id = ? ORDER BY created_at ASC, id ASC`, s.tenantID)
	if err != nil {
		log.Errorf("sqlite/proxypool: list proxy_pool: %v", err)
		return nil
	}
	defer rows.Close()

	entries := make([]config.ProxyPoolEntry, 0)
	for rows.Next() {
		entry, ok := scanEntry(rows)
		if ok {
			entries = append(entries, entry)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warnf("sqlite/proxypool: scan proxy_pool rows: %v", err)
	}
	return entries
}

func (s Store) Get(id string) *config.ProxyPoolEntry {
	if s.db == nil {
		return nil
	}

	normalizedID := config.NormalizeProxyID(id)
	if normalizedID == "" {
		return nil
	}
	row := s.db.QueryRow(`SELECT id, name, url, enabled, description FROM proxy_pool WHERE tenant_id = ? AND id = ?`, s.tenantID, normalizedID)
	entry, ok := scanEntry(row)
	if !ok {
		return nil
	}
	return &entry
}

func (s Store) Replace(entries []config.ProxyPoolEntry) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}

	normalized := config.NormalizeProxyPool(entries)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM proxy_pool WHERE tenant_id = ?", s.tenantID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if len(normalized) == 0 {
		return tx.Commit()
	}

	stmt, err := tx.Prepare(`INSERT INTO proxy_pool
		(tenant_id, id, name, url, enabled, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
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
		if _, err := stmt.Exec(s.tenantID, entry.ID, entry.Name, entry.URL, enabledInt, entry.Description, now, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s Store) Update(id string, entry config.ProxyPoolEntry) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}

	normalizedID := config.NormalizeProxyID(id)
	if normalizedID == "" {
		return ErrEntryNotFound
	}

	enabledInt := 0
	if entry.Enabled {
		enabledInt = 1
	}
	result, err := s.db.Exec(
		`UPDATE proxy_pool
		 SET name = ?, url = ?, enabled = ?, description = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ?`,
		entry.Name,
		entry.URL,
		enabledInt,
		entry.Description,
		time.Now().UTC().Format(time.RFC3339),
		s.tenantID,
		normalizedID,
	)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrEntryNotFound
	}
	return nil
}

func (s Store) ApplyToConfig(cfg *config.Config) bool {
	if s.db == nil || cfg == nil {
		return false
	}
	cfg.ProxyPool = s.List()
	return true
}

func (s Store) MigrateFromConfig(cfg *config.Config) (migrated int, hadStored bool, shouldClean bool) {
	if s.db == nil || cfg == nil {
		return 0, false, false
	}
	if len(s.List()) > 0 {
		cfg.ProxyPool = nil
		return 0, true, true
	}
	if len(cfg.ProxyPool) == 0 {
		return 0, false, false
	}

	normalized := config.NormalizeProxyPool(cfg.ProxyPool)
	cfg.ProxyPool = nil
	if len(normalized) == 0 {
		return 0, false, true
	}
	if err := s.Replace(normalized); err != nil {
		log.Errorf("sqlite/proxypool: migrate proxy_pool: %v", err)
		return 0, false, false
	}
	log.Infof("sqlite/proxypool: migrated %d proxy_pool entries from config to the database", len(normalized))
	return len(normalized), false, true
}

func scanEntry(scanner scanner) (config.ProxyPoolEntry, bool) {
	var entry config.ProxyPoolEntry
	var enabledInt int
	if err := scanner.Scan(&entry.ID, &entry.Name, &entry.URL, &enabledInt, &entry.Description); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("sqlite/proxypool: scan proxy_pool row: %v", err)
		}
		return config.ProxyPoolEntry{}, false
	}
	entry.Enabled = enabledInt != 0
	return entry, true
}

func migrateTenantSchema(db *sql.DB) {
	rows, err := db.Query("PRAGMA table_info(proxy_pool)")
	if err != nil {
		return
	}
	defer rows.Close()
	hasTenant, tenantPrimary, idPrimary := false, false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def sql.NullString
		if rows.Scan(&cid, &name, &typ, &notNull, &def, &pk) != nil {
			return
		}
		hasTenant = hasTenant || name == "tenant_id"
		tenantPrimary = tenantPrimary || (name == "tenant_id" && pk > 0)
		idPrimary = idPrimary || (name == "id" && pk > 0)
	}
	_ = rows.Close()
	if hasTenant && tenantPrimary && idPrimary {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if !hasTenant {
		if _, err = tx.Exec("ALTER TABLE proxy_pool ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'"); err != nil {
			return
		}
	}
	if _, err = tx.Exec("ALTER TABLE proxy_pool RENAME TO proxy_pool_legacy"); err != nil {
		return
	}
	if _, err = tx.Exec(createProxyPoolTableSQL); err != nil {
		return
	}
	if _, err = tx.Exec("INSERT INTO proxy_pool(tenant_id,id,name,url,enabled,description,created_at,updated_at) SELECT tenant_id,id,name,url,enabled,description,created_at,updated_at FROM proxy_pool_legacy"); err != nil {
		return
	}
	if _, err = tx.Exec("DROP TABLE proxy_pool_legacy"); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		log.Warnf("sqlite/proxypool: migrate tenant schema: %v", err)
	}
}
