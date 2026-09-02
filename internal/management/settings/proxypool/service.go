package proxypool

import (
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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
