package auth

func (m *Manager) executorForTenant(tenantID, provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executorForTenantLocked(tenantID, provider)
}
