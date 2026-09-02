package routing

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

const createRoutingConfigTableSQL = `
CREATE TABLE IF NOT EXISTS routing_config (
  tenant_id  TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  id         INTEGER NOT NULL CHECK (id = 1),
  payload    TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id)
);
`

type Store struct {
	db       *sql.DB
	tenantID string
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
	if _, err := db.Exec(createRoutingConfigTableSQL); err != nil {
		log.Errorf("sqlite/routing: create routing_config table: %v", err)
	}
	migrateTenantSchema(db)
}

func normalize(input config.RoutingConfig) config.RoutingConfig {
	holder := &config.Config{Routing: input}
	holder.SanitizeRouting()
	return holder.Routing
}

func meaningful(cfg config.RoutingConfig) bool {
	return cfg.Strategy != "" || !cfg.IncludeDefaultGroup || len(cfg.ChannelGroups) > 0 || len(cfg.PathRoutes) > 0
}

func (s Store) Get() *config.RoutingConfig {
	if s.db == nil {
		return nil
	}

	var payload string
	if err := s.db.QueryRow(`SELECT payload FROM routing_config WHERE tenant_id = ? AND id = 1`, s.tenantID).Scan(&payload); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("sqlite/routing: load routing_config: %v", err)
		}
		return nil
	}

	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}

	var cfg config.RoutingConfig
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		log.Warnf("sqlite/routing: decode routing_config: %v", err)
		return nil
	}
	normalized := normalize(cfg)
	return &normalized
}

func (s Store) Upsert(cfg config.RoutingConfig) error {
	if s.db == nil {
		return nil
	}

	normalized := normalize(cfg)
	payload, err := json.Marshal(normalized)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(
		`INSERT INTO routing_config (tenant_id, id, payload, updated_at)
		 VALUES (?, 1, ?, ?)
		 ON CONFLICT(tenant_id, id) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`,
		s.tenantID,
		string(payload),
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

func (s Store) ApplyToConfig(cfg *config.Config) bool {
	if s.db == nil || cfg == nil {
		return false
	}
	stored := s.Get()
	if stored == nil {
		return false
	}
	cfg.Routing = *stored
	return true
}

func (s Store) MigrateFromConfig(cfg *config.Config) (migrated bool, hadStored bool) {
	if s.db == nil || cfg == nil {
		return false, false
	}
	if s.Get() != nil {
		return false, true
	}
	if !meaningful(cfg.Routing) {
		return false, false
	}
	if err := s.Upsert(cfg.Routing); err != nil {
		log.Errorf("sqlite/routing: migrate routing config: %v", err)
		return false, false
	}
	return true, false
}

func migrateTenantSchema(db *sql.DB) {
	if db == nil {
		return
	}
	rows, err := db.Query("PRAGMA table_info(routing_config)")
	if err != nil {
		return // PostgreSQL schema is handled by versioned migrations.
	}
	defer rows.Close()
	hasTenant := false
	tenantPrimary := false
	idPrimary := false
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
		if _, err = tx.Exec("ALTER TABLE routing_config ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'"); err != nil {
			return
		}
	}
	if _, err = tx.Exec("ALTER TABLE routing_config RENAME TO routing_config_legacy"); err != nil {
		return
	}
	if _, err = tx.Exec(createRoutingConfigTableSQL); err != nil {
		return
	}
	if _, err = tx.Exec("INSERT INTO routing_config(tenant_id,id,payload,updated_at) SELECT tenant_id,id,payload,updated_at FROM routing_config_legacy"); err != nil {
		return
	}
	if _, err = tx.Exec("DROP TABLE routing_config_legacy"); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		log.Warnf("sqlite/routing: migrate tenant schema: %v", err)
	}
}
