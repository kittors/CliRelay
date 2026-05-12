package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type ProxyPoolMutator interface {
	DisableProxy(proxyID string) bool
	EnableProxy(proxyID string) bool
	GetPool() []config.ProxyPoolEntry
}

type SQLitePoolMutator struct{}

func (m *SQLitePoolMutator) GetPool() []config.ProxyPoolEntry {
	return usage.ListProxyPool()
}

func (m *SQLitePoolMutator) DisableProxy(proxyID string) bool {
	pool := usage.ListProxyPool()
	updated := make([]config.ProxyPoolEntry, 0, len(pool))
	found := false
	for _, entry := range pool {
		e := entry
		if e.ID == proxyID {
			e.Enabled = false
			found = true
		}
		updated = append(updated, e)
	}
	if !found {
		return false
	}
	return usage.ReplaceProxyPool(updated) == nil
}

func (m *SQLitePoolMutator) EnableProxy(proxyID string) bool {
	pool := usage.ListProxyPool()
	updated := make([]config.ProxyPoolEntry, 0, len(pool))
	found := false
	for _, entry := range pool {
		e := entry
		if e.ID == proxyID {
			e.Enabled = true
			found = true
		}
		updated = append(updated, e)
	}
	if !found {
		return false
	}
	return usage.ReplaceProxyPool(updated) == nil
}

type AuthStore interface {
	ListAuths() []*coreauth.Auth
	UpdateAuth(auth *coreauth.Auth) error
}

type ProxyManager struct {
	mu        sync.RWMutex
	cfg       config.ProxyManagerConfig
	engine    *AssignmentEngine
	checker   *HealthChecker
	detector  *BanDetector
	mutator   ProxyPoolMutator
	authStore AuthStore
	sdkCfg    *config.SDKConfig

	cancel context.CancelFunc
}

func NewProxyManager(cfg config.ProxyManagerConfig, mutator ProxyPoolMutator, authStore AuthStore, sdkCfg *config.SDKConfig) *ProxyManager {
	pool := mutator.GetPool()
	return &ProxyManager{
		cfg:       cfg,
		engine:    NewAssignmentEngine(cfg.Assignment, pool),
		checker:   NewHealthChecker(cfg.Health, sdkCfg),
		detector:  NewBanDetector(cfg.BanDetect),
		mutator:   mutator,
		authStore: authStore,
		sdkCfg:    sdkCfg,
	}
}

func (pm *ProxyManager) Start(ctx context.Context) {
	if !pm.IsEnabled() {
		return
	}

	ctx, pm.cancel = context.WithCancel(ctx)

	pm.runInitialAssignment(ctx)

	if pm.cfg.Health.Enabled {
		go pm.healthLoop(ctx)
	}

	log.Infof("proxy manager started (assignment=%v, health-check=%v, ban-detect=%v)",
		pm.cfg.Assignment.Enabled, pm.cfg.Health.Enabled, pm.cfg.BanDetect.Enabled)
}

func (pm *ProxyManager) Stop() {
	if pm.cancel != nil {
		pm.cancel()
	}
}

func (pm *ProxyManager) IsEnabled() bool {
	return pm.cfg.Assignment.Enabled || pm.cfg.Health.Enabled || pm.cfg.BanDetect.Enabled
}

func (pm *ProxyManager) runInitialAssignment(ctx context.Context) {
	if !pm.cfg.Assignment.Enabled || pm.authStore == nil {
		return
	}

	auths := pm.authStore.ListAuths()
	changed := pm.engine.Assign(auths)
	if len(changed) > 0 {
		pm.persistAuthChanges(changed)
	}
}

func (pm *ProxyManager) healthLoop(ctx context.Context) {
	interval := pm.checker.Interval()

	select {
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	pm.runHealthCheck(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pm.runHealthCheck(ctx)
		}
	}
}

