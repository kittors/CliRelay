package api

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/access"
	internalserviceapp "github.com/router-for-me/CLIProxyAPI/v6/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/ipaccess"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// configSyncState applies management changes made on other cluster nodes to
// this node. It only runs in cluster mode: a single node never receives
// configuration events, so nothing here changes single-node behaviour.
type configSyncState struct {
	mu         sync.Mutex
	dispatcher *configsync.Dispatcher
	// resync is the full reload of last resort (see WithConfigResyncCallback).
	resync func(*config.Config)
	// modelsChanged re-registers the models of one tenant's credentials.
	modelsChanged func(tenantID string)
}

// startConfigSync subscribes this node to configuration events. Server.Start
// calls it, which is after the cluster coordinator has joined: a coordinator
// taken earlier would be the single-node stand-in whose subscriptions never
// fire.
func (s *Server) startConfigSync() {
	if coordinator := cluster.Default(); coordinator.Enabled() {
		s.startConfigSyncWith(coordinator, configsync.DefaultWindow)
	}
}

// startConfigSyncWith attaches the dispatcher to coordinator; tests pass a
// MemoryHub node and a short window.
func (s *Server) startConfigSyncWith(coordinator *cluster.Coordinator, window time.Duration) *configsync.Dispatcher {
	s.configSync.mu.Lock()
	defer s.configSync.mu.Unlock()
	if s.configSync.dispatcher != nil {
		return s.configSync.dispatcher
	}
	d := configsync.NewDispatcher(configsync.Options{
		Window:     window,
		Handlers:   s.configSyncHandlers(),
		FullReload: s.fullConfigReload,
		Fingerprint: func(ctx context.Context) (configsync.Fingerprints, error) {
			return configsync.SnapshotFingerprints(ctx, usage.RuntimeDB())
		},
		NodeID: coordinator.NodeID(),
	})
	d.Start(coordinator)
	s.configSync.dispatcher = d
	return d
}

func (s *Server) stopConfigSync() {
	s.configSync.mu.Lock()
	d := s.configSync.dispatcher
	s.configSync.dispatcher = nil
	s.configSync.mu.Unlock()
	d.Stop()
}

func (s *Server) configSyncHandlers() map[string]configsync.Handler {
	reloadAccess := func(context.Context, []cluster.ConfigEvent) error { return s.reloadAccessProviders() }
	// Read from the database on every use; nothing on this node caches them.
	readOnDemand := func(context.Context, []cluster.ConfigEvent) error { return nil }
	return map[string]configsync.Handler{
		configsync.DomainAPIKeys:            reloadAccess,
		configsync.DomainPermissionProfiles: reloadAccess,
		configsync.DomainEndUsers:           reloadAccess,
		configsync.DomainRouting:            s.reloadRouting,
		configsync.DomainProxyPool:          s.reloadProxyPool,
		configsync.DomainRuntimeSettings:    s.reloadRuntimeSettings,
		configsync.DomainModelConfigs:       s.reloadModelConfigs,
		configsync.DomainModelOwnerPresets:  readOnDemand,
		configsync.DomainCcSwitch:           readOnDemand,
		configsync.DomainPricing: func(context.Context, []cluster.ConfigEvent) error {
			usage.ReloadPricingCache()
			return nil
		},
		configsync.DomainIPAccessPolicy: func(ctx context.Context, _ []cluster.ConfigEvent) error {
			ipaccess.Default().ReloadPolicy(ctx)
			return nil
		},
		configsync.DomainIPAccessRules: func(ctx context.Context, _ []cluster.ConfigEvent) error {
			return ipaccess.Default().Refresh(ctx)
		},
		configsync.DomainTenants: s.reloadTenants,
	}
}

// liveConfig runs fn on this node's live configuration under the management
// handler's lock, the lock its own writes take, and returns the config.
func (s *Server) liveConfig(fn func(*config.Config)) *config.Config {
	var live *config.Config
	apply := func(cfg *config.Config) {
		live = cfg
		if fn != nil {
			fn(cfg)
		}
	}
	if s.mgmt != nil {
		s.mgmt.WithLiveConfig(apply)
	} else if s.cfg != nil {
		apply(s.cfg)
	}
	return live
}

func (s *Server) coreManager() *auth.Manager {
	if s == nil || s.handlers == nil {
		return nil
	}
	return s.handlers.AuthManager
}

// reloadAccessProviders rebuilds the API key map requests are authenticated
// against. It reads api keys, permission profiles and end-user accounts from
// the database and touches nothing else, in particular no executor.
func (s *Server) reloadAccessProviders() error {
	cfg := s.liveConfig(nil)
	if s.accessManager == nil || cfg == nil {
		return nil
	}
	_, err := access.ApplyAccessProviders(s.accessManager, nil, cfg)
	return err
}

