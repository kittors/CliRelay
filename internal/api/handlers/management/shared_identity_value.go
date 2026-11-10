package management

import "strings"

// maskSharedIdentityValue rewrites every string of a shared account's public
// entry, however deep: a restriction's status message and quota reason quote
// the account as often as the top-level status message does, and tags are
// lists of strings. Containers are copied rather than changed in place, since
// an entry may hold a slice or map that is still the owner's.
func maskSharedIdentityValue(value any, replacer *strings.Replacer) any {
	switch value := value.(type) {
	case string:
		return replacer.Replace(value)
	case map[string]any:
		if value == nil {
			return value
		}
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = maskSharedIdentityValue(item, replacer)
		}
		return out
	case []map[string]any:
		if value == nil {
			return value
		}
		out := make([]map[string]any, len(value))
		for i, item := range value {
			masked, _ := maskSharedIdentityValue(item, replacer).(map[string]any)
			out[i] = masked
		}
		return out
	case []any:
		if value == nil {
			return value
		}
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = maskSharedIdentityValue(item, replacer)
		}
		return out
	case []string:
		if value == nil {
			return value
		}
		out := make([]string, len(value))
		for i, item := range value {
			out[i] = replacer.Replace(item)
		}
		return out
	default:
		return value
	}
}
