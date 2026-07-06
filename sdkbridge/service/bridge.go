// Package serviceapp exposes service/runtime bridges outside the sdk tree.
//
// sdk/cliproxy uses this package to reach service runtime helpers without
// importing internal assembly packages directly.
package serviceapp

import (
	"context"

	internalserviceapp "github.com/router-for-me/CLIProxyAPI/v6/internal/app/service"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

type RuntimeAuthUpdateAction = internalserviceapp.RuntimeAuthUpdateAction

const (
	RuntimeAuthUpdateActionAdd    = internalserviceapp.RuntimeAuthUpdateActionAdd
	RuntimeAuthUpdateActionModify = internalserviceapp.RuntimeAuthUpdateActionModify
	RuntimeAuthUpdateActionDelete = internalserviceapp.RuntimeAuthUpdateActionDelete
)

type RuntimeAuthUpdate = internalserviceapp.RuntimeAuthUpdate

type WatcherBridge = internalserviceapp.WatcherBridge

type ServerStarter = internalserviceapp.ServerStarter

func NewWatcher(configPath, authDir string, reload func(*config.Config)) (WatcherBridge, error) {
	return internalserviceapp.NewWatcher(configPath, authDir, reload)
}

type WebsocketGateway = internalserviceapp.WebsocketGateway

func NewDefaultWebsocketGateway(onConnected func(string), onDisconnected func(string, error)) WebsocketGateway {
	return internalserviceapp.NewDefaultWebsocketGateway(onConnected, onDisconnected)
}

type PprofServer = internalserviceapp.PprofServer

func NewPprofServer() PprofServer {
	return internalserviceapp.NewPprofServer()
}

func SanitizePprofAddr(addr string, allowRemote bool) string {
	return internalserviceapp.SanitizePprofAddr(addr, allowRemote)
}

func StartServerLoop(server ServerStarter) chan error {
	return internalserviceapp.StartServerLoop(server)
}

func EnsureAuthDir(authDir string) error {
	return internalserviceapp.EnsureAuthDir(authDir)
}

type OAuthProviderModelConfigRow = internalserviceapp.OAuthProviderModelConfigRow

func ListOAuthProviderModelConfigRows() []OAuthProviderModelConfigRow {
	return internalserviceapp.ListOAuthProviderModelConfigRows()
}

func ConfigureServiceAccess(cfg *config.Config, accessManager *sdkaccess.Manager) {
	internalserviceapp.ConfigureServiceAccess(cfg, accessManager)
}

func NewDefaultAuthManager() *sdkAuth.Manager {
	return internalserviceapp.NewDefaultAuthManager()
}

func BuildAPIKeyClients(cfg *config.Config) (int, int, int, int, int, int, int, int) {
	return internalserviceapp.BuildAPIKeyClients(cfg)
}

func NewDefaultRoundTripperProvider() coreauth.RoundTripperProvider {
	return internalserviceapp.NewDefaultRoundTripperProvider()
}

func StartOpenRouterModelSync(ctx context.Context) {
	internalserviceapp.StartOpenRouterModelSync(ctx)
}

func ApplyDBBackedRuntimeSettings(cfg *config.Config, configPath string) {
	internalserviceapp.ApplyDBBackedRuntimeSettings(cfg, configPath)
}

func SyncConfigDerivedAuths(cfg *config.Config, coreManager *coreauth.Manager) {
	internalserviceapp.SyncConfigDerivedAuths(cfg, coreManager)
}

func FetchAntigravityModels(ctx context.Context, auth *coreauth.Auth, cfg *config.Config) []*sdkmodelcatalog.ModelInfo {
	return internalserviceapp.FetchAntigravityModels(ctx, auth, cfg)
}

func RegisterExecutorForAuth(coreManager *coreauth.Manager, cfg *config.Config, auth *coreauth.Auth, forceReplace bool, gateway WebsocketGateway) {
	internalserviceapp.RegisterExecutorForAuth(coreManager, cfg, auth, forceReplace, gateway)
}
