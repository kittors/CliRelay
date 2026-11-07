package proxypool

import (
	"context"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	sqlproxypool "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

var ErrItemNotFound = errors.New("item not found")

func StoreAvailable() bool {
	return usage.ProxyPoolStoreAvailable()
}

func List() []config.ProxyPoolEntry {
	return usage.ListProxyPool()
}

func Get(id string) *config.ProxyPoolEntry {
	return usage.GetProxyPoolEntry(id)
}

func Replace(entries []config.ProxyPoolEntry) error {
	return usage.ReplaceProxyPool(entries)
}

func Update(id string, entry config.ProxyPoolEntry) error {
	err := usage.UpdateProxyPoolEntry(id, entry)
	if errors.Is(err, sqlproxypool.ErrEntryNotFound) {
		return ErrItemNotFound
	}
	return err
}

func StoreAvailableForTenant(tenantID string) bool {
	return usage.ProxyPoolStoreAvailableForTenant(tenantID)
}
func ListForTenant(tenantID string) []config.ProxyPoolEntry {
	return usage.ListProxyPoolForTenant(tenantID)
}
func GetForTenant(tenantID, id string) *config.ProxyPoolEntry {
	return usage.GetProxyPoolEntryForTenant(tenantID, id)
}
func ReplaceForTenant(tenantID string, entries []config.ProxyPoolEntry) error {
	return usage.ReplaceProxyPoolForTenant(tenantID, entries)
}
func UpdateForTenant(tenantID, id string, entry config.ProxyPoolEntry) error {
	err := usage.UpdateProxyPoolEntryForTenant(tenantID, id, entry)
	if errors.Is(err, sqlproxypool.ErrEntryNotFound) {
		return ErrItemNotFound
	}
	return err
}

// ReplaceForTenantExpect replaces the tenant's proxy pool if the collection is
// still at expected (configsync.AnyVersion: unchecked).
func ReplaceForTenantExpect(ctx context.Context, tenantID string, entries []config.ProxyPoolEntry, expected int64) (int64, error) {
	return usage.ReplaceProxyPoolForTenantExpect(ctx, tenantID, entries, expected)
}

// SharedForTenant returns the system tenant's proxies the platform shares with
// tenantID; the tenant resolves them like its own unless it overrides one.
func SharedForTenant(tenantID string) []config.ProxyPoolEntry {
	return usage.SharedProxyPoolForTenant(tenantID)
}

// TenantsOverriding returns, for each of ids, the tenants whose own entry with
// that ID overrides the shared one.
func TenantsOverriding(ctx context.Context, ids []string) (map[string][]string, error) {
	return usage.TenantsOverridingProxies(ctx, ids)
}

// CollectionVersionForTenant returns the version of the tenant's proxy pool,
// for read-modify-write updates made on the tenant's behalf.
func CollectionVersionForTenant(tenantID string) int64 {
	return usage.ConfigCollectionVersion(configsync.DomainProxyPool, tenantID)
}