func tenantsOf(events []cluster.ConfigEvent) []string {
	seen := make(map[string]struct{}, len(events))
	var out []string
	for _, ev := range events {
		tenant := configsync.NormalizeTenantID(ev.TenantID)
		if _, dup := seen[tenant]; dup {
			continue
		}
		seen[tenant] = struct{}{}
		out = append(out, tenant)
	}
	return out
}

// reloadRouting applies stored routing configs. The selector is swapped only
// when the system strategy actually changed, as a local reload does.
func (s *Server) reloadRouting(_ context.Context, events []cluster.ConfigEvent) error {
	manager := s.coreManager()
	for _, tenant := range tenantsOf(events) {
		if tenant != identity.SystemTenantID {
			base := s.liveConfig(nil)
			tenantCfg := usage.BuildTenantRuntimeConfig(base, tenant)
			if manager != nil {
				manager.SetConfigForTenant(tenant, &tenantCfg)
			}
			continue
		}
		var previous, next string
		cfg := s.liveConfig(func(live *config.Config) {
			previous = config.NormalizeRoutingStrategy(live.Routing.Strategy)
			usage.ApplyStoredRoutingConfig(live)
			next = config.NormalizeRoutingStrategy(live.Routing.Strategy)
		})
		if manager == nil || cfg == nil {
			continue
		}
		manager.SetConfig(cfg)
		if previous != next {
			manager.SetSelector(selectorForStrategy(next))
		}
	}
	return nil
}

// selectorForStrategy mirrors the choice the service makes on reload.
func selectorForStrategy(strategy string) auth.Selector {
	if strings.EqualFold(strategy, "fill-first") {
		return &auth.FillFirstSelector{}
	}
	return &auth.RoundRobinSelector{}
}

// reloadProxyPool applies stored proxy pools. System executors read the live
// config, so the pool is applied in place and config-derived credentials are
// re-resolved without replacing executors. Tenant executors hold a copy of
// their tenant config taken when they were bound, so a tenant whose pool
// changed has its executors rebound, as a local save does.
func (s *Server) reloadProxyPool(_ context.Context, events []cluster.ConfigEvent) error {
	manager := s.coreManager()
	for _, tenant := range tenantsOf(events) {
		if tenant != identity.SystemTenantID {
			base := s.liveConfig(nil)
			if manager == nil || base == nil {
				continue
			}
			tenantCfg := usage.BuildTenantRuntimeConfig(base, tenant)
			manager.SetConfigForTenant(tenant, &tenantCfg)
			internalserviceapp.RebindTenantExecutors(base, manager, tenant, nil)
			continue
		}
		cfg := s.liveConfig(func(live *config.Config) { usage.ApplyStoredProxyPool(live) })
		if manager == nil || cfg == nil {
			continue
		}
		manager.SetConfig(cfg)
		internalserviceapp.SyncConfigDerivedAuthsForTenantInPlace(cfg, manager, identity.SystemTenantID)
	}
	return nil
}

// reloadRuntimeSettings applies stored runtime settings key by key.
func (s *Server) reloadRuntimeSettings(_ context.Context, events []cluster.ConfigEvent) error {
	var systemKeys []string
	tenantKeys := make(map[string][]string)
	for _, ev := range events {
		tenant := configsync.NormalizeTenantID(ev.TenantID)
		if tenant == identity.SystemTenantID {
			systemKeys = append(systemKeys, ev.Key)
		} else {
			tenantKeys[tenant] = append(tenantKeys[tenant], ev.Key)
		}
	}
	if len(systemKeys) > 0 {
		s.reloadSystemRuntimeSettings(systemKeys)
	}
	manager := s.coreManager()
	for tenant, keys := range tenantKeys {
		base := s.liveConfig(nil)
		if manager == nil || base == nil {
			continue
		}
		// Credentials live in the credential records, so provider-key changes
		// are applied without touching executors. Any other tenant setting is
		// read by executors from the config copy they were bound with, which
		// only a rebind refreshes.
		if onlyProviderLists(keys) {
			internalserviceapp.SyncConfigDerivedAuthsForTenantInPlace(base, manager, tenant)
		} else {
			internalserviceapp.SyncConfigDerivedAuthsForTenant(base, manager, tenant)
		}
		if needsModelRefresh(keys) && s.configSync.modelsChanged != nil {
			s.configSync.modelsChanged(tenant)
		}
	}
	return nil
}

