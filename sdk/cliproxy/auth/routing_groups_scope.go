package auth

import (
	"strconv"
	"strings"
)

func channelGroupStrategy(cfg *runtimeConfigSnapshot, groupName string) string {
	if cfg == nil {
		return ""
	}
	groupName = normalizeGroupName(groupName)
	if groupName == "" {
		return ""
	}
	for i := range cfg.Routing.ChannelGroups {
		group := cfg.Routing.ChannelGroups[i]
		if normalizeGroupName(group.Name) != groupName {
			continue
		}
		if strings.TrimSpace(group.Strategy) == "" {
			return ""
		}
		return group.Strategy
	}
	return ""
}

func onlyAllowedGroupName(allowedGroups map[string]struct{}) string {
	if len(allowedGroups) != 1 {
		return ""
	}
	for group := range allowedGroups {
		return normalizeGroupName(group)
	}
	return ""
}

func scopedRoutingStrategy(cfg *runtimeConfigSnapshot, routeGroup string, allowedGroups map[string]struct{}) string {
	if routeGroup = normalizeGroupName(routeGroup); routeGroup != "" {
		if strategy := channelGroupStrategy(cfg, routeGroup); strategy != "" {
			return strategy
		}
		return globalRoutingStrategy(cfg)
	}
	if allowedGroup := onlyAllowedGroupName(allowedGroups); allowedGroup != "" {
		if strategy := channelGroupStrategy(cfg, allowedGroup); strategy != "" {
			return strategy
		}
	}
	return globalRoutingStrategy(cfg)
}

func globalRoutingStrategy(cfg *runtimeConfigSnapshot) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Routing.Strategy)
}

func includeDefaultGroup(cfg *runtimeConfigSnapshot) bool {
	if cfg == nil {
		return true
	}
	return cfg.Routing.IncludeDefaultGroup
}

func routingChannelGroupMatchesAuth(auth *Auth, group runtimeRoutingChannelGroup, authPrefix string) bool {
	if auth == nil {
		return false
	}
	if authPrefix == "" {
		authPrefix = normalizeGroupName(auth.Prefix)
	}
	for _, prefix := range group.Match.Prefixes {
		if authPrefix != "" && normalizeGroupName(prefix) == authPrefix {
			return true
		}
	}
	for _, channel := range group.Match.Channels {
		if authMatchesChannelName(auth, channel) {
			return true
		}
	}
	return authMatchesAnyTag(auth, group.Match.Tags)
}

func requestedModelHasRoutingPrefix(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 {
		return false
	}
	return normalizeGroupName(parts[0]) != "" && strings.TrimSpace(parts[1]) != ""
}

func defaultRouteGroupForSelection(cfg *runtimeConfigSnapshot, model string) string {
	if cfg == nil || !cfg.Routing.IncludeDefaultGroup || requestedModelHasRoutingPrefix(model) {
		return ""
	}
	return "default"
}

func effectiveRouteGroupForSelection(cfg *runtimeConfigSnapshot, routeGroup string, allowedGroups map[string]struct{}, model string) string {
	if routeGroup = normalizeGroupName(routeGroup); routeGroup != "" {
		return routeGroup
	}
	if len(allowedGroups) > 0 {
		return ""
	}
	return defaultRouteGroupForSelection(cfg, model)
}

