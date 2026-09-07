package auth

import (
	"strings"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

func (m *Manager) ListForTenant(tenantID string) []*Auth {
	if m == nil {
		return nil
	}
	tenantID = normalizedTenantID(tenantID)
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0)
	for _, auth := range m.auths {
		if auth != nil && normalizedTenantID(auth.TenantID) == tenantID {
			list = append(list, auth.Clone())
		}
	}
	return list
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.mu.Lock()
	m.selector = selector
	if roundRobinSelector, ok := selector.(*RoundRobinSelector); ok {
		m.roundRobinSelector = roundRobinSelector
	}
	if fillFirstSelector, ok := selector.(*FillFirstSelector); ok {
		m.fillFirstSelector = fillFirstSelector
	}
	if sessionStickySelector, ok := selector.(*SessionStickySelector); ok {
		m.sessionStickySelector = sessionStickySelector
	}
	if m.roundRobinSelector == nil {
		m.roundRobinSelector = &RoundRobinSelector{}
	}
	if m.fillFirstSelector == nil {
		m.fillFirstSelector = &FillFirstSelector{}
	}
	if m.leastLoadSelector == nil {
		m.leastLoadSelector = &LeastLoadSelector{}
	}
	if m.sessionStickySelector == nil {
		m.sessionStickySelector = NewSessionStickySelector(m.roundRobinSelector)
	}
	// A replaced selector must keep observing load, otherwise least-load and the
	// sticky release threshold silently lose their inputs after a config reload.
	if m.scheduler != nil {
		m.roundRobinSelector.deps = m.scheduler
		m.fillFirstSelector.deps = m.scheduler
		m.leastLoadSelector.deps = m.scheduler
		m.sessionStickySelector.deps = m.scheduler
	}
	if m.sessionStickySelector.distribution == nil {
		m.sessionStickySelector.distribution = func(name string) Selector {
			return m.distributionSelectorLocked(name)
		}
	}
	m.mu.Unlock()
}

// SetQuotaLoadSource wires the usage layer's quota snapshot into scheduling.
// Least-load uses it to steer traffic away from accounts nearing their window
// ceiling, and session stickiness uses it for early binding release.
func (m *Manager) SetQuotaLoadSource(source QuotaLoadSource) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.scheduler == nil {
		m.scheduler = &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: m.concurrencyLimiter}
	}
	m.scheduler.quota = source
	m.mu.Unlock()
}

// distributionSelectorLocked returns the singleton selector implementing one
// distribution mode. Singletons matter: the cursors, pressure counters and
// sticky bindings are what make the distribution behave consistently across
// requests.
func (m *Manager) distributionSelectorLocked(distribution string) Selector {
	if m == nil {
		return &RoundRobinSelector{}
	}
	switch sdkconfig.NormalizeDistribution(distribution) {
	case sdkconfig.DistributionFillFirst:
		if m.fillFirstSelector != nil {
			return m.fillFirstSelector
		}
		return &FillFirstSelector{}
	case sdkconfig.DistributionLeastLoad:
		if m.leastLoadSelector != nil {
			return m.leastLoadSelector
		}
		return &LeastLoadSelector{}
	default:
		if m.roundRobinSelector != nil {
			return m.roundRobinSelector
		}
		return &RoundRobinSelector{}
	}
}

// selectorForRoutingScopeLocked picks the selector for this request's routing
// scope. Session stickiness wraps a distribution mode rather than replacing it,
// so an operator can combine "keep a conversation on one account" with any of
// the balancing behaviours.
func (m *Manager) selectorForRoutingScopeLocked(cfg *runtimeConfigSnapshot, routeGroup string, allowedGroups map[string]struct{}) Selector {
	if m == nil {
		return &RoundRobinSelector{}
	}
	scheduling := scopedScheduling(cfg, routeGroup, allowedGroups)
	if scheduling.StickyEnabled {
		if m.sessionStickySelector != nil {
			return m.sessionStickySelector
		}
		return NewSessionStickySelector(m.distributionSelectorLocked(scheduling.Distribution))
	}
	return m.distributionSelectorLocked(scheduling.Distribution)
}

func authAllowedByChannels(auth *Auth, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		return true
	}
	if auth == nil {
		return false
	}
	for _, identifier := range auth.ChannelIdentifiers() {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(identifier))]; ok {
			return true
		}
	}
	return false
}

// CanServeModelWithChannels reports whether at least one active auth in the allowed channel set supports modelID.
func (m *Manager) CanServeModelWithChannels(modelID string, allowed map[string]struct{}) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	return m.CanServeModelWithScopes(modelID, allowed, nil, "")
}

// GetByID retrieves an auth entry by its ID.
func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// Executor returns the registered system-tenant executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	return m.ExecutorForTenant(defaultTenantID, provider)
}

// ExecutorForTenant returns the executor bound to one tenant runtime config.
func (m *Manager) ExecutorForTenant(tenantID, provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	executor := m.executorForTenantLocked(tenantID, provider)
	return executor, executor != nil
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.RLock()
	executors := make([]ProviderExecutor, 0, len(m.executors)+len(m.tenantExecutors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	for _, byProvider := range m.tenantExecutors {
		for _, exec := range byProvider {
			executors = append(executors, exec)
		}
	}
	m.mu.RUnlock()

	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}
