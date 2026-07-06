package cliproxy

import (
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
)

func listOAuthProviderModelConfigRows() []oauthProviderModelConfigRow {
	rows := serviceapp.ListOAuthProviderModelConfigRows()
	if len(rows) == 0 {
		return nil
	}
	out := make([]oauthProviderModelConfigRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, oauthProviderModelConfigRow{
			ModelID:     row.ModelID,
			OwnedBy:     row.OwnedBy,
			Description: row.Description,
			Source:      row.Source,
			Enabled:     row.Enabled,
		})
	}
	return out
}

func configureServiceAccess(cfg *config.Config, accessManager *sdkaccess.Manager) {
	serviceapp.ConfigureServiceAccess(cfg, accessManager)
}

func newDefaultAuthManager() *sdkAuth.Manager {
	return serviceapp.NewDefaultAuthManager()
}

func buildAPIKeyClients(cfg *config.Config) (int, int, int, int, int, int, int, int) {
	return serviceapp.BuildAPIKeyClients(cfg)
}

type serverStarter interface {
	Start() error
}

func launchServiceServerLoop(server serverStarter) chan error {
	if server == nil {
		return nil
	}
	return serviceapp.StartServerLoop(server)
}

func ensureServiceAuthDir(authDir string) error {
	return serviceapp.EnsureAuthDir(authDir)
}

func toBridgeRuntimeAuthUpdate(update runtimeAuthUpdate) serviceapp.RuntimeAuthUpdate {
	return serviceapp.RuntimeAuthUpdate{
		Action: serviceapp.RuntimeAuthUpdateAction(update.Action),
		ID:     update.ID,
		Auth:   update.Auth,
	}
}

func fromBridgeRuntimeAuthUpdate(update serviceapp.RuntimeAuthUpdate) runtimeAuthUpdate {
	return runtimeAuthUpdate{
		Action: runtimeAuthUpdateAction(update.Action),
		ID:     update.ID,
		Auth:   update.Auth,
	}
}
