package auth

import "strings"

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
