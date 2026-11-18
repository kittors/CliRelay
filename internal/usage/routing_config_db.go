package usage

import (
	"context"
	"database/sql"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sqlrouting "github.com/router-for-me/CLIProxyAPI/v8/internal/storage/sqlstore/routing"
	log "github.com/sirupsen/logrus"
)

// Compatibility bridge contract:
// - Owner: runtime routing configuration boundary.
// - Real implementation: internal/storage/sqlstore/routing.
// - Allowed callers: bootstrap/runtime overlay and legacy management adapters pending migration.
// - Exit condition: management/service callers depend on dedicated routing boundary instead of usage; do not add new imports here.
func initRoutingConfigTable(db *sql.DB) {
	sqlrouting.InitTable(db)
}

func routingConfigStore() sqlrouting.Store {
	return sqlrouting.NewStore(getDB())
}

func routingConfigStoreForTenant(tenantID string) sqlrouting.Store {
	return sqlrouting.NewTenantStore(getDB(), tenantID)
}

func ApplyStoredRoutingConfig(cfg *config.Config) bool {
	if cfg == nil || !ConfigStoreAvailable() {
		return false
	}
	return routingConfigStore().ApplyToConfig(cfg)
}

func MigrateRoutingConfigFromConfig(cfg *config.Config, configFilePath string) bool {
	if cfg == nil || !ConfigStoreAvailable() {
		return false
	}
	fileRouting := cfg.Routing
	migrated, hadStored := routingConfigStore().MigrateFromConfig(cfg)
	if hadStored {
		if cleanRoutingConfigFromYAML(configFilePath) > 0 {
			warnIfYAMLRoutingIgnored(fileRouting)
		}
		return false
	}
	if !migrated {
		return false
	}
	if strings.TrimSpace(configFilePath) != "" {
		if backupConfigForMigration(configFilePath, routingMigrationBackupSuffix) {
			cleanRoutingConfigFromYAML(configFilePath)
		}
	}
	return true
}

// warnIfYAMLRoutingIgnored reports a routing section that was just dropped
// from config.yaml because the database holds routing, when it differed from
// the stored value. A management save of such a section is rejected (see
// DatabaseOwnedSectionsChangedByYAML); a file edited by hand is only noticed
// here, at the reload or start that drops it. Unlike that check it cannot
// resolve channel names to their canonical spelling, which needs the auth
// list, so a copy that only spells a channel differently is reported too.
func warnIfYAMLRoutingIgnored(fileRouting config.RoutingConfig) {
	stored := routingConfigStore().Get()
	if stored == nil || config.SameRoutingConfig(fileRouting, *stored) {
		return
	}
	log.Warnf("usage: ignored the routing section of config.yaml: it differs from the routing stored in the database, which owns it; edit routing on %s", databaseOwnedSectionHomes["routing"])
}

// LoadStoredRoutingConfig returns the system tenant's stored routing config,
// nil when it has none, or the error that kept it from being read.
func LoadStoredRoutingConfig(ctx context.Context) (*config.RoutingConfig, error) {
	stored, _, err := routingConfigStore().Load(ctx)
	return stored, err
}

func GetRoutingConfig() *config.RoutingConfig {
	return routingConfigStore().Get()
}

func GetRoutingConfigForTenant(tenantID string) *config.RoutingConfig {
	return routingConfigStoreForTenant(tenantID).Get()
}

func UpsertRoutingConfig(cfg config.RoutingConfig) error {
	return routingConfigStore().Upsert(cfg)
}

func UpsertRoutingConfigForTenant(tenantID string, cfg config.RoutingConfig) error {
	return routingConfigStoreForTenant(tenantID).Upsert(cfg)
}

// GetRoutingConfigWithVersionForTenant returns the stored routing config and
// its version, or (nil, 0) when the tenant has none.
func GetRoutingConfigWithVersionForTenant(tenantID string) (*config.RoutingConfig, int64) {
	return routingConfigStoreForTenant(tenantID).GetWithVersion()
}

// CompareAndSwapRoutingConfigForTenant stores cfg if the stored version still
// equals expected (0: none stored, configsync.AnyVersion: unchecked).
func CompareAndSwapRoutingConfigForTenant(ctx context.Context, tenantID string, cfg config.RoutingConfig, expected int64) (int64, error) {
	return routingConfigStoreForTenant(tenantID).CompareAndSwap(ctx, cfg, expected)
}

// UpdateRoutingConfigForTenant applies a derived change to the stored routing
// config, retrying on concurrent writes; see sqlrouting.Store.Update.
func UpdateRoutingConfigForTenant(ctx context.Context, tenantID string, fallback config.RoutingConfig, mutate func(*config.RoutingConfig) bool) (config.RoutingConfig, bool, error) {
	return routingConfigStoreForTenant(tenantID).Update(ctx, fallback, mutate)
}
