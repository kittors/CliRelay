package routing

import "strings"

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
