package routing

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	log "github.com/sirupsen/logrus"
)

const createRoutingConfigTableSQL = `
CREATE TABLE IF NOT EXISTS routing_config (
  tenant_id  TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  id         INTEGER NOT NULL CHECK (id = 1),
  payload    TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL DEFAULT '',
  version    INTEGER NOT NULL DEFAULT 1,
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
	ensureVersionColumn(db)
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
	stored, _ := s.GetWithVersion()
	return stored
}

// Upsert stores cfg whatever the stored version is. It still bumps the
// version and announces the write; management saves use CompareAndSwap.
func (s Store) Upsert(cfg config.RoutingConfig) error {
	if s.db == nil {
		return nil
	}
	_, err := s.CompareAndSwap(context.Background(), cfg, configsync.AnyVersion)
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
	// Insert only: when several nodes start at once the first import wins.
	if _, err := s.CompareAndSwap(context.Background(), cfg.Routing, 0); err != nil {
		if errors.Is(err, configsync.ErrVersionConflict) {
			return false, true
		}
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
