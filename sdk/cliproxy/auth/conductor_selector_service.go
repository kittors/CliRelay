package auth

import (
	"context"
	"strconv"
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
	// tenantWide drops the implicit "default" group a request without a route
	// group would otherwise be placed in.
	tenantWide bool
	// grants are the system credentials shared with other tenants, captured
	// once per selection so every candidate is judged against the same set.
	grants *sharedAuthGrantIndex
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
		tenantWide:      tenantWideSelectionFromMetadata(meta),
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
	// With no route group the scope is empty, which also skips the group model
	// lists: modelAllowedByRoutingGroupScopes only consults groups in scope. An
	// explicit route group still wins.
	if s.tenantWide && s.routeGroup == "" {
		return ""
	}
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
	return s.candidateExclusion(candidate, scopedRouteGroup, tried, registryRef) == ""
}

// candidateExclusion names the first gate that keeps candidate out of selection
// in this scope, or returns "" for a candidate. Selection and the routing
// simulator read the same verdict, so the simulator cannot explain a decision
// selection did not make.
func (s selectionScope) candidateExclusion(candidate *Auth, scopedRouteGroup string, tried map[string]struct{}, registryRef ModelRegistry) CandidateExclusion {
	if candidate == nil || candidate.Disabled || candidate.Status == StatusDisabled {
		return ExcludedDisabled
	}
	if !authUsableByTenant(candidate, s.tenantID, s.grants) {
		return ExcludedTenant
	}
	if s.pinnedAuthID != "" && candidate.ID != s.pinnedAuthID {
		return ExcludedPinned
	}
	if !authAllowedByChannels(candidate, s.allowedChannels) {
		return ExcludedChannel
	}
	if !authAllowedByGroups(s.cfg, candidate, s.allowedGroups) {
		return ExcludedAllowedGroups
	}
	if scopedRouteGroup != "" && !authInRouteGroup(s.cfg, candidate, scopedRouteGroup) {
		return ExcludedRouteGroup
	}
	if _, used := tried[candidate.ID]; used {
		return ExcludedTried
	}
	if s.modelKey != "" && !candidateSupportsModel(s.cfg, registryRef, candidate, s.modelKey, scopedRouteGroup, s.allowedGroups) {
		return ExcludedModel
	}
	return ""
}

type selectorService struct {
	manager *Manager
}

func newSelectorService(manager *Manager) selectorService {
	return selectorService{manager: manager}
}

