package auth

import (
	"strings"

	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

type ModelRegistry interface {
	ClearModelQuotaExceeded(clientID, modelID string)
	SetModelQuotaExceeded(clientID, modelID string)
	SuspendClientModel(clientID, modelID string, reason string)
	ResumeClientModel(clientID, modelID string)
	ClientSupportsModel(clientID, modelID string) bool
	GetModelsForClient(clientID string) []*sdkmodelcatalog.ModelInfo
}

func (m *Manager) resumeRecoveredQuotaModels(authID string, models []string) {
	if len(models) == 0 || strings.TrimSpace(authID) == "" {
		return
	}
	registryRef := m.modelRegistry
	if registryRef == nil {
		return
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		registryRef.ClearModelQuotaExceeded(authID, model)
		registryRef.ResumeClientModel(authID, model)
	}
}

// KnownChannelGroups returns the currently known explicit and implicit channel groups.
func (m *Manager) KnownChannelGroups() map[string]struct{} {
	return m.KnownChannelGroupsForTenant(defaultTenantID)
}
func (m *Manager) KnownChannelGroupsForTenant(tenantID string) map[string]struct{} {
	tenantID = normalizedTenantID(tenantID)
	out := make(map[string]struct{})
	if m == nil {
		return out
	}
	cfg := m.currentRuntimeConfigForTenant(tenantID)
	if cfg != nil {
		for i := range cfg.Routing.ChannelGroups {
			if name := normalizeGroupName(cfg.Routing.ChannelGroups[i].Name); name != "" {
				out[name] = struct{}{}
			}
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil || normalizedTenantID(auth.TenantID) != tenantID {
			continue
		}
		for group := range authGroups(cfg, auth) {
			out[group] = struct{}{}
		}
	}
	return out
}

// ServeModelScopeOptions tunes CanServeModelWithScopes* for management UI paths.
type ServeModelScopeOptions struct {
	// IgnoreGroupAllowedModels skips channel-group AllowedModels checks so the
	// channel-group editor can list every model the group's channels can serve,
	// not only models already on the saved allow-list.
	IgnoreGroupAllowedModels bool
}

// CanServeModelWithScopes reports whether at least one active auth can serve the model under the given restrictions.
func (m *Manager) CanServeModelWithScopes(modelID string, allowedChannels, allowedGroups map[string]struct{}, routeGroup string) bool {
	return m.CanServeModelWithScopesForTenant(defaultTenantID, modelID, allowedChannels, allowedGroups, routeGroup)
}
func (m *Manager) CanServeModelWithScopesForTenant(tenantID, modelID string, allowedChannels, allowedGroups map[string]struct{}, routeGroup string) bool {
	return m.CanServeModelWithScopesForTenantOpts(tenantID, modelID, allowedChannels, allowedGroups, routeGroup, ServeModelScopeOptions{})
}

func (m *Manager) CanServeModelWithScopesForTenantOpts(tenantID, modelID string, allowedChannels, allowedGroups map[string]struct{}, routeGroup string, opts ServeModelScopeOptions) bool {
	tenantID = normalizedTenantID(tenantID)
	modelID = strings.TrimSpace(modelID)
	if modelID == "" || m == nil {
		return false
	}
	registryRef := m.modelRegistry
	cfg := m.currentRuntimeConfigForTenant(tenantID)
	selectionRouteGroup := effectiveRouteGroupForSelection(cfg, routeGroup, allowedGroups, modelID)

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, candidate := range m.auths {
		if candidate == nil || normalizedTenantID(candidate.TenantID) != tenantID || candidate.Disabled || candidate.Status == StatusDisabled {
			continue
		}
		if !authAllowedByChannels(candidate, allowedChannels) {
			continue
		}
		if !authAllowedByGroups(cfg, candidate, allowedGroups) {
			continue
		}
		if !authInRouteGroup(cfg, candidate, selectionRouteGroup) {
			continue
		}
		if candidateSupportsModelOpts(cfg, registryRef, candidate, modelID, selectionRouteGroup, allowedGroups, opts) {
			return true
		}
	}
	return false
}
