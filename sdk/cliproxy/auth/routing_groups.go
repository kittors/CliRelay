package auth

import (
	"fmt"
	"strconv"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func metadataStringSet(meta map[string]any, key string, normalizer func(string) string) map[string]struct{} {
	if len(meta) == 0 {
		return nil
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return nil
	}
	var values []string
	switch typed := raw.(type) {
	case string:
		values = strings.Split(typed, ",")
	case []string:
		values = typed
	case []any:
		values = make([]string, 0, len(typed))
		for _, item := range typed {
			values = append(values, fmt.Sprint(item))
		}
	default:
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		if normalizer != nil {
			normalized = normalizer(normalized)
		}
		if normalized == "" {
			continue
		}
		out[normalized] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func allowedChannelGroupsFromMetadata(meta map[string]any) map[string]struct{} {
	return metadataStringSet(meta, "allowed-channel-groups", internalrouting.NormalizeGroupName)
}

func routeGroupFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	switch raw := meta[cliproxyexecutor.RouteGroupMetadataKey].(type) {
	case string:
		return internalrouting.NormalizeGroupName(raw)
	case []byte:
		return internalrouting.NormalizeGroupName(string(raw))
	default:
		return ""
	}
}

func routeFallbackFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return "none"
	}
	switch raw := meta[cliproxyexecutor.RouteFallbackMetadataKey].(type) {
	case string:
		return internalrouting.NormalizeFallback(raw)
	case []byte:
		return internalrouting.NormalizeFallback(string(raw))
	default:
		return "none"
	}
}

func includeDefaultGroup(cfg *internalconfig.Config) bool {
	if cfg == nil {
		return true
	}
	return cfg.Routing.IncludeDefaultGroup
}

func authGroups(cfg *internalconfig.Config, auth *Auth) map[string]struct{} {
	if auth == nil {
		return nil
	}
	out := make(map[string]struct{})
	if prefix := internalrouting.NormalizeGroupName(auth.Prefix); prefix != "" {
		out[prefix] = struct{}{}
	} else if includeDefaultGroup(cfg) {
		out["default"] = struct{}{}
	}
	if cfg == nil {
		if len(out) == 0 {
			return nil
		}
		return out
	}
	authPrefix := internalrouting.NormalizeGroupName(auth.Prefix)
	for i := range cfg.Routing.ChannelGroups {
		group := cfg.Routing.ChannelGroups[i]
		matched := false
		for _, prefix := range group.Match.Prefixes {
			if authPrefix != "" && internalrouting.NormalizeGroupName(prefix) == authPrefix {
				matched = true
				break
			}
		}
		if !matched {
			for _, channel := range group.Match.Channels {
				if authMatchesChannelName(auth, channel) {
					matched = true
					break
				}
			}
		}
		if matched {
			out[group.Name] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func authAllowedByGroups(cfg *internalconfig.Config, auth *Auth, allowed map[string]struct{}) bool {
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

func authInRouteGroup(cfg *internalconfig.Config, auth *Auth, group string) bool {
	group = internalrouting.NormalizeGroupName(group)
	if group == "" {
		return true
	}
	_, ok := authGroups(cfg, auth)[group]
	return ok
}

func priorityScopeGroups(routeGroup string, allowedGroups map[string]struct{}) map[string]struct{} {
	routeGroup = internalrouting.NormalizeGroupName(routeGroup)
	if routeGroup != "" {
		return map[string]struct{}{routeGroup: {}}
	}
	if len(allowedGroups) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(allowedGroups))
	for group := range allowedGroups {
		group = internalrouting.NormalizeGroupName(group)
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

func derivedGroupPriority(cfg *internalconfig.Config, auth *Auth, scopedGroups map[string]struct{}) (int, bool) {
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
		if _, ok := groups[group.Name]; !ok {
			continue
		}
		if _, ok := scopedGroups[group.Name]; !ok {
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

func prepareCandidateForSelection(cfg *internalconfig.Config, auth *Auth, routeGroup string, allowedGroups map[string]struct{}) *Auth {
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

func candidateSupportsModel(cfg *internalconfig.Config, registryRef *registry.ModelRegistry, auth *Auth, modelID string, routeGroup string, allowedGroups map[string]struct{}) bool {
	modelID = strings.TrimSpace(modelID)
	if auth == nil || modelID == "" {
		return false
	}
	groups := authGroups(cfg, auth)
	if !modelAllowedByRoutingGroupScopes(cfg, modelID, groups, routeGroup, allowedGroups) {
		return false
	}
	if registryRef == nil {
		return true
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

func modelAllowedByRoutingGroupScopes(cfg *internalconfig.Config, modelID string, candidateGroups map[string]struct{}, routeGroup string, allowedGroups map[string]struct{}) bool {
	modelID = strings.TrimSpace(modelID)
	if cfg == nil || modelID == "" || len(candidateGroups) == 0 {
		return true
	}

	scopedGroups := make(map[string]struct{})
	if routeGroup != "" {
		normalized := internalrouting.NormalizeGroupName(routeGroup)
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
		groupName := internalrouting.NormalizeGroupName(group.Name)
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

// KnownChannelGroups returns the currently known explicit and implicit channel groups.
func (m *Manager) KnownChannelGroups() map[string]struct{} {
	out := make(map[string]struct{})
	if m == nil {
		return out
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg != nil {
		for i := range cfg.Routing.ChannelGroups {
			if name := internalrouting.NormalizeGroupName(cfg.Routing.ChannelGroups[i].Name); name != "" {
				out[name] = struct{}{}
			}
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		for group := range authGroups(cfg, auth) {
			out[group] = struct{}{}
		}
	}
	return out
}

// CanServeModelWithScopes reports whether at least one active auth can serve the model under the given restrictions.
func (m *Manager) CanServeModelWithScopes(modelID string, allowedChannels, allowedGroups map[string]struct{}, routeGroup string) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" || m == nil {
		return false
	}
	registryRef := registry.GetGlobalRegistry()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if !authAllowedByChannels(candidate, allowedChannels) {
			continue
		}
		if !authAllowedByGroups(cfg, candidate, allowedGroups) {
			continue
		}
		if !authInRouteGroup(cfg, candidate, routeGroup) {
			continue
		}
		if candidateSupportsModel(cfg, registryRef, candidate, modelID, routeGroup, allowedGroups) {
			return true
		}
	}
	return false
}
