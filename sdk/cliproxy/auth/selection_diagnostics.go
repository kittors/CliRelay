package auth

import (
	"fmt"
	"sort"
	"strings"
)

// Selection diagnostics.
//
// When no credential can serve a request the selector used to report
// "auth_not_found: no auth available" regardless of the reason. That message is
// actively misleading in the most common case: the credentials exist and are
// healthy, but the requested model is absent from a channel group's allowed-models
// list, so every candidate is filtered out before selection runs.
//
// Diagnosing that from the outside is close to impossible — the operator sees
// "no auth available" while the panel happily lists the account. Reconstructing
// the reason here costs one extra pass over the candidate set, and only on the
// failure path, where the request is already lost.

// selectionRejection explains why a candidate was excluded.
type selectionRejection struct {
	// modelBlockedByGroups holds the channel groups whose allowed-models list
	// excluded the requested model.
	modelBlockedByGroups map[string]struct{}
	// sawTenantCandidate records that the tenant owns at least one enabled
	// credential for the request's provider, which is what makes a model-scope
	// rejection the likely explanation rather than "there are no accounts".
	sawTenantCandidate bool
	// sawServingCandidate records that some healthy tenant credential actually
	// serves the requested model. Without it a scope rejection means nothing:
	// the model may simply not be registered anywhere.
	sawServingCandidate bool
	// servingBlockedByChannels records that a serving credential was excluded by
	// the caller's allowed-channels restriction.
	servingBlockedByChannels bool
	// servingBlockedByGroups records that a serving credential was excluded by
	// the caller's allowed-channel-groups restriction.
	servingBlockedByGroups bool
}

// diagnoseEmptyCandidates inspects the credentials the scope considered and
// returns a specific error when the cause is identifiable, or nil to fall back to
// the generic message.
func (s selectorService) diagnoseEmptyCandidates(
	scope selectionScope,
	scopedRouteGroup string,
	registryRef ModelRegistry,
	includeCandidate func(candidate *Auth) bool,
) error {
	if s.manager == nil || scope.modelKey == "" {
		return nil
	}

	rejection := selectionRejection{modelBlockedByGroups: map[string]struct{}{}}
	for _, candidate := range s.manager.auths {
		if candidate == nil || candidate.Disabled || candidate.Status == StatusDisabled {
			continue
		}
		if normalizedTenantID(candidate.TenantID) != scope.tenantID {
			continue
		}
		if includeCandidate != nil && !includeCandidate(candidate) {
			continue
		}
		rejection.sawTenantCandidate = true

		// Everything below only makes sense for a credential that serves the model.
		// A credential outside the caller's channel scope that could not serve it
		// anyway is not the reason the request failed.
		if !candidateServesModel(registryRef, candidate, scope.modelKey) {
			continue
		}
		rejection.sawServingCandidate = true

		// The caller's own restrictions come first: they exclude the credential
		// before any group's model list is consulted, and they are what the
		// operator has to change.
		if !authAllowedByChannels(candidate, scope.allowedChannels) {
			rejection.servingBlockedByChannels = true
			continue
		}
		if !authAllowedByGroups(scope.cfg, candidate, scope.allowedGroups) {
			rejection.servingBlockedByGroups = true
			continue
		}

		// Re-run only the model-scope gate. A candidate that clears it was excluded
		// for some other reason, which this diagnosis deliberately does not guess at.
		groups := authGroups(scope.cfg, candidate)
		if modelAllowedByRoutingGroupScopes(scope.cfg, scope.modelKey, groups, scopedRouteGroup, scope.allowedGroups) {
			continue
		}
		for _, name := range blockingRouteGroups(scope.cfg, scope.modelKey, groups, scopedRouteGroup, scope.allowedGroups) {
			rejection.modelBlockedByGroups[name] = struct{}{}
		}
	}

	if !rejection.sawTenantCandidate {
		return nil
	}

	// A credential that serves the model exists but the caller may not reach it.
	// This is the case the generic message hid worst: the panel lists the account,
	// the model panel lists the model, and the request still says "no auth".
	if rejection.sawServingCandidate {
		if rejection.servingBlockedByGroups {
			return &Error{
				Code: "model_outside_allowed_channel_groups",
				Message: fmt.Sprintf(
					"model %q is served only by credentials outside the channel groups this credential may use (%s); add a serving account to one of those groups, or widen the allowed channel groups",
					scope.modelKey, describeScopeNames(scope.allowedGroups),
				),
			}
		}
		if rejection.servingBlockedByChannels {
			return &Error{
				Code: "model_outside_allowed_channels",
				Message: fmt.Sprintf(
					"model %q is served only by credentials outside the channels this credential may use (%s); widen the allowed channels or use an account in one of them",
					scope.modelKey, describeScopeNames(scope.allowedChannels),
				),
			}
		}
	}

	if len(rejection.modelBlockedByGroups) == 0 {
		return nil
	}

	names := make([]string, 0, len(rejection.modelBlockedByGroups))
	for name := range rejection.modelBlockedByGroups {
		names = append(names, name)
	}
	sort.Strings(names)

	return &Error{
		Code: "model_not_allowed_by_channel_group",
		Message: fmt.Sprintf(
			"model %q is not in the allowed models of channel group %s; add it there or route the request to a group that permits it",
			scope.modelKey, strings.Join(quoteAll(names), ", "),
		),
	}
}

