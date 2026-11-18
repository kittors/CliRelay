package api

import (
	internalserviceapp "github.com/router-for-me/CLIProxyAPI/v8/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func (s *Server) syncConfigDerivedAuths(cfg *config.Config) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil || cfg == nil {
		return
	}
	internalserviceapp.SyncConfigDerivedAuths(cfg, s.handlers.AuthManager)
}