// optionsForSelectionScope stamps the resolved route group and the scheduling
// settings that govern it onto the request options. The Selector interface is
// part of the SDK contract and cannot grow a parameter, so per-group settings
// ride along in the metadata.
func optionsForSelectionScope(opts cliproxyexecutor.Options, routeGroup string, scheduling runtimeGroupScheduling) cliproxyexecutor.Options {
	next := opts
	meta := make(map[string]any, len(opts.Metadata)+5)
	for key, value := range opts.Metadata {
		meta[key] = value
	}
	if routeGroup != "" {
		meta[cliproxyexecutor.RouteGroupMetadataKey] = routeGroup
	} else {
		delete(meta, cliproxyexecutor.RouteGroupMetadataKey)
	}
	meta[distributionMetadataKey] = scheduling.Distribution
	if scheduling.StickyEnabled {
		meta[stickyEnabledMetadataKey] = "true"
		if scheduling.StickyMax > 0 {
			meta[stickyMaxRequestsKey] = strconv.Itoa(scheduling.StickyMax)
		}
		if scheduling.StickyRelease > 0 {
			meta[stickyReleaseAtLoadKey] = strconv.FormatFloat(scheduling.StickyRelease, 'f', -1, 64)
		}
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
	scope.grants = s.manager.sharedGrantIndex()

	s.manager.mu.RLock()
	// A tenant with no credential of this provider has no executor for it, yet
	// may still be served by a system credential shared with it. Every
	// candidate runs on its owner's executor; see shared_grants.go.
	tenantHasExecutor := s.manager.executorForTenantLocked(scope.tenantID, provider) != nil
	selected, _, err := s.pickLocked(ctx, scope, provider, opts, tried, func(candidate *Auth) bool {
		return candidate != nil && candidate.Provider == provider && s.manager.executorForAuthLocked(candidate, provider) != nil
	}, s.manager.selectorForRoutingScopeLocked)
	if err != nil {
		notServed := !tenantHasExecutor && !s.manager.sharesProviderWithTenantLocked(scope.tenantID, provider, scope.grants)
		s.manager.mu.RUnlock()
		if notServed {
			// Nothing of its own and nothing shared: keep the error a tenant
			// without this provider has always received. A shared credential
			// that exists but cannot serve right now (cooldown, model gate)
			// reports its own reason instead.
			return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		return nil, nil, err
	}
	executor := s.manager.executorForAuthLocked(selected, provider)
	authCopy := selected.Clone()
	s.manager.mu.RUnlock()

	return s.ensureSelectedAuthIndex(authCopy, selected), executor, nil
}

func (s selectorService) pickMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	scope := newSelectionScope(s.manager.currentRuntimeConfigForTenant(tenantIDFromMetadata(opts.Metadata)), model, opts.Metadata)
	scope.grants = s.manager.sharedGrantIndex()
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
	selected, _, err := s.pickLocked(ctx, scope, "mixed", opts, tried, s.mixedCandidateFilterLocked(providerSet), s.manager.selectorForRoutingScopeLocked)
	if err != nil {
		s.manager.mu.RUnlock()
		return nil, nil, "", err
	}

	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	executor := s.manager.executorForAuthLocked(selected, providerKey)
	okExecutor := executor != nil
	if !okExecutor {
		s.manager.mu.RUnlock()
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	s.manager.mu.RUnlock()

	return s.ensureSelectedAuthIndex(authCopy, selected), executor, providerKey, nil
}

// mixedCandidateFilterLocked admits the credentials of the given providers that
// have an executor, the candidate set of a mixed-provider request.
func (s selectorService) mixedCandidateFilterLocked(providerSet map[string]struct{}) func(candidate *Auth) bool {
	return func(candidate *Auth) bool {
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
		if s.manager.executorForAuthLocked(candidate, providerKey) == nil {
			return false
		}
		return true
	}
}

// selectorSource returns the selector that serves one routing scope. Requests
// get the manager's live selectors; the routing simulator passes copies whose
// scheduling state it may advance without touching live traffic.
type selectorSource func(cfg *runtimeConfigSnapshot, routeGroup string, allowedGroups map[string]struct{}) Selector

func (s selectorService) pickLocked(
	ctx context.Context,
	scope selectionScope,
	selectorProvider string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
	includeCandidate func(candidate *Auth) bool,
	selectorFor selectorSource,
) (*Auth, string, error) {
	includeCandidate = withSelectionCapabilities(opts, includeCandidate)
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
		scheduling := scopedScheduling(scope.cfg, selectorRouteGroup, scope.allowedGroups)
		selector := selectorFor(scope.cfg, selectorRouteGroup, scope.allowedGroups)
		if s.manager.concurrencyLimiter != nil {
			candidates = s.manager.concurrencyLimiter.FilterAvailableCandidates(candidates)
		}
		selected, errPick := selector.Pick(ctx, selectorProvider, scope.model, optionsForSelectionScope(opts, selectorRouteGroup, scheduling), candidates)
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
	if opts.Alt == "alpha/search" {
		return nil, "", &Error{Code: "alpha_search_auth_unavailable", Message: "no eligible Codex Alpha Search credential for this model and scope; use Codex OAuth or an API key with alpha-search enabled and a base-url", HTTPStatus: 503}
	}
	return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
}

// withSelectionCapabilities keeps scheduling and retry planning on the same
// protocol-specific candidate set, after the tenant, channel and model gates.
func withSelectionCapabilities(opts cliproxyexecutor.Options, includeCandidate func(*Auth) bool) func(*Auth) bool {
	if opts.Alt != "alpha/search" {
		return includeCandidate
	}
	return func(candidate *Auth) bool {
		return SupportsCodexAlphaSearch(candidate) && (includeCandidate == nil || includeCandidate(candidate))
	}
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