func authGroups(cfg *runtimeConfigSnapshot, auth *Auth) map[string]struct{} {
	if auth == nil {
		return nil
	}
	out := make(map[string]struct{})
	authPrefix := normalizeGroupName(auth.Prefix)
	if cfg == nil {
		if authPrefix != "" {
			out[authPrefix] = struct{}{}
		} else if includeDefaultGroup(cfg) {
			out["default"] = struct{}{}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	explicitGroups := make(map[string]struct{})
	excludeFromDefault := false
	for i := range cfg.Routing.ChannelGroups {
		group := cfg.Routing.ChannelGroups[i]
		groupName := normalizeGroupName(group.Name)
		if groupName == "" {
			continue
		}
		if routingChannelGroupMatchesAuth(auth, group, authPrefix) {
			explicitGroups[groupName] = struct{}{}
			if groupName != "default" && group.ExcludeFromDefault {
				excludeFromDefault = true
			}
		}
	}
	if authPrefix != "" {
		out[authPrefix] = struct{}{}
	} else if includeDefaultGroup(cfg) && !excludeFromDefault {
		out["default"] = struct{}{}
	}
	for group := range explicitGroups {
		out[group] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func authAllowedByGroups(cfg *runtimeConfigSnapshot, auth *Auth, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		return true
	}
	for group := range authGroups(cfg, auth) {
		if _, ok := allowed[group]; ok {
			return true
		}
	}
	return false
}

func authInRouteGroup(cfg *runtimeConfigSnapshot, auth *Auth, group string) bool {
	group = normalizeGroupName(group)
	if group == "" {
		return true
	}
	_, ok := authGroups(cfg, auth)[group]
	return ok
}

func priorityScopeGroups(routeGroup string, allowedGroups map[string]struct{}) map[string]struct{} {
	routeGroup = normalizeGroupName(routeGroup)
	if routeGroup != "" {
		return map[string]struct{}{routeGroup: {}}
	}
	if len(allowedGroups) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(allowedGroups))
	for group := range allowedGroups {
		group = normalizeGroupName(group)
		if group == "" {
			continue
		}
		out[group] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func derivedGroupPriority(cfg *runtimeConfigSnapshot, auth *Auth, scopedGroups map[string]struct{}) (int, bool) {
	if cfg == nil || auth == nil {
		return 0, false
	}
	if len(scopedGroups) == 0 {
		return 0, false
	}
	groups := authGroups(cfg, auth)
	if len(groups) == 0 {
		return 0, false
	}
	best := 0
	found := false
	for i := range cfg.Routing.ChannelGroups {
		group := cfg.Routing.ChannelGroups[i]
		groupName := normalizeGroupName(group.Name)
		if _, ok := groups[groupName]; !ok {
			continue
		}
		if _, ok := scopedGroups[groupName]; !ok {
			continue
		}
		for name, priority := range group.ChannelPriorities {
			if authMatchesChannelName(auth, name) && (!found || priority > best) {
				best = priority
				found = true
			}
		}
		if group.Priority != 0 && (!found || group.Priority > best) {
			best = group.Priority
			found = true
		}
	}
	return best, found
}

func prepareCandidateForSelection(cfg *runtimeConfigSnapshot, auth *Auth, routeGroup string, allowedGroups map[string]struct{}) *Auth {
	if auth == nil {
		return nil
	}
	cloned := auth.Clone()
	if cloned == nil {
		return nil
	}
	if strings.TrimSpace(cloned.Attributes["priority"]) != "" {
		return cloned
	}
	priority, ok := derivedGroupPriority(cfg, cloned, priorityScopeGroups(routeGroup, allowedGroups))
	if !ok {
		return cloned
	}
	if cloned.Attributes == nil {
		cloned.Attributes = make(map[string]string)
	}
	cloned.Attributes["priority"] = strconv.Itoa(priority)
	return cloned
}

func candidateSupportsModel(cfg *runtimeConfigSnapshot, registryRef ModelRegistry, auth *Auth, modelID string, routeGroup string, allowedGroups map[string]struct{}) bool {
	return candidateSupportsModelOpts(cfg, registryRef, auth, modelID, routeGroup, allowedGroups, ServeModelScopeOptions{})
}

func candidateSupportsModelOpts(cfg *runtimeConfigSnapshot, registryRef ModelRegistry, auth *Auth, modelID string, routeGroup string, allowedGroups map[string]struct{}, opts ServeModelScopeOptions) bool {
	modelID = strings.TrimSpace(modelID)
	if auth == nil || modelID == "" {
		return false
	}
	groups := authGroups(cfg, auth)
	if !opts.IgnoreGroupAllowedModels && !modelAllowedByRoutingGroupScopes(cfg, modelID, groups, routeGroup, allowedGroups) {
		return false
	}
	if registryRef == nil {
		return true
	}
	if len(registryRef.GetModelsForClient(auth.ID)) == 0 {
		return !authHasDisableAllModelsRule(auth)
	}
	if registryRef.ClientSupportsModel(auth.ID, modelID) {
		return true
	}
	if len(groups) == 0 {
		return false
	}
	tryGroups := make(map[string]struct{})
	if routeGroup != "" {
		if _, ok := groups[routeGroup]; ok {
			tryGroups[routeGroup] = struct{}{}
		}
	}
	if len(allowedGroups) > 0 {
		for group := range groups {
			if _, ok := allowedGroups[group]; ok {
				tryGroups[group] = struct{}{}
			}
		}
	}
	if len(tryGroups) == 0 {
		for group := range groups {
			tryGroups[group] = struct{}{}
		}
	}
	for group := range tryGroups {
		if group == "default" {
			continue
		}
		if registryRef.ClientSupportsModel(auth.ID, group+"/"+modelID) {
			return true
		}
	}
	return false
}

func authHasDisableAllModelsRule(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	for _, key := range []string{"excluded_models", "excluded-models"} {
		for _, raw := range strings.Split(auth.Attributes[key], ",") {
			if strings.TrimSpace(raw) == "*" {
				return true
			}
		}
	}
	return false
}

func modelAllowedByRoutingGroupScopes(cfg *runtimeConfigSnapshot, modelID string, candidateGroups map[string]struct{}, routeGroup string, allowedGroups map[string]struct{}) bool {
	modelID = strings.TrimSpace(modelID)
	if cfg == nil || modelID == "" || len(candidateGroups) == 0 {
		return true
	}

	scopedGroups := make(map[string]struct{})
	if routeGroup != "" {
		normalized := normalizeGroupName(routeGroup)
		if _, ok := candidateGroups[normalized]; ok {
			scopedGroups[normalized] = struct{}{}
		}
	}
	if len(allowedGroups) > 0 {
		for group := range candidateGroups {
			if _, ok := allowedGroups[group]; ok {
				scopedGroups[group] = struct{}{}
			}
		}
	}
	if len(scopedGroups) == 0 {
		return true
	}

	foundRestrictedGroup := false
	for _, group := range cfg.Routing.ChannelGroups {
		groupName := normalizeGroupName(group.Name)
		if _, ok := scopedGroups[groupName]; !ok {
			continue
		}
		if len(group.AllowedModels) == 0 {
			return true
		}
		foundRestrictedGroup = true
		if routingGroupAllowsModel(groupName, group.AllowedModels, modelID) {
			return true
		}
	}
	return !foundRestrictedGroup
}

func routingGroupAllowsModel(groupName string, allowedModels []string, modelID string) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	unprefixed := modelID
	if groupName != "" {
		prefix := groupName + "/"
		if strings.HasPrefix(strings.ToLower(modelID), strings.ToLower(prefix)) {
			unprefixed = modelID[len(prefix):]
		}
	}
	for _, allowed := range allowedModels {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}
		if strings.EqualFold(allowed, modelID) || strings.EqualFold(allowed, unprefixed) {
			return true
		}
	}
	return false
}