// reloadSystemRuntimeSettings applies keys to the live config in place. The
// system tenant's executors read that config by reference, so no executor is
// replaced and no upstream session is cut.
func (s *Server) reloadSystemRuntimeSettings(keys []string) {
	cfg := s.liveConfig(func(live *config.Config) { usage.ReloadRuntimeSettingKeys(identity.SystemTenantID, live, keys...) })
	if cfg == nil {
		return
	}
	s.applyConfigInPlace(cfg)
	manager := s.coreManager()
	if manager != nil {
		manager.SetConfig(cfg)
		manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
	}
	if manager != nil && !onlyNonProviderLists(keys) {
		internalserviceapp.SyncConfigDerivedAuthsForTenantInPlace(cfg, manager, identity.SystemTenantID)
	}
	if needsModelRefresh(keys) && s.configSync.modelsChanged != nil {
		s.configSync.modelsChanged(identity.SystemTenantID)
	}
}

func onlyProviderLists(keys []string) bool {
	for _, key := range keys {
		if !runtimeconfig.IsProviderListKey(key) {
			return false
		}
	}
	return true
}

func onlyNonProviderLists(keys []string) bool {
	for _, key := range keys {
		if runtimeconfig.IsProviderListKey(key) {
			return false
		}
	}
	return true
}

func needsModelRefresh(keys []string) bool {
	for _, key := range keys {
		if runtimeconfig.ReloadScopeForKey(key) == runtimeconfig.ReloadWithModels {
			return true
		}
	}
	return false
}

// applyConfigInPlace runs the parts of UpdateClients that follow a config
// value to where it takes effect (log level, request logging, retries,
// websocket auth, access providers, amp module ...), and skips the credential
// sync, whose executor rebinding is what a peer reload must avoid.
func (s *Server) applyConfigInPlace(cfg *config.Config) {
	oldCfg := s.oldConfigSnapshot()
	s.applyRequestLoggerConfig(oldCfg, cfg)
	s.applyProcessLoggingConfig(oldCfg, cfg)
	s.applyUsageStatisticsConfig(oldCfg, cfg)
	usage.ApplyRequestLogStorageConfig(cfg.RequestLogStorage)
	s.applyAuthRuntimeConfig(oldCfg, cfg)
	s.applyRequestBodyConfig(oldCfg, cfg)
	s.applyRuntimeLogLevel(oldCfg, cfg)
	s.applyProxyWarmupConfig(cfg)
	s.updateManagementRouteAvailability(oldCfg, cfg)
	s.applyAccessConfig(oldCfg, cfg)
	s.commitUpdatedConfig(oldCfg, cfg)
	s.refreshServerHandlers(cfg)
	s.refreshAmpModule(oldCfg, cfg)
}

// reloadModelConfigs drops cached model state and re-registers the models of
// the affected tenants' credentials.
func (s *Server) reloadModelConfigs(_ context.Context, events []cluster.ConfigEvent) error {
	modelconfigsettings.InvalidateDisabledModelCache()
	if s.configSync.modelsChanged == nil {
		return nil
	}
	for _, tenant := range tenantsOf(events) {
		s.configSync.modelsChanged(tenant)
	}
	return nil
}

// reloadTenants rebuilds the per-tenant runtime configs, which is how a tenant
// created on another node becomes routable here.
func (s *Server) reloadTenants(_ context.Context, events []cluster.ConfigEvent) error {
	cfg := s.liveConfig(nil)
	if manager := s.coreManager(); manager != nil && cfg != nil {
		internalserviceapp.ApplyTenantRuntimeConfigs(cfg, manager)
	}
	// The tenant access check serves cached rows for up to 15 s; drop them so
	// a tenant suspended or expired on another node is refused here at once.
	invalidateTenantCaches(events)
	return nil
}

// invalidateTenantCaches drops the cached tenant rows named by events, or all
// of them when an event does not name its tenant (collection-level changes
// and resyncs), since then any row may be stale.
func invalidateTenantCaches(events []cluster.ConfigEvent) {
	if len(events) == 0 {
		identity.InvalidateAllTenants()
		return
	}
	for _, event := range events {
		if strings.TrimSpace(event.TenantID) == "" {
			identity.InvalidateAllTenants()
			return
		}
	}
	for _, event := range events {
		identity.InvalidateTenant(event.TenantID)
	}
}

// fullConfigReload is the dispatcher's last resort: the service's own reload
// plus every cache this file reloads per domain.
func (s *Server) fullConfigReload(ctx context.Context) error {
	cfg := s.liveConfig(nil)
	if cfg == nil {
		return nil
	}
	if s.configSync.resync != nil {
		s.configSync.resync(cfg)
	} else {
		applyStoredConfigOverlays(cfg, s.configFilePath)
		s.UpdateClients(cfg)
	}
	usage.ReloadPricingCache()
	modelconfigsettings.InvalidateDisabledModelCache()
	identity.InvalidateAllTenants()
	ipaccess.Default().ReloadPolicy(ctx)
	if err := ipaccess.Default().Refresh(ctx); err != nil {
		log.WithError(err).Debug("config sync: ip access refresh failed")
	}
	if err := s.reloadAccessProviders(); err != nil {
		return err
	}
	return s.reloadTenants(ctx, nil)
}
