package serviceapp

import (
	"context"
	"sync"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

// Platform-shared resources at runtime. The grants themselves live in
// sqlstore/resourcegrants; this file applies them to the credential manager
// and to the proxy pools tenant executors are bound with.

// LoadSharedAuthGrants hands the manager the system credentials other tenants
// may use. A failed read keeps the grants the manager already holds: dropping
// them would cut grantees off a working account because of a database blip.
func LoadSharedAuthGrants(manager *coreauth.Manager) {
	if manager == nil || !internalusage.ResourceGrantsAvailable() {
		return
	}
	grants, err := internalusage.ListResourceGrants(context.Background(), resourcegrants.KindAuth)
	if err != nil {
		log.WithError(err).Warn("shared resources: read credential grants; keeping the previous ones")
		return
	}
	manager.SetSharedAuthGrants(grants)
}

// boundTenantProxyPools remembers the proxy pool each tenant's executors were
// last bound with. Executors copy the tenant config when they are bound, so a
// change to a shared proxy only reaches a tenant through a rebind, and a
// rebind cuts the tenant's open Codex websocket sessions. Comparing against
// what was bound lets an edit of the platform pool rebind only the tenants
// whose effective pool it actually changed.
var (
	boundTenantProxyPoolsMu sync.Mutex
	boundTenantProxyPools   = map[string][]internalconfig.ProxyPoolEntry{}
)

func rememberTenantProxyPool(tenantID string, pool []internalconfig.ProxyPoolEntry) {
	tenantID = coreauth.NormalizedTenantID(tenantID)
	if tenantID == identity.SystemTenantID {
		return
	}
	snapshot := append([]internalconfig.ProxyPoolEntry(nil), pool...)
	boundTenantProxyPoolsMu.Lock()
	boundTenantProxyPools[tenantID] = snapshot
	boundTenantProxyPoolsMu.Unlock()
}

func tenantProxyPoolUnchanged(tenantID string, pool []internalconfig.ProxyPoolEntry) bool {
	boundTenantProxyPoolsMu.Lock()
	bound, ok := boundTenantProxyPools[coreauth.NormalizedTenantID(tenantID)]
	boundTenantProxyPoolsMu.Unlock()
	// Order matters to ResolveProxyURL only through IDs, which a pool keeps
	// unique, so the unordered comparison is the right one.
	return ok && internalconfig.SameProxyPool(bound, pool)
}

// RefreshSharedProxyPools re-applies every tenant's effective proxy pool after
// the platform pool or the proxy grants changed, rebinding the executors of
// the tenants whose pool differs from the one they were bound with. The
// system tenant reads the live config and needs nothing here.
func RefreshSharedProxyPools(base *config.Config, manager *coreauth.Manager) {
	if base == nil || manager == nil || identity.Default() == nil {
		return
	}
	tenants, err := identity.Default().ListTenants(context.Background())
	if err != nil {
		log.WithError(err).Warn("shared resources: list tenants to refresh proxy pools")
		return
	}
	for _, tenant := range tenants {
		if tenant.ID == "" || tenant.ID == identity.SystemTenantID {
			continue
		}
		tenantCfg := internalusage.BuildTenantRuntimeConfig(base, tenant.ID)
		if tenantProxyPoolUnchanged(tenant.ID, tenantCfg.ProxyPool) {
			continue
		}
		manager.SetConfigForTenant(tenant.ID, &tenantCfg)
		RebindTenantExecutors(base, manager, tenant.ID, nil)
		// A tenant without credentials binds no executor; record the pool so
		// the next refresh does not treat it as changed again.
		rememberTenantProxyPool(tenant.ID, tenantCfg.ProxyPool)
	}
}
