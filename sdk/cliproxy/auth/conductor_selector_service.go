package auth

import (
	"context"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type selectionScope struct {
	tenantID        string
	cfg             *runtimeConfigSnapshot
	model           string
	modelKey        string
	pinnedAuthID    string
	allowedChannels map[string]struct{}
	allowedGroups   map[string]struct{}
	routeGroup      string
	routeFallback   string
}

func newSelectionScope(cfg *runtimeConfigSnapshot, model string, meta map[string]any) selectionScope {
	return selectionScope{
		tenantID:        tenantIDFromMetadata(meta),
		cfg:             cfg,
		model:           model,
		modelKey:        normalizeSelectionModelKey(model),
		pinnedAuthID:    pinnedAuthIDFromMetadata(meta),
		allowedChannels: allowedChannelsFromMetadata(meta),
		allowedGroups:   allowedChannelGroupsFromMetadata(meta),
		routeGroup:      routeGroupFromMetadata(meta),
		routeFallback:   routeFallbackFromMetadata(meta),
	}
}

const defaultTenantID = "00000000-0000-0000-0000-000000000001"

func normalizedTenantID(tenantID string) string {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return defaultTenantID
	}
	return tenantID
}

// NormalizedTenantID returns the canonical tenant scope used by runtime
// credential selection. Empty legacy records belong to the system tenant.
func NormalizedTenantID(tenantID string) string {
	return normalizedTenantID(tenantID)
}

func tenantIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return defaultTenantID
	}
	return normalizedTenantID(metadataStringValue(meta, cliproxyexecutor.TenantMetadataKey))
}

func tenantSelectionPrefix(meta map[string]any) string {
	return tenantIDFromMetadata(meta) + ":"
}

func normalizeSelectionModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := parseModelSuffix(model)
	if parsed.ModelName != "" {
		return strings.TrimSpace(parsed.ModelName)
	}
	return model
}

func (s selectionScope) initialRouteGroup() string {
	return effectiveRouteGroupForSelection(s.cfg, s.routeGroup, s.allowedGroups, s.modelKey)
}

func (s selectionScope) fallbackRouteGroup() string {
	if s.routeGroup == "" || s.routeFallback != "default" {
		return ""
	}
	return defaultRouteGroupForSelection(s.cfg, s.modelKey)
}

func (s selectionScope) routeGroupsToTry() []string {
	initial := s.initialRouteGroup()
	fallback := s.fallbackRouteGroup()
	if fallback == "" || fallback == initial {
		return []string{initial}
	}
	return []string{initial, fallback}
}

func (s selectionScope) allowsCandidate(candidate *Auth, scopedRouteGroup string, tried map[string]struct{}, registryRef ModelRegistry) bool {
	if candidate == nil || candidate.Disabled || candidate.Status == StatusDisabled {
		return false
	}
	if normalizedTenantID(candidate.TenantID) != s.tenantID {
		return false
	}
	if s.pinnedAuthID != "" && candidate.ID != s.pinnedAuthID {
		return false
	}
	if !authAllowedByChannels(candidate, s.allowedChannels) {
		return false
	}
	if !authAllowedByGroups(s.cfg, candidate, s.allowedGroups) {
		return false
	}
	if scopedRouteGroup != "" && !authInRouteGroup(s.cfg, candidate, scopedRouteGroup) {
		return false
	}
	if _, used := tried[candidate.ID]; used {
		return false
	}
	if s.modelKey != "" && !candidateSupportsModel(s.cfg, registryRef, candidate, s.modelKey, scopedRouteGroup, s.allowedGroups) {
		return false
	}
	return true
}

type selectorService struct {
	manager *Manager
}

func newSelectorService(manager *Manager) selectorService {
	return selectorService{manager: manager}
}

func optionsForSelectionRouteGroup(opts cliproxyexecutor.Options, routeGroup string) cliproxyexecutor.Options {
	next := opts
	meta := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		meta[key] = value
	}
	if routeGroup != "" {
		meta[cliproxyexecutor.RouteGroupMetadataKey] = routeGroup
	} else {
		delete(meta, cliproxyexecutor.RouteGroupMetadataKey)
	}
	next.Metadata = meta
	return next
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	return newSelectorService(m).pickProvider(ctx, provider, model, opts, tried)
}

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	return newSelectorService(m).pickMixed(ctx, providers, model, opts, tried)
}

