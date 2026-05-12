package cliproxy

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func (s *Service) RegisterProxyBanNotifier(notifier ProxyBanNotifier) {
	if s == nil || s.compositeHook == nil {
		return
	}
	s.compositeHook.AddNotifier(notifier)
}

func (s *Service) CoreAuthManager() *coreauth.Manager {
	if s == nil {
		return nil
	}
	return s.coreManager
}

func (s *Service) AddServerOption(opt api.ServerOption) {
	if s == nil {
		return
	}
	s.serverOptions = append(s.serverOptions, opt)
}
