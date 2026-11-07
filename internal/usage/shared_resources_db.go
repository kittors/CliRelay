package usage

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sqlproxypool "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	log "github.com/sirupsen/logrus"
)

// Platform-shared resources: which tenants may use the system tenant's proxies
// and AI account credentials. The model lives in sqlstore/resourcegrants; this
// file only adapts it for the runtime config built here.

func resourceGrantsStore() resourcegrants.Store {
	return resourcegrants.NewStore(getDB())
}

// ResourceGrantsAvailable reports whether grants can be read and written.
func ResourceGrantsAvailable() bool {
	return resourceGrantsStore().Available()
}

// ListResourceGrants returns the grantees of every shared resource of kind,
// keyed by resource ID.
func ListResourceGrants(ctx context.Context, kind string) (map[string][]string, error) {
	return resourceGrantsStore().List(ctx, kind)
}

// ReplaceResourceGrants gives every resource in ids exactly the grantees in
// tenants (resourcegrants.AllTenants for everybody, empty to stop sharing).
func ReplaceResourceGrants(ctx context.Context, kind string, ids, tenants []string) (int64, error) {
	return resourceGrantsStore().Replace(ctx, kind, ids, tenants)
}

// SharedProxyPoolForTenant returns the system tenant's proxies shared with
// tenantID, in the system pool's order. The system tenant itself owns them and
// gets nothing here. A failed read is logged and yields no shared entries,
// like a failed read of the tenant's own pool does.
func SharedProxyPoolForTenant(tenantID string) []config.ProxyPoolEntry {
	tenantID = normalizeTenantID(tenantID)
	if tenantID == systemTenantID || !ResourceGrantsAvailable() {
		return nil
	}
	grants, err := ListResourceGrants(context.Background(), resourcegrants.KindProxy)
	if err != nil {
		log.WithError(err).Warn("usage: read shared proxy grants")
		return nil
	}
	if len(grants) == 0 {
		return nil
	}
	var shared []config.ProxyPoolEntry
	for _, entry := range ListProxyPool() {
		if resourcegrants.Allows(grants[entry.ID], tenantID) {
			shared = append(shared, entry)
		}
	}
	return shared
}

// TenantsOverridingProxies returns, for each of ids, the tenants whose own
// pool holds an entry with that ID and so overrides the shared entry.
func TenantsOverridingProxies(ctx context.Context, ids []string) (map[string][]string, error) {
	return sqlproxypool.ListTenantsHoldingIDs(ctx, getDB(), ids)
}