func (pm *ProxyManager) runHealthCheck(ctx context.Context) {
	pool := pm.mutator.GetPool()

	disabled := pm.checker.CheckAll(ctx, pool)
	for _, proxyID := range disabled {
		if err := pm.disableAndReassign(proxyID); err != nil {
			log.Warnf("proxy manager: failed to disable proxy %s: %v", proxyID, err)
		}
	}

	recovered := pm.checker.RecoverDisabled(ctx, pool)
	for _, proxyID := range recovered {
		if pm.mutator.EnableProxy(proxyID) {
			pm.engine.UpdatePool(pm.mutator.GetPool())
			log.Infof("proxy manager: proxy %s recovered and re-enabled", proxyID)
		}
	}
}

func (pm *ProxyManager) disableAndReassign(proxyID string) error {
	if !pm.mutator.DisableProxy(proxyID) {
		return nil
	}
	pm.engine.UpdatePool(pm.mutator.GetPool())

	if pm.cfg.Assignment.Enabled && pm.authStore != nil {
		auths := pm.authStore.ListAuths()
		reassigned := pm.engine.Reassign(auths, proxyID)
		if len(reassigned) > 0 {
			pm.persistAuthChanges(reassigned)
		}
	}
	return nil
}

func (pm *ProxyManager) OnAuthRevoked(auth *coreauth.Auth) {
	if !pm.cfg.BanDetect.Enabled || auth == nil {
		return
	}

	proxyID := auth.ProxyID
	if proxyID == "" {
		return
	}

	reason := auth.StatusMessage
	if reason == "" {
		reason = "account_revoked"
	}

	_ = pm.detector.RecordBan(auth, reason)

	banCount, shouldDisable := pm.detector.CheckProxyBanned(proxyID)
	if shouldDisable {
		log.Warnf("proxy manager: proxy %s auto-disabled due to %d bans within window (threshold=%d)",
			proxyID, banCount, pm.cfg.BanDetect.BanThreshold)

		_ = pm.disableAndReassign(proxyID)
	} else if banCount > 0 {
		log.Warnf("proxy manager: proxy %s has %d/%d bans within window", proxyID, banCount, pm.cfg.BanDetect.BanThreshold)
	}
}

func (pm *ProxyManager) OnConfigReload(cfg *config.Config) {
	if cfg == nil {
		return
	}
	pool := pm.mutator.GetPool()
	pm.engine.UpdatePool(pool)
}

func (pm *ProxyManager) persistAuthChanges(auths []*coreauth.Auth) {
	if pm.authStore == nil {
		return
	}
	for _, a := range auths {
		if err := pm.authStore.UpdateAuth(a); err != nil {
			log.Warnf("proxy manager: failed to persist proxy assignment for auth %s: %v", a.ID, err)
		}
	}
}

func (pm *ProxyManager) GetHealthStatuses() map[string]*ProxyHealthStatus {
	if pm.checker == nil {
		return nil
	}
	return pm.checker.GetAllStatuses()
}

func (pm *ProxyManager) GetDistribution() []ProxyDistribution {
	if pm.authStore == nil {
		return nil
	}
	auths := pm.authStore.ListAuths()
	pool := pm.mutator.GetPool()
	return ComputeDistribution(auths, pool)
}

func (pm *ProxyManager) GetBanEvents(proxyID string, limit int) []BanEvent {
	if pm.detector == nil {
		return nil
	}
	return pm.detector.RecentEvents(proxyID, limit)
}

func (pm *ProxyManager) ForceHealthCheck(ctx context.Context) []string {
	if pm.checker == nil {
		return nil
	}
	pool := pm.mutator.GetPool()
	return pm.checker.CheckAll(ctx, pool)
}

func (pm *ProxyManager) TriggerAssignment() int {
	if !pm.cfg.Assignment.Enabled || pm.authStore == nil {
		return 0
	}
	auths := pm.authStore.ListAuths()
	changed := pm.engine.Assign(auths)
	if len(changed) > 0 {
		pm.persistAuthChanges(changed)
	}
	return len(changed)
}
