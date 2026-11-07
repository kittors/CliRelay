package proxypool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
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

// systemTenantID mirrors identity.SystemTenantID, which this storage package
// does not import.
const systemTenantID = "00000000-0000-0000-0000-000000000001"

func NewTenantStore(db *sql.DB, tenantID string) Store {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = systemTenantID
	}
	return Store{db: db, tenantID: tenantID}
}

func InitTable(db *sql.DB) {
	if db == nil {
		return
	}
	// Whole-replace writes of this table bump its collection version.
	configsync.InitTables(db)
	// Replacing the system tenant's pool also drops grants of removed entries.
	resourcegrants.InitTable(db)
	if _, err := db.Exec(createProxyPoolTableSQL); err != nil {
		log.Errorf("sqlite/proxypool: create proxy_pool table: %v", err)
	}
	migrateTenantSchema(db)
}

func (s Store) Available() bool {
	return s.db != nil
}

const listProxyPoolSQL = `SELECT id, name, url, enabled, description FROM proxy_pool WHERE tenant_id = ? ORDER BY created_at ASC, id ASC`

func (s Store) List() []config.ProxyPoolEntry {
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query(listProxyPoolSQL, s.tenantID)
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

// ListContext is List for callers that must tell an empty pool from one that
// could not be read: List logs a failed query and returns nothing, and skips
// rows it cannot scan, where ListContext returns the error.
func (s Store) ListContext(ctx context.Context) ([]config.ProxyPoolEntry, error) {
	if s.db == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx, listProxyPoolSQL, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("list proxy_pool: %w", err)
	}
	defer rows.Close()
	entries := make([]config.ProxyPoolEntry, 0)
	for rows.Next() {
		entry, err := scanEntryErr(rows)
		if err != nil {
			return nil, fmt.Errorf("scan proxy_pool row: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read proxy_pool rows: %w", err)
	}
	return entries, nil
}

// ListEnabledURLs returns the URL of every enabled entry of every tenant. It
// serves checks that must cover every proxy the deployment can send traffic
// through; the runtime config only holds the system tenant's pool. A nil db
// yields nothing.
func ListEnabledURLs(ctx context.Context, db *sql.DB) ([]string, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT url FROM proxy_pool WHERE enabled <> 0`)
	if err != nil {
		return nil, fmt.Errorf("list enabled proxy_pool urls: %w", err)
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan proxy_pool url: %w", err)
		}
		urls = append(urls, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read proxy_pool urls: %w", err)
	}
	return urls, nil
}

// ListTenantsHoldingIDs returns, for each of ids, the tenants other than the
// system tenant whose own pool has an entry with that ID. Such an entry
// overrides the platform's shared entry of the same ID for that tenant; the
// platform view lists them so the operator can see whose traffic an edit of
// the shared entry will not reach. A nil db or no ids yields nothing.
func ListTenantsHoldingIDs(ctx context.Context, db *sql.DB, ids []string) (map[string][]string, error) {
	if db == nil || len(ids) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, systemTenantID)
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, tenant_id FROM proxy_pool
		WHERE tenant_id <> ? AND id IN (`+strings.Join(placeholders, ", ")+`)
		ORDER BY id ASC, tenant_id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list proxy_pool holders: %w", err)
	}
	defer rows.Close()
	holders := make(map[string][]string)
	for rows.Next() {
		var id, tenantID string
		if err := rows.Scan(&id, &tenantID); err != nil {
			return nil, fmt.Errorf("scan proxy_pool holder: %w", err)
		}
		holders[id] = append(holders[id], tenantID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read proxy_pool holders: %w", err)
	}
	return holders, nil
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

// Replace replaces the tenant's proxy pool whatever the stored collection
// version is, as clients without versions expect.
func (s Store) Replace(entries []config.ProxyPoolEntry) error {
	_, err := s.ReplaceExpect(context.Background(), entries, configsync.AnyVersion)
	return err
}

// ReplaceExpect replaces the tenant's proxy pool if the collection is still at
// expected (configsync.AnyVersion: unchecked), announces it, and returns the
// new collection version.
func (s Store) ReplaceExpect(ctx context.Context, entries []config.ProxyPoolEntry, expected int64) (int64, error) {
	if s.db == nil {
		return 0, fmt.Errorf("database not initialised")
	}
	normalized := config.NormalizeProxyPool(entries)
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	version, err := configsync.BumpTx(ctx, tx, configsync.DomainProxyPool, s.tenantID, expected)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec("DELETE FROM proxy_pool WHERE tenant_id = ?", s.tenantID); err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	kept := make([]string, 0, len(normalized))
	for _, entry := range normalized {
		enabledInt := 0
		if entry.Enabled {
			enabledInt = 1
		}
		if _, err = tx.Exec(`INSERT INTO proxy_pool
			(tenant_id, id, name, url, enabled, description, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, s.tenantID, entry.ID, entry.Name, entry.URL, enabledInt, entry.Description, now, now); err != nil {
			return 0, err
		}
		kept = append(kept, entry.ID)
	}
	events := []cluster.ConfigEvent{configsync.KeyEvent(configsync.DomainProxyPool, s.tenantID, "", version)}
	if s.tenantID == systemTenantID {
		// Only the system tenant shares proxies. A grant must go with its entry:
		// left behind, it would share whatever entry is created later under the
		// same ID, which nobody chose to share.
		grantsChanged, errRetain := resourcegrants.RetainTx(ctx, tx, resourcegrants.KindProxy, kept)
		if errRetain != nil {
			return 0, errRetain
		}
		if grantsChanged {
			events = append(events, configsync.Event(configsync.DomainResourceGrants, ""))
		}
	}
	if err = configsync.CommitTx(ctx, tx, events...); err != nil {
		return 0, err
	}
	return version, nil
}

// Update updates one proxy entry, bumping the collection version and
// announcing the change with it.
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
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(
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
	return configsync.BumpAndCommit(ctx, tx, configsync.DomainProxyPool, s.tenantID)
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
	entry, err := scanEntryErr(scanner)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("sqlite/proxypool: scan proxy_pool row: %v", err)
		}
		return config.ProxyPoolEntry{}, false
	}
	return entry, true
}

func scanEntryErr(scanner scanner) (config.ProxyPoolEntry, error) {
	var entry config.ProxyPoolEntry
	var enabledInt int
	if err := scanner.Scan(&entry.ID, &entry.Name, &entry.URL, &enabledInt, &entry.Description); err != nil {
		return config.ProxyPoolEntry{}, err
	}
	entry.Enabled = enabledInt != 0
	return entry, nil
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
