package usage

import (
	"context"
	"database/sql"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sqlproxypool "github.com/router-for-me/CLIProxyAPI/v8/internal/storage/sqlstore/proxypool"
	log "github.com/sirupsen/logrus"
)

// Compatibility bridge contract:
// - Owner: proxy pool runtime settings boundary.
// - Real implementation: internal/storage/sqlstore/proxypool.
// - Allowed callers: bootstrap/runtime overlay and legacy management adapters pending migration.
// - Exit condition: management/service callers use a dedicated proxy-pool boundary; do not add new imports here.
func initProxyPoolTable(db *sql.DB) {
	sqlproxypool.InitTable(db)
}

func proxyPoolStore() sqlproxypool.Store {
	return sqlproxypool.NewStore(getDB())
}

func proxyPoolStoreForTenant(tenantID string) sqlproxypool.Store {
	return sqlproxypool.NewTenantStore(getDB(), tenantID)
}

// ProxyPoolStoreAvailable reports whether the proxy-pool store is ready.
func ProxyPoolStoreAvailable() bool {
	return proxyPoolStore().Available()
}

// ListProxyPool retrieves all reusable proxies from the database.
func ListProxyPool() []config.ProxyPoolEntry {
	return proxyPoolStore().List()
}

// GetProxyPoolEntry retrieves one reusable proxy by ID.
func GetProxyPoolEntry(id string) *config.ProxyPoolEntry {
	return proxyPoolStore().Get(id)
}

// ReplaceProxyPool atomically replaces all stored proxy entries after normalization.
func ReplaceProxyPool(entries []config.ProxyPoolEntry) error {
	return proxyPoolStore().Replace(entries)
}

// UpdateProxyPoolEntry updates one reusable proxy identified by its stable ID.
func UpdateProxyPoolEntry(id string, entry config.ProxyPoolEntry) error {
	return proxyPoolStore().Update(id, entry)
}

// ApplyStoredProxyPool overlays the DB-backed proxy pool onto the runtime config.
func ApplyStoredProxyPool(cfg *config.Config) bool {
	if cfg == nil || !ProxyPoolStoreAvailable() {
		return false
	}
	return proxyPoolStore().ApplyToConfig(cfg)
}

// MigrateProxyPoolFromConfig moves legacy YAML proxy-pool entries into the database.
func MigrateProxyPoolFromConfig(cfg *config.Config, configFilePath string) int {
	if cfg == nil || !ProxyPoolStoreAvailable() {
		return 0
	}
	fileEntries := cfg.ProxyPool
	migrated, hadStored, shouldClean := proxyPoolStore().MigrateFromConfig(cfg)
	if hadStored {
		if cleanProxyPoolFromYAML(configFilePath) > 0 {
			warnIfYAMLProxyPoolIgnored(fileEntries)
		}
		return 0
	}
	if shouldClean && configFilePath != "" {
		if backupConfigForMigration(configFilePath, proxyPoolMigrationBackupSuffix) {
			cleanProxyPoolFromYAML(configFilePath)
		}
	}
	return migrated
}

// warnIfYAMLProxyPoolIgnored is warnIfYAMLRoutingIgnored for proxy-pool.
func warnIfYAMLProxyPoolIgnored(fileEntries []config.ProxyPoolEntry) {
	if config.SameProxyPool(fileEntries, proxyPoolStore().List()) {
		return
	}
	log.Warnf("usage: ignored the proxy-pool section of config.yaml: it differs from the proxy pool stored in the database, which owns it; edit proxies on %s", databaseOwnedSectionHomes["proxy-pool"])
}

// LoadStoredProxyPool returns the system tenant's stored proxy pool, or the
// error that kept it from being read; ListProxyPool returns nothing then.
func LoadStoredProxyPool(ctx context.Context) ([]config.ProxyPoolEntry, error) {
	return proxyPoolStore().ListContext(ctx)
}

func ProxyPoolStoreAvailableForTenant(tenantID string) bool {
	return proxyPoolStoreForTenant(tenantID).Available()
}
func ListProxyPoolForTenant(tenantID string) []config.ProxyPoolEntry {
	return proxyPoolStoreForTenant(tenantID).List()
}
func GetProxyPoolEntryForTenant(tenantID, id string) *config.ProxyPoolEntry {
	return proxyPoolStoreForTenant(tenantID).Get(id)
}
func ReplaceProxyPoolForTenant(tenantID string, entries []config.ProxyPoolEntry) error {
	return proxyPoolStoreForTenant(tenantID).Replace(entries)
}
func UpdateProxyPoolEntryForTenant(tenantID, id string, entry config.ProxyPoolEntry) error {
	return proxyPoolStoreForTenant(tenantID).Update(id, entry)
}
