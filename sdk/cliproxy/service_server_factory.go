package cliproxy

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func (s *Service) buildServerOptions() []api.ServerOption {
	serverOptions := append([]api.ServerOption(nil), s.serverOptions...)
	serverOptions = append(serverOptions, api.WithConfigMutatedCallback(func(updated *config.Config) {
		s.applyConfigReload(updated, true)
	}))
	serverOptions = append(serverOptions, api.WithModelConfigMutatedCallback(func(tenantID string) {
		s.onModelCatalogChanged(tenantID)
	}))
	// Last resort of cluster config sync: a full reload that, unlike a local
	// save, does not re-register every model.
	serverOptions = append(serverOptions, api.WithConfigResyncCallback(func(updated *config.Config) {
		s.applyConfigReload(updated, false)
	}))
	return serverOptions
}

func (s *Service) configureServer(ctx context.Context) {
	if s == nil {
		return
	}
	s.server = api.NewServer(s.cfg, s.coreManager, s.accessManager, s.configPath, s.buildServerOptions()...)
	if s.authManager == nil {
		s.authManager = newDefaultAuthManager()
	}
	s.bindWebsocketGateway(ctx)
}
