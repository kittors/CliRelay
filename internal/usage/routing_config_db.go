package usage

import (
	"database/sql"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sqlrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/routing"
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
	migrated, hadStored := routingConfigStore().MigrateFromConfig(cfg)
	if hadStored {
		cleanRoutingConfigFromYAML(configFilePath)
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
