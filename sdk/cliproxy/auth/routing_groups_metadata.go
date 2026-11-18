package auth

import (
	"fmt"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func normalizeRoutingTag(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" {
		return ""
	}
	return strings.Join(strings.Fields(trimmed), "-")
}

func normalizeGroupName(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	trimmed = strings.Trim(trimmed, "/")
	return trimmed
}

func normalizeRouteFallback(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none":
		return "none"
	case "default":
		return "default"
	default:
		return "none"
	}
}

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

func metadataTagList(meta map[string]any, key string) ([]string, bool) {
	if len(meta) == 0 {
		return nil, false
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return nil, ok
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
		return nil, ok
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		tag := normalizeRoutingTag(value)
		if tag == "" {
			continue
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out, ok
}

func metadataStringValue(meta map[string]any, keys ...string) string {
	if len(meta) == 0 {
		return ""
	}
	for _, key := range keys {
		if value, ok := meta[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func appendUniqueTag(values []string, tag string) []string {
	normalized := normalizeRoutingTag(tag)
	if normalized == "" {
		return values
	}
	for _, existing := range values {
		if existing == normalized {
			return values
		}
	}
	return append(values, normalized)
}

func allowedChannelGroupsFromMetadata(meta map[string]any) map[string]struct{} {
	return metadataStringSet(meta, "allowed-channel-groups", normalizeGroupName)
}

func routeGroupFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	switch raw := meta[cliproxyexecutor.RouteGroupMetadataKey].(type) {
	case string:
		return normalizeGroupName(raw)
	case []byte:
		return normalizeGroupName(string(raw))
	default:
		return ""
	}
}

// tenantWideSelectionFromMetadata reports whether the request may select from
// every credential of its tenant; see cliproxyexecutor.TenantWideSelectionMetadataKey.
func tenantWideSelectionFromMetadata(meta map[string]any) bool {
	raw, ok := meta[cliproxyexecutor.TenantWideSelectionMetadataKey]
	if !ok || raw == nil {
		return false
	}
	enabled, ok := parseBoolAny(raw)
	return ok && enabled
}

func routeFallbackFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return "none"
	}
	switch raw := meta[cliproxyexecutor.RouteFallbackMetadataKey].(type) {
	case string:
		return normalizeRouteFallback(raw)
	case []byte:
		return normalizeRouteFallback(string(raw))
	default:
		return "none"
	}
}
