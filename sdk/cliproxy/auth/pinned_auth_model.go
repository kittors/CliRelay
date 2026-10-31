package auth

import "strings"

// AuthServesModel reports whether the credential authID could be selected to
// serve model for one of providers on behalf of tenantID.
//
// It applies only the checks that do not change from one request to the next:
// provider, enablement, tenant visibility and the model list, using the same
// model-support rule as selection (thinking suffixes stripped, channel-group
// prefixes and credentials without a registered model list handled). Cooldown,
// quota and concurrency are deliberately ignored, so a caller that keeps a
// credential pinned across requests does not lose the pin merely because the
// account is busy for a moment.
func (m *Manager) AuthServesModel(tenantID, authID string, providers []string, model string) bool {
	authID = strings.TrimSpace(authID)
	modelKey := normalizeSelectionModelKey(model)
	if m == nil || authID == "" || modelKey == "" {
		return false
	}
	tenantID = normalizedTenantID(tenantID)
	cfg := m.currentRuntimeConfigForTenant(tenantID)
	grants := m.sharedGrantIndex()

	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if !authUsableByTenant(auth, tenantID, grants) {
		return false
	}
	if len(providers) > 0 {
		authProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
		matched := false
		for _, provider := range providers {
			if strings.ToLower(strings.TrimSpace(provider)) == authProvider {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return candidateSupportsModel(cfg, m.modelRegistry, auth, modelKey, "", nil)
}
