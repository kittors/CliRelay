package management

import "strings"

// maskSharedIdentityValue rewrites strings in the JSON-shaped public entry,
// including restrictions' messages/reasons and tag arrays. Copy containers:
// some entries refer to the owner's metadata and must not be changed in place.
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
			out[i] = maskSharedIdentityValue(item, replacer).(map[string]any)
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