// candidateServesModel reports whether a credential can serve a model, ignoring
// every scope gate. It mirrors the registry half of candidateSupportsModelOpts so
// the diagnosis agrees with the selector on what "serves" means.
func candidateServesModel(registryRef ModelRegistry, auth *Auth, modelID string) bool {
	if auth == nil {
		return false
	}
	if registryRef == nil {
		return true
	}
	if len(registryRef.GetModelsForClient(auth.ID)) == 0 {
		return !authHasDisableAllModelsRule(auth)
	}
	return registryRef.ClientSupportsModel(auth.ID, modelID)
}

// describeScopeNames renders a restriction set for an operator-facing message.
func describeScopeNames(scope map[string]struct{}) string {
	if len(scope) == 0 {
		return "none"
	}
	names := make([]string, 0, len(scope))
	for name := range scope {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(quoteAll(names), ", ")
}

// blockingRouteGroups lists the groups whose allowed-models list rejected a model.
func blockingRouteGroups(
	cfg *runtimeConfigSnapshot,
	modelID string,
	candidateGroups map[string]struct{},
	routeGroup string,
	allowedGroups map[string]struct{},
) []string {
	if cfg == nil || len(candidateGroups) == 0 {
		return nil
	}

	scoped := scopedRouteGroupNames(candidateGroups, routeGroup, allowedGroups)
	if len(scoped) == 0 {
		return nil
	}

	blocking := make([]string, 0, len(scoped))
	for _, group := range cfg.Routing.ChannelGroups {
		name := normalizeGroupName(group.Name)
		if _, ok := scoped[name]; !ok {
			continue
		}
		if len(group.AllowedModels) == 0 {
			continue
		}
		if !routingGroupAllowsModel(name, group.AllowedModels, modelID) {
			blocking = append(blocking, group.Name)
		}
	}
	return blocking
}

// scopedRouteGroupNames mirrors the group narrowing modelAllowedByRoutingGroupScopes
// performs, so the diagnosis reports the same groups the gate actually consulted.
func scopedRouteGroupNames(
	candidateGroups map[string]struct{},
	routeGroup string,
	allowedGroups map[string]struct{},
) map[string]struct{} {
	scoped := make(map[string]struct{})
	if routeGroup != "" {
		normalized := normalizeGroupName(routeGroup)
		if _, ok := candidateGroups[normalized]; ok {
			scoped[normalized] = struct{}{}
		}
	}
	if len(allowedGroups) > 0 {
		for group := range candidateGroups {
			if _, ok := allowedGroups[group]; ok {
				scoped[group] = struct{}{}
			}
		}
	}
	return scoped
}

func quoteAll(values []string) []string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return quoted
}
