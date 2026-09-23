package modelcatalog

import (
	"strings"

	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
)

// filterModelsByRoutingAllowedModels drops models the effective channel group's
// model gate rejects. Used after live-discovery merge: discovery models are not
// registry-backed, so CanServe cannot enforce the gate for them.
// filterModelsByRoutingAllowedModelsOpts skips the filter entirely when the caller
// is the channel-group editor, which must list models it is meant to add.
func (s *Service) filterModelsByRoutingAllowedModelsOpts(opts AvailabilityFilterOptions, models []map[string]any, allowedGroupsRaw string) []map[string]any {
	if opts.IgnoreGroupAllowedModels {
		return models
	}
	return s.filterModelsByRoutingAllowedModels(models, allowedGroupsRaw)
}

func (s *Service) filterModelsByRoutingAllowedModels(models []map[string]any, allowedGroupsRaw string) []map[string]any {
	gate := s.resolveRoutingModelGate(allowedGroupsRaw)
	if gate.unrestricted {
		return models
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if gate.allows(id) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// routingModelGate mirrors the runtime gate in sdk/cliproxy/auth so plaza and the
// catalog list exactly the models a request would be allowed to reach.
type routingModelGate struct {
	// unrestricted is set when some scoped group serves every model its channels
	// offer, which makes the union of scoped groups unrestricted too.
	unrestricted bool
	groups       []routingModelGroupGate
}

type routingModelGroupGate struct {
	allowed  []string
	excluded []string
}

// allows reports whether any scoped group would serve the model. The scoped
// groups form a union, so one permissive group is enough.
func (g routingModelGate) allows(model string) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	if g.unrestricted {
		return true
	}
	for _, group := range g.groups {
		if len(group.excluded) > 0 && routingAllowedModelMatches(model, group.excluded) {
			continue
		}
		if len(group.allowed) == 0 || routingAllowedModelMatches(model, group.allowed) {
			return true
		}
	}
	return false
}

// resolveRoutingModelGate collects the model gates of the scoped groups. A group
// with an empty allow list and no exclusions, or a scope that matches no group at
// all, leaves the gate unrestricted.
func (s *Service) resolveRoutingModelGate(allowedGroupsRaw string) routingModelGate {
	unrestricted := routingModelGate{unrestricted: true}
	if s == nil {
		return unrestricted
	}
	routing := tenantRoutingConfig(s.tenantID, s.cfg)
	if routing == nil {
		return unrestricted
	}
	scopedGroups := internalrouting.ParseNormalizedSet(strings.TrimSpace(allowedGroupsRaw), internalrouting.NormalizeGroupName)
	if len(scopedGroups) == 0 {
		if routing.IncludeDefaultGroup {
			scopedGroups = map[string]struct{}{"default": {}}
		}
	}
	if len(scopedGroups) == 0 {
		return unrestricted
	}
	var gate routingModelGate
	for _, group := range routing.ChannelGroups {
		groupName := internalrouting.NormalizeGroupName(group.Name)
		if _, ok := scopedGroups[groupName]; !ok {
			continue
		}
		// Both lists empty means the group serves every model its channels offer,
		// including ones the upstream adds later.
		if len(group.AllowedModels) == 0 && len(group.ExcludedModels) == 0 {
			return unrestricted
		}
		gate.groups = append(gate.groups, routingModelGroupGate{
			allowed:  group.AllowedModels,
			excluded: group.ExcludedModels,
		})
	}
	if len(gate.groups) == 0 {
		return unrestricted
	}
	return gate
}

func routingAllowedModelMatches(model string, patterns []string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if pattern == "*" {
			return true
		}
		if strings.EqualFold(model, pattern) {
			return true
		}
		if idx := strings.Index(model, "/"); idx >= 0 && strings.EqualFold(strings.TrimSpace(model[idx+1:]), pattern) {
			return true
		}
	}
	return false
}
