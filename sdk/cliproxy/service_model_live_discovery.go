package cliproxy

import (
	"context"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
	log "github.com/sirupsen/logrus"
)

// Upstream model discovery feeding registration.
//
// A Codex credential registers the compiled-in catalog plus whatever ChatGPT's
// model manifest last listed for its tenant. The catalog is the floor: models the
// manifest never reports (image models) or drops for a while stay routable. The
// manifest is how a model ChatGPT ships today becomes routable today, without a
// catalog edit and a release. Before this, discovery fed only the panels, so every
// new model needed a hand-written catalog entry (#990 for gpt-6-astra), and one
// without it — gpt-6-sol — was listed in the catalog while every request for it
// failed with "no provider serves model".
//
// Fetching never blocks registration. Credentials register from whatever list is
// stored, a background worker keeps the lists current, and a changed list
// re-registers the credentials of that tenant and provider.

const (
	// providerDiscoveryInterval is how often every tenant's lists are re-fetched.
	providerDiscoveryInterval = 30 * time.Minute
	// providerDiscoveryRetryBackoff spaces out on-demand fetches for a list whose
	// last attempt failed, so a credential re-registering in a loop cannot become an
	// upstream request loop.
	providerDiscoveryRetryBackoff = 5 * time.Minute
)

type providerDiscoveryState struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	stopped   bool
	wg        sync.WaitGroup
	inflight  map[string]bool
	lastTried map[string]time.Time
	// ready is closed when the startup registration pass has finished. A change
	// arriving earlier waits for it, so that pass — which may still be registering
	// from the empty startup snapshot — cannot overwrite the merged result.
	ready     chan struct{}
	readyOnce sync.Once
}

type providerDiscoveryTarget struct {
	tenantID string
	provider string
}

// withLiveDiscoveredModels merges the tenant's stored upstream list into the models
// a credential registers.
func (s *Service) withLiveDiscoveredModels(a *coreauth.Auth, provider, authKind string, models []*ModelInfo) []*ModelInfo {
	if a == nil || !serviceapp.ProviderDiscoveryDrivesRouting(provider) {
		return models
	}
	if strings.EqualFold(strings.TrimSpace(authKind), "apikey") || !serviceapp.IsProviderDiscoveryCredential(a, provider) {
		return models
	}
	live := serviceapp.ProviderDiscoverySnapshot(a.TenantID, provider)
	if len(live) == 0 {
		// Nothing stored for this tenant yet: its first credential, or a start whose
		// first fetch has not answered. Ask for one; its arrival re-registers this
		// credential.
		s.requestProviderDiscovery(a.TenantID, provider)
		return models
	}
	return mergeLiveDiscoveredModels(models, live)
}

// mergeLiveDiscoveredModels appends the discovered models the base set lacks. A
// model already in the base keeps its catalog definition, which describes
// capabilities the manifest does not.
func mergeLiveDiscoveredModels(models, live []*ModelInfo) []*ModelInfo {
	if len(live) == 0 {
		return models
	}
	seen := make(map[string]struct{}, len(models)+len(live))
	out := make([]*ModelInfo, 0, len(models)+len(live))
	for _, model := range models {
		if model == nil {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(model.ID))] = struct{}{}
		out = append(out, model)
	}
	for _, model := range live {
		if model == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(model.ID))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	return out
}

