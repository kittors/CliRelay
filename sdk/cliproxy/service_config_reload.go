package cliproxy

import (
	"context"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
)

func (s *Service) startOpenRouterModelSync(ctx context.Context) {
	serviceapp.StartOpenRouterModelSync(ctx)
}

func (s *Service) applyDBBackedRuntimeSettings(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	serviceapp.ApplyDBBackedRuntimeSettings(cfg, s.configPath)
}

func (s *Service) syncConfigDerivedAuths(cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	serviceapp.SyncConfigDerivedAuths(cfg, s.coreManager)
}

func (s *Service) applyRetryConfig(cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	maxInterval := time.Duration(cfg.MaxRetryInterval) * time.Second
	s.coreManager.SetRetryConfig(cfg.RequestRetry, maxInterval)
	s.coreManager.SetAccountConcurrencyConfig(cfg.AccountConcurrency.WaitTimeout(), cfg.AccountConcurrency.QueueDepth())
}

func (s *Service) applyConfigReload(newCfg *config.Config, refreshRegisteredModels bool) {
	if s == nil {
		return
	}
	previousStrategy := ""
	s.cfgMu.RLock()
	if s.cfg != nil {
		previousStrategy = strings.ToLower(strings.TrimSpace(s.cfg.Routing.Strategy))
	}
	s.cfgMu.RUnlock()

	if newCfg == nil {
		s.cfgMu.RLock()
		newCfg = s.cfg
		s.cfgMu.RUnlock()
	}
	if newCfg == nil {
		return
	}
	s.applyDBBackedRuntimeSettings(newCfg)

	nextStrategy := strings.ToLower(strings.TrimSpace(newCfg.Routing.Strategy))
	previousStrategy = config.NormalizeRoutingStrategy(previousStrategy)
	nextStrategy = config.NormalizeRoutingStrategy(nextStrategy)
	if s.coreManager != nil && previousStrategy != nextStrategy {
		var selector coreauth.Selector
		switch nextStrategy {
		case "fill-first":
			selector = &coreauth.FillFirstSelector{}
		default:
			selector = &coreauth.RoundRobinSelector{}
		}
		s.coreManager.SetSelector(selector)
	}

	s.applyRetryConfig(newCfg)
	s.applyPprofConfig(newCfg)
	if s.server != nil {
		s.server.UpdateClients(newCfg)
	} else {
		s.syncConfigDerivedAuths(newCfg)
	}
	s.cfgMu.Lock()
	s.cfg = newCfg
	s.cfgMu.Unlock()
	if s.watcher != nil {
		s.watcher.SetConfig(newCfg)
	}
	if s.coreManager != nil {
		s.coreManager.SetConfig(newCfg)
		s.coreManager.SetOAuthModelAlias(newCfg.OAuthModelAlias)
	}
	s.rebindExecutors()
	if refreshRegisteredModels && s.coreManager != nil {
		s.refreshRegisteredModels(context.Background())
	}
}

func isServiceConfigDerivedAuth(authState *coreauth.Auth) bool {
	if authState == nil || authState.Attributes == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(authState.Attributes["source"])), "config:")
}
