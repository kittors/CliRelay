package auth

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	m.store = store
	m.mu.Unlock()
	m.adoptStoreRefreshCoordinator(store)
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// RegisterExecutor registers a provider executor for the system tenant.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	m.RegisterExecutorForTenant(defaultTenantID, executor)
}

// RegisterExecutorForTenant registers a provider executor for one tenant runtime config.
func (m *Manager) RegisterExecutorForTenant(tenantID string, executor ProviderExecutor) {
	if m == nil || executor == nil {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(executor.Identifier()))
	if provider == "" {
		return
	}
	tenantID = normalizedTenantID(tenantID)

	var replaced ProviderExecutor
	m.mu.Lock()
	if tenantID == defaultTenantID {
		replaced = m.executors[provider]
		m.executors[provider] = executor
	} else {
		byProvider := m.tenantExecutors[tenantID]
		if byProvider == nil {
			byProvider = make(map[string]ProviderExecutor)
			m.tenantExecutors[tenantID] = byProvider
		}
		replaced = byProvider[provider]
		byProvider[provider] = executor
	}
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the system-tenant executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	m.UnregisterExecutorForTenant(defaultTenantID, provider)
}

// UnregisterExecutorForTenant removes one tenant's executor association.
func (m *Manager) UnregisterExecutorForTenant(tenantID, provider string) {
	if m == nil {
		return
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	tenantID = normalizedTenantID(tenantID)
	m.mu.Lock()
	if tenantID == defaultTenantID {
		delete(m.executors, provider)
	} else if byProvider := m.tenantExecutors[tenantID]; byProvider != nil {
		delete(byProvider, provider)
		if len(byProvider) == 0 {
			delete(m.tenantExecutors, tenantID)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) executorForTenantLocked(tenantID, provider string) ProviderExecutor {
	if m == nil {
		return nil
	}
	tenantID = normalizedTenantID(tenantID)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if tenantID == defaultTenantID {
		return m.executors[provider]
	}
	return m.tenantExecutors[tenantID][provider]
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	m.pinStoreIndex(auth)
	auth.EnsureIndex()
	snapshot := auth.Clone()
	m.mu.Lock()
	m.auths[auth.ID] = snapshot
	m.mu.Unlock()
	if err := m.persist(ctx, snapshot); err != nil {
		m.mu.Lock()
		delete(m.auths, auth.ID)
		m.mu.Unlock()
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
		return nil, err
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.hook.OnAuthRegistered(ctx, snapshot.Clone())
	return snapshot.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	var previous *Auth
	m.mu.Lock()
	m.pinStoreIndex(auth)
	if existing, ok := m.auths[auth.ID]; ok && existing != nil && !auth.indexAssigned && auth.Index == "" {
		auth.Index = existing.Index
		auth.indexAssigned = existing.indexAssigned
	}
	if existing, ok := m.auths[auth.ID]; ok && existing != nil {
		previous = existing.Clone()
	}
	auth.EnsureIndex()
	snapshot := auth.Clone()
	if previous != nil {
		preserveRuntimeFields(snapshot, previous)
		preserveAvailabilityRuntimeForUpdate(snapshot, previous, time.Now())
	}
	m.auths[auth.ID] = snapshot
	m.mu.Unlock()
	if err := m.persist(ctx, snapshot); err != nil {
		m.mu.Lock()
		if previous != nil {
			m.auths[auth.ID] = previous
		} else {
			delete(m.auths, auth.ID)
		}
		m.mu.Unlock()
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
		return nil, err
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
	return snapshot.Clone(), nil
}

// Delete removes an auth entry from the runtime manager and persistence store.
func (m *Manager) Delete(ctx context.Context, id string) (*Auth, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	m.mu.Lock()
	previous, ok := m.auths[id]
	if !ok || previous == nil {
		m.mu.Unlock()
		return nil, nil
	}
	snapshot := previous.Clone()
	delete(m.auths, id)
	m.mu.Unlock()

	if err := m.deletePersist(ctx, snapshot); err != nil {
		m.mu.Lock()
		m.auths[id] = snapshot
		m.mu.Unlock()
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
		return nil, err
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if hook, ok := m.hook.(AuthDeletedHook); ok {
		hook.OnAuthDeleted(ctx, snapshot.Clone())
	}
	return snapshot.Clone(), nil
}

// Load resets manager state from the backing store. Lifecycle callbacks run
// after releasing the manager lock so database-backed hooks cannot deadlock auth
// selection or refresh paths.
func (m *Manager) Load(ctx context.Context) error {
	items, err := m.loadPersistedAuths(ctx)
	if err != nil {
		return err
	}
	loaded := make([]*Auth, 0, len(items))
	m.mu.Lock()
	m.auths = make(map[string]*Auth, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		m.pinStoreIndex(auth)
		auth.EnsureIndex()
		snapshot := auth.Clone()
		m.auths[auth.ID] = snapshot
		loaded = append(loaded, snapshot.Clone())
	}
	m.rebuildAPIKeyModelAliasLocked()
	m.mu.Unlock()
	if hook, ok := m.hook.(AuthLoadedHook); ok {
		for _, auth := range loaded {
			hook.OnAuthLoaded(ctx, auth)
		}
	}
	return nil
}