func (s selectorService) pickProvider(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	scope := newSelectionScope(s.manager.currentRuntimeConfigForTenant(tenantIDFromMetadata(opts.Metadata)), model, opts.Metadata)

	s.manager.mu.RLock()
	executor := s.manager.executorForTenantLocked(scope.tenantID, provider)
	okExecutor := executor != nil
	if !okExecutor {
		s.manager.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	selected, _, err := s.pickLocked(ctx, scope, provider, opts, tried, func(candidate *Auth) bool {
		return candidate != nil && candidate.Provider == provider
	})
	if err != nil {
		s.manager.mu.RUnlock()
		return nil, nil, err
	}
	authCopy := selected.Clone()
	s.manager.mu.RUnlock()

	return s.ensureSelectedAuthIndex(authCopy, selected), executor, nil
}

func (s selectorService) pickMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	scope := newSelectionScope(s.manager.currentRuntimeConfigForTenant(tenantIDFromMetadata(opts.Metadata)), model, opts.Metadata)
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	s.manager.mu.RLock()
	selected, _, err := s.pickLocked(ctx, scope, "mixed", opts, tried, func(candidate *Auth) bool {
		if candidate == nil {
			return false
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			return false
		}
		if _, ok := providerSet[providerKey]; !ok {
			return false
		}
		if s.manager.executorForTenantLocked(scope.tenantID, providerKey) == nil {
			return false
		}
		return true
	})
	if err != nil {
		s.manager.mu.RUnlock()
		return nil, nil, "", err
	}

	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	executor := s.manager.executorForTenantLocked(scope.tenantID, providerKey)
	okExecutor := executor != nil
	if !okExecutor {
		s.manager.mu.RUnlock()
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	s.manager.mu.RUnlock()

	return s.ensureSelectedAuthIndex(authCopy, selected), executor, providerKey, nil
}

func (s selectorService) pickLocked(
	ctx context.Context,
	scope selectionScope,
	selectorProvider string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
	includeCandidate func(candidate *Auth) bool,
) (*Auth, string, error) {
	registryRef := s.manager.modelRegistry
	var diagnosis error
	for _, selectorRouteGroup := range scope.routeGroupsToTry() {
		candidates := s.buildCandidatesLocked(scope, selectorRouteGroup, tried, registryRef, includeCandidate)
		if len(candidates) == 0 {
			if diagnosis == nil {
				diagnosis = s.diagnoseEmptyCandidates(scope, selectorRouteGroup, registryRef, includeCandidate)
			}
			continue
		}
		selector := s.manager.selectorForRoutingScopeLocked(scope.cfg, selectorRouteGroup, scope.allowedGroups)
		if s.manager.concurrencyLimiter != nil {
			candidates = s.manager.concurrencyLimiter.FilterAvailableCandidates(candidates)
		}
		selected, errPick := selector.Pick(ctx, selectorProvider, scope.model, optionsForSelectionRouteGroup(opts, selectorRouteGroup), candidates)
		if errPick != nil {
			return nil, "", errPick
		}
		if selected == nil {
			return nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		return selected, selectorRouteGroup, nil
	}
	// A specific cause beats the generic message: "no auth available" reads as
	// "the account is missing" when the account is present and healthy.
	if diagnosis != nil {
		return nil, "", diagnosis
	}
	return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
}

func (s selectorService) buildCandidatesLocked(
	scope selectionScope,
	scopedRouteGroup string,
	tried map[string]struct{},
	registryRef ModelRegistry,
	includeCandidate func(candidate *Auth) bool,
) []*Auth {
	candidates := make([]*Auth, 0, len(s.manager.auths))
	for _, candidate := range s.manager.auths {
		if !scope.allowsCandidate(candidate, scopedRouteGroup, tried, registryRef) {
			continue
		}
		if includeCandidate != nil && !includeCandidate(candidate) {
			continue
		}
		candidates = append(candidates, prepareCandidateForSelection(scope.cfg, candidate, scopedRouteGroup, scope.allowedGroups))
	}
	return candidates
}

func (s selectorService) ensureSelectedAuthIndex(authCopy *Auth, selected *Auth) *Auth {
	if authCopy == nil || selected == nil || selected.indexAssigned {
		return authCopy
	}
	s.manager.mu.Lock()
	if current := s.manager.auths[authCopy.ID]; current != nil && !current.indexAssigned {
		current.EnsureIndex()
		authCopy = current.Clone()
	}
	s.manager.mu.Unlock()
	return authCopy
}
