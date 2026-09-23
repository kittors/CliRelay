package serviceapp

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/modeldiscovery"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// ProviderDiscoveryDrivesRouting reports whether a provider's upstream model list is
// merged into the models its credentials register.
func ProviderDiscoveryDrivesRouting(provider string) bool {
	return modeldiscovery.DrivesRouting(provider)
}

// ProviderDiscoveryRoutingProviders lists the providers whose upstream lists drive routing.
func ProviderDiscoveryRoutingProviders() []string {
	return modeldiscovery.RoutingProviders()
}

// IsProviderDiscoveryCredential reports whether a credential takes part in its
// provider's shared upstream list; see modeldiscovery.IsDiscoveryCredential.
func IsProviderDiscoveryCredential(auth *coreauth.Auth, provider string) bool {
	return modeldiscovery.IsDiscoveryCredential(auth, provider)
}

// ProviderDiscoverySnapshot returns the last upstream model list stored for a
// tenant's credentials of a routing provider, however old it is.
func ProviderDiscoverySnapshot(tenantID, provider string) []*sdkmodelcatalog.ModelInfo {
	if !modeldiscovery.DrivesRouting(provider) {
		return nil
	}
	return modeldiscovery.Snapshot(tenantID, provider)
}

// RefreshProviderDiscovery re-fetches a provider's upstream list for a tenant and
// reports whether a live answer was stored.
func RefreshProviderDiscovery(ctx context.Context, manager *coreauth.Manager, cfg *config.Config, tenantID, provider string) bool {
	return modeldiscovery.Refresh(ctx, manager, cfg, tenantID, provider)
}

// SetProviderDiscoveryChangeHook registers the callback run after a routing
// provider's upstream list changes for a tenant; nil removes it.
func SetProviderDiscoveryChangeHook(fn func(tenantID, provider string)) {
	modeldiscovery.SetRoutingChangeHook(fn)
}
