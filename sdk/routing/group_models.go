package routing

import (
	"fmt"
	"sort"
	"strings"
)

// discoveredModel is installed by model discovery. Nil until that package
// initialises, which is the same as "nothing has been listed".
var discoveredModel func(tenantID, modelID string) bool

// SetDiscoveredModel registers the lookup the channel-group gate uses to admit
// a model the upstream listed after an allow list was saved. Nil clears it.
func SetDiscoveredModel(fn func(tenantID, modelID string) bool) {
	discoveredModel = fn
}

// DiscoveredModel reports whether tenantID's stored upstream lists contain modelID.
func DiscoveredModel(tenantID, modelID string) bool {
	fn := discoveredModel
	if fn == nil {
		return false
	}
	return fn(tenantID, modelID)
}

// FollowsUpstreamModels resolves a channel group's follow-upstream-models
// setting: whether its allow list also admits a model discovery lists after the
// list was saved. Every gate that reads an allow list (the request gate, the
// credential selector, /v1/models and the panel's availability) asks this, so
// they cannot disagree on which groups grow on their own.
//
// A group that never set it follows only when it is the default group. That is
// the root pool behind /v1 and the model plaza, which followed upstream before
// the setting existed; a curated group stays frozen until an operator opts it
// in, because inheriting every new model is not what an allow list promised.
func FollowsUpstreamModels(groupName string, setting *bool) bool {
	if setting != nil {
		return *setting
	}
	return NormalizeGroupName(groupName) == "default"
}

// GroupAdmitsDiscoveredModel reports whether a group that follows upstream
// admits modelID because discovery listed it for tenantID. It does not read the
// group's exclusions; callers check those first, since an exclusion always wins.
func GroupAdmitsDiscoveredModel(follows bool, tenantID, modelID string) bool {
	return follows && strings.TrimSpace(modelID) != "" && DiscoveredModel(tenantID, modelID)
}

// ChannelGroupModelDenialMessage explains why channel groups refused a model,
// naming each group by the list that did it: excludedBy holds groups whose
// excluded-models cover the model, notAllowedBy groups whose allow list does not
// name it. The request gate and the credential selector both report a refusal
// with it, so a client sees the same wording whichever one stopped the request.
//
// upstreamListed adds the cheaper fix when discovery already lists the model:
// every group that refused it is then a frozen one, and following upstream
// admits this model and the next one, where adding the id fixes only this one.
func ChannelGroupModelDenialMessage(model string, excludedBy, notAllowedBy []string, upstreamListed bool) string {
	excluded, notAllowed := quotedGroupNames(excludedBy), quotedGroupNames(notAllowedBy)
	switch {
	case excluded == "" && notAllowed == "":
		return fmt.Sprintf("model %q is not served by the channel groups this request may use", model)
	case excluded == "":
		message := fmt.Sprintf(
			"model %q is not in the allowed models of channel group %s; add it there or route the request to a group that permits it",
			model, notAllowed,
		)
		if upstreamListed {
			message += "; the upstream already lists this model, so turning on the group's follow-upstream-models setting admits it and the models the upstream adds later"
		}
		return message
	case notAllowed == "":
		return fmt.Sprintf(
			"model %q is excluded by channel group %s; remove it from the group's excluded models or route the request to a group that permits it",
			model, excluded,
		)
	default:
		return fmt.Sprintf(
			"model %q is excluded by channel group %s and not in the allowed models of channel group %s; change the group's model list or route the request to a group that permits it",
			model, excluded, notAllowed,
		)
	}
}

// quotedGroupNames renders a set of group names as a sorted, quoted list.
func quotedGroupNames(names []string) string {
	seen := make(map[string]struct{}, len(names))
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		quoted = append(quoted, name)
	}
	sort.Strings(quoted)
	for i, name := range quoted {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

// ChannelGroupExcludesModel reports whether a channel group's excluded-models
// list covers the requested model.
//
// Either side may name the model with or without a route prefix: the panel
// lists a prefixed credential's models as "xai/grok-4.7", while a client routed
// by group path asks for "grok-4.7", and the reverse happens too. Each side is
// compared as written and with its first path segment removed, so an exclusion
// cannot be sidestepped by spelling the model the other way. Entries accept the
// same '*' wildcard as provider-level excluded-models: "*" alone blocks the
// whole group, "grok-imagine-*" a family.
//
// The match is deliberately loose, because a miss here serves a model the
// operator excluded. The price is that a vendor-namespaced id such as
// "anthropic/claude-x" also counts as "claude-x", and an entry like "xai/*"
// covers every model: a request for a bare id cannot be tied to one prefix.
// Blocking one model too many shows up at once and is undone in the panel;
// serving an excluded one does not show up at all.
//
// Allow lists keep their exact matching and do not use this: there a miss
// refuses a model rather than serving it.
func ChannelGroupExcludesModel(excluded []string, model string) bool {
	requested := modelSpellings(model)
	if len(requested) == 0 {
		return false
	}
	for _, entry := range excluded {
		for _, pattern := range modelSpellings(entry) {
			for _, candidate := range requested {
				if MatchWildcard(pattern, candidate) {
					return true
				}
			}
		}
	}
	return false
}

// modelSpellings lower-cases a model id and returns it as written and, when it
// carries one, without its first path segment.
func modelSpellings(id string) []string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return nil
	}
	if idx := strings.Index(id, "/"); idx > 0 && idx < len(id)-1 {
		return []string{id, id[idx+1:]}
	}
	return []string{id}
}

// MatchWildcard reports whether value matches pattern, where '*' matches any
// substring, including an empty one. The comparison is case-sensitive, so
// callers lower-case both sides first. Provider-level and channel-group
// excluded-models share this rule.
func MatchWildcard(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	return true
}
