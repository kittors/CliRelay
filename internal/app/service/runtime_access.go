package serviceapp

import (
	"context"
	"strings"
	"sync"

	configaccess "github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/schedulingload"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type OAuthProviderModelConfigRow struct {
	ModelID     string
	OwnedBy     string
	Description string
	Source      string
	Enabled     bool
}

// schedulingLoadProvider is a process-wide singleton: it caches one quota
// snapshot shared by every selector, and ApplyTenantRuntimeConfigs runs again on
// each config reload, so rebuilding it per call would throw the cache away.
var (
	schedulingLoadOnce     sync.Once
	schedulingLoadProvider *schedulingload.Provider
)

func sharedSchedulingLoadProvider() *schedulingload.Provider {
	schedulingLoadOnce.Do(func() {
		schedulingLoadProvider = schedulingload.NewProvider()
	})
	return schedulingLoadProvider
}

func ApplyTenantRuntimeConfigs(base *config.Config, manager *coreauth.Manager) {
	if base == nil || manager == nil || identity.Default() == nil {
		return
	}
	tenants, err := identity.Default().ListTenants(context.Background())
	if err != nil {
		return
	}
	loadProvider := sharedSchedulingLoadProvider()
	loadProvider.TrackTenant(identity.SystemTenantID)
	for _, tenant := range tenants {
		if tenant.ID == "" || tenant.ID == identity.SystemTenantID {
			continue
		}
		loadProvider.TrackTenant(tenant.ID)
		tenantCfg := internalusage.BuildTenantRuntimeConfig(base, tenant.ID)
		manager.SetConfigForTenant(tenant.ID, &tenantCfg)
	}
	// Least-load distribution and the sticky release threshold both read this;
	// without it they degrade to concurrency-only signals.
	manager.SetQuotaLoadSource(loadProvider)
}

func ListOAuthProviderModelConfigRows() []OAuthProviderModelConfigRow {
	return ListOAuthProviderModelConfigRowsForTenant("")
}

// ListOAuthProviderModelConfigRowsForTenant reads the model library of the
// tenant that owns the credential being registered. Registration used to read
// the system tenant only, so catalog models added by any other tenant never
// became routable for that tenant's accounts.
func ListOAuthProviderModelConfigRowsForTenant(tenantID string) []OAuthProviderModelConfigRow {
	rows := modelconfigsettings.ListAllConfigsForTenant(tenantID)
	if len(rows) == 0 {
		return nil
	}
	out := make([]OAuthProviderModelConfigRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, OAuthProviderModelConfigRow{
			ModelID:     row.ModelID,
			OwnedBy:     row.OwnedBy,
			Description: row.Description,
			Source:      row.Source,
			Enabled:     row.Enabled,
		})
	}
	return out
}

// ListModelOwnersForAuthGroupsForTenant returns the model owners a tenant has
// mapped onto the given auth-group identifiers (provider, channel name, channel
// identifiers). Operators express "these catalog models belong to this channel"
// through that mapping, so registration must honour it instead of relying only
// on the built-in provider→owner aliases.
func ListModelOwnersForAuthGroupsForTenant(tenantID string, authGroups []string) []string {
	if len(authGroups) == 0 {
		return nil
	}
	mappings := modelconfigsettings.ListAuthGroupOwnerMappingsForTenant(tenantID)
	if len(mappings) == 0 {
		return nil
	}
	byGroup := make(map[string]string, len(mappings))
	for _, row := range mappings {
		group := normalizeOwnerScopeKey(row.AuthGroup)
		owner := strings.TrimSpace(row.Owner)
		if group == "" || owner == "" {
			continue
		}
		byGroup[group] = owner
	}
	seen := make(map[string]struct{}, len(authGroups))
	out := make([]string, 0, len(authGroups))
	for _, group := range authGroups {
		owner := byGroup[normalizeOwnerScopeKey(group)]
		if owner == "" {
			continue
		}
		if _, exists := seen[owner]; exists {
			continue
		}
		seen[owner] = struct{}{}
		out = append(out, owner)
	}
	return out
}

func normalizeOwnerScopeKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func ConfigureServiceAccess(cfg *config.Config, accessManager *sdkaccess.Manager) {
	if cfg == nil || accessManager == nil {
		return
	}
	configaccess.Register(&cfg.SDKConfig)
	accessManager.SetProviders(sdkaccess.RegisteredProviders())
}

func BuildAPIKeyClients(cfg *config.Config) (int, int, int, int, int, int, int, int) {
	return watcher.BuildAPIKeyClients(cfg)
}

func StartOpenRouterModelSync(ctx context.Context) {
	internalusage.StartOpenRouterModelSyncScheduler(ctx)
}

func ApplyDBBackedRuntimeSettings(cfg *config.Config, configPath string) {
	if cfg == nil {
		return
	}
	internalusage.MigrateRoutingConfigFromConfig(cfg, configPath)
	internalusage.ApplyStoredRoutingConfig(cfg)
	internalusage.MigrateProxyPoolFromConfig(cfg, configPath)
	internalusage.ApplyStoredProxyPool(cfg)
	internalusage.MigrateRuntimeSettingsFromConfig(cfg, configPath)
	internalusage.ApplyStoredRuntimeSettings(cfg)
}