// startProviderDiscovery begins keeping the routing providers' lists current. It
// runs before the startup registration pass so the first fetches overlap it.
func (s *Service) startProviderDiscovery(parent context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	st := &s.providerDiscovery
	st.mu.Lock()
	if st.ctx != nil {
		st.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	st.ctx, st.cancel = ctx, cancel
	st.inflight = make(map[string]bool)
	st.lastTried = make(map[string]time.Time)
	st.ready = make(chan struct{})
	st.wg.Add(1)
	st.mu.Unlock()

	serviceapp.SetProviderDiscoveryChangeHook(s.onProviderDiscoveryChanged)

	go func() {
		defer st.wg.Done()
		ticker := time.NewTicker(providerDiscoveryInterval)
		defer ticker.Stop()
		for {
			for _, target := range s.providerDiscoveryTargets() {
				s.launchProviderDiscovery(target, true)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// markProviderDiscoveryRegistrationReady releases the re-registrations held back
// during the startup pass.
func (s *Service) markProviderDiscoveryRegistrationReady() {
	st := &s.providerDiscovery
	st.mu.Lock()
	ready := st.ready
	st.mu.Unlock()
	if ready == nil {
		return
	}
	st.readyOnce.Do(func() { close(ready) })
}

// waitProviderDiscoveryRegistrationReady blocks until the startup registration pass
// has finished. It reports false when discovery stopped first.
func (s *Service) waitProviderDiscoveryRegistrationReady() bool {
	st := &s.providerDiscovery
	st.mu.Lock()
	ready, ctx := st.ready, st.ctx
	st.mu.Unlock()
	if ready == nil {
		return true
	}
	select {
	case <-ready:
		return true
	case <-ctx.Done():
		return false
	}
}

// stopProviderDiscovery cancels the worker and every fetch it started, then waits
// for them to return.
func (s *Service) stopProviderDiscovery() {
	st := &s.providerDiscovery
	st.mu.Lock()
	if st.ctx == nil || st.stopped {
		st.mu.Unlock()
		return
	}
	st.stopped = true
	st.cancel()
	st.mu.Unlock()
	serviceapp.SetProviderDiscoveryChangeHook(nil)
	st.wg.Wait()
}

// providerDiscoveryTargets lists each tenant and routing provider pair that has a
// credential able to fetch the list.
func (s *Service) providerDiscoveryTargets() []providerDiscoveryTarget {
	if s == nil || s.coreManager == nil {
		return nil
	}
	seen := make(map[providerDiscoveryTarget]struct{})
	var targets []providerDiscoveryTarget
	for _, auth := range s.coreManager.List() {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if !serviceapp.ProviderDiscoveryDrivesRouting(provider) || !serviceapp.IsProviderDiscoveryCredential(auth, provider) {
			continue
		}
		target := providerDiscoveryTarget{tenantID: coreauth.NormalizedTenantID(auth.TenantID), provider: provider}
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	return targets
}

// requestProviderDiscovery asks for one list outside the periodic schedule.
func (s *Service) requestProviderDiscovery(tenantID, provider string) {
	s.launchProviderDiscovery(providerDiscoveryTarget{
		tenantID: coreauth.NormalizedTenantID(tenantID),
		provider: strings.ToLower(strings.TrimSpace(provider)),
	}, false)
}

// launchProviderDiscovery fetches one list in the background. Nothing starts before
// startProviderDiscovery or after stopProviderDiscovery, or while a fetch for the
// same list is running; on-demand requests also respect the retry backoff.
func (s *Service) launchProviderDiscovery(target providerDiscoveryTarget, periodic bool) {
	if s == nil || s.coreManager == nil {
		return
	}
	st := &s.providerDiscovery
	key := target.tenantID + "|" + target.provider
	st.mu.Lock()
	ctx := st.ctx
	if ctx == nil || ctx.Err() != nil || st.stopped || st.inflight[key] {
		st.mu.Unlock()
		return
	}
	if last, tried := st.lastTried[key]; !periodic && tried && time.Since(last) < providerDiscoveryRetryBackoff {
		st.mu.Unlock()
		return
	}
	st.inflight[key] = true
	st.lastTried[key] = time.Now()
	st.wg.Add(1)
	st.mu.Unlock()

	go func() {
		defer st.wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Errorf("model discovery panicked: provider=%s tenant=%s err=%v", target.provider, target.tenantID, recovered)
			}
			st.mu.Lock()
			delete(st.inflight, key)
			st.mu.Unlock()
		}()
		if !serviceapp.RefreshProviderDiscovery(ctx, s.coreManager, s.configSnapshot(), target.tenantID, target.provider) && ctx.Err() == nil {
			log.Warnf("model discovery: %s listed no models for tenant %s; keeping the last known list", target.provider, target.tenantID)
		}
	}()
}

// onProviderDiscoveryChanged re-registers the credentials a changed list applies
// to. Bursts coalesce the same way model-library changes do.
func (s *Service) onProviderDiscoveryChanged(tenantID, provider string) {
	if s == nil || s.coreManager == nil {
		return
	}
	key := "discovery|" + tenantID + "|" + provider
	if !s.catalogRefresh.begin(key) {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Errorf("model discovery re-registration panicked: provider=%s tenant=%s err=%v", provider, tenantID, recovered)
				s.catalogRefresh.abandon(key)
			}
		}()
		if !s.waitProviderDiscoveryRegistrationReady() {
			s.catalogRefresh.abandon(key)
			return
		}
		for {
			s.refreshRegisteredModelsForProvider(context.Background(), tenantID, provider)
			if !s.catalogRefresh.finish(key) {
				return
			}
		}
	}()
}

// refreshRegisteredModelsForProvider re-registers one provider's credentials in a tenant.
func (s *Service) refreshRegisteredModelsForProvider(ctx context.Context, tenantID, provider string) {
	if s == nil || s.coreManager == nil {
		return
	}
	for _, auth := range s.coreManager.ListForTenant(tenantID) {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		s.registerModelsForAuth(ctx, auth)
	}
}

func (s *Service) configSnapshot() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}
