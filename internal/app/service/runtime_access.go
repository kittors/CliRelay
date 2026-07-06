package serviceapp

import (
	"context"

	configaccess "github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type OAuthProviderModelConfigRow struct {
	ModelID     string
	OwnedBy     string
	Description string
	Source      string
	Enabled     bool
}

func ListOAuthProviderModelConfigRows() []OAuthProviderModelConfigRow {
	rows := modelconfigsettings.ListAllConfigs()
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
