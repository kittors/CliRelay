package authfiles

import (
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const MaxCustomTags = 3

var codexPlanDisplayTags = map[string]struct{}{
	"business":   {},
	"edu":        {},
	"enterprise": {},
	"free":       {},
	"plus":       {},
	"pro":        {},
	"team":       {},
}

type TagPayload struct {
	DefaultTags       []string
	CustomTags        []string
	HiddenDefaultTags []string
	DisplayTags       []string
}

func BuildTagPayload(auth *coreauth.Auth) TagPayload {
	if auth == nil {
		return TagPayload{
			DefaultTags:       []string{},
			CustomTags:        []string{},
			HiddenDefaultTags: []string{},
			DisplayTags:       []string{},
		}
	}
	return BuildTagPayloadFromValues(strings.TrimSpace(auth.Provider), auth.Metadata)
}

func BuildTagPayloadFromValues(provider string, metadata map[string]any) TagPayload {
	defaultTags := defaultAuthTags(provider, metadata)
	customTags := MetadataStringSlice(metadata, "custom_tags")
	hiddenDefaultTags := MetadataStringSlice(metadata, "hidden_default_tags")
	explicitDisplayTags, hasExplicitDisplayTags := MetadataStringSliceWithPresence(
		metadata,
		"display_tags",
	)
	hiddenSet := make(map[string]struct{}, len(hiddenDefaultTags))
	for _, tag := range hiddenDefaultTags {
		hiddenSet[tag] = struct{}{}
	}

	var displayTags []string
	if !hasExplicitDisplayTags {
		displayTags = make([]string, 0, len(defaultTags)+len(customTags))
		for _, tag := range defaultTags {
			if _, hidden := hiddenSet[tag]; hidden {
				continue
			}
			displayTags = append(displayTags, tag)
		}
		for _, tag := range customTags {
			if _, exists := hiddenSet[tag]; exists {
				continue
			}
			if ContainsNormalizedTag(displayTags, tag) {
				continue
			}
			displayTags = append(displayTags, tag)
		}
	} else {
		displayTags = reconcileExplicitDisplayTags(
			provider,
			metadata,
			defaultTags,
			customTags,
			explicitDisplayTags,
		)
	}

	return TagPayload{
		DefaultTags:       append([]string{}, defaultTags...),
		CustomTags:        append([]string{}, customTags...),
		HiddenDefaultTags: append([]string{}, hiddenDefaultTags...),
		DisplayTags:       append([]string{}, displayTags...),
	}
}

func reconcileExplicitDisplayTags(provider string, metadata map[string]any, defaultTags []string, customTags []string, explicitDisplayTags []string) []string {
	if len(explicitDisplayTags) == 0 {
		return []string{}
	}
	allowed := make(map[string]struct{}, len(defaultTags)+len(customTags))
	for _, tag := range defaultTags {
		if normalized := NormalizeTagValue(tag); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	for _, tag := range customTags {
		if normalized := NormalizeTagValue(tag); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}

	providerTag := NormalizeTagValue(provider)
	currentPlan := NormalizeTagValue(MetadataString(metadata, "plan_type", "planType"))
	out := make([]string, 0, len(explicitDisplayTags))
	for _, tag := range explicitDisplayTags {
		normalized := NormalizeTagValue(tag)
		if normalized == "" {
			continue
		}
		if _, ok := allowed[normalized]; ok {
			if !ContainsNormalizedTag(out, normalized) {
				out = append(out, normalized)
			}
			continue
		}
		if isStaleCodexPlanDisplayTag(providerTag, currentPlan, normalized) {
			if _, ok := allowed[currentPlan]; ok && !ContainsNormalizedTag(out, currentPlan) {
				out = append(out, currentPlan)
			}
		}
	}
	return out
}

func isStaleCodexPlanDisplayTag(providerTag string, currentPlan string, tag string) bool {
	if providerTag != "codex" || currentPlan == "" || tag == currentPlan {
		return false
	}
	if _, ok := codexPlanDisplayTags[currentPlan]; !ok {
		return false
	}
	_, ok := codexPlanDisplayTags[tag]
	return ok
}

func defaultAuthTags(provider string, metadata map[string]any) []string {
	tags := make([]string, 0, 2)
	if normalizedProvider := NormalizeTagValue(provider); normalizedProvider != "" && normalizedProvider != "unknown" {
		tags = append(tags, normalizedProvider)
	}
	if metadata != nil {
		if planType := NormalizeTagValue(MetadataString(metadata, "plan_type", "planType")); planType != "" {
			if !ContainsNormalizedTag(tags, planType) {
				tags = append(tags, planType)
			}
		}
	}
	return tags
}

func NormalizeEditableTags(values []string, max int) ([]string, error) {
	normalized := NormalizeTagList(values)
	if max > 0 && len(normalized) > max {
		return nil, fmt.Errorf("custom_tags supports at most %d items", max)
	}
	return normalized, nil
}

func NormalizeTagList(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		tag := NormalizeTagValue(value)
		if tag == "" {
			continue
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func NormalizeTagValue(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" {
		return ""
	}
	return strings.Join(strings.Fields(trimmed), "-")
}

func ContainsNormalizedTag(values []string, target string) bool {
	normalizedTarget := NormalizeTagValue(target)
	for _, value := range values {
		if NormalizeTagValue(value) == normalizedTarget {
			return true
		}
	}
	return false
}

func MetadataStringSlice(metadata map[string]any, key string) []string {
	values, _ := MetadataStringSliceWithPresence(metadata, key)
	return values
}

func MetadataStringSliceWithPresence(metadata map[string]any, key string) ([]string, bool) {
	if len(metadata) == 0 {
		return []string{}, false
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return []string{}, ok
	}
	switch typed := raw.(type) {
	case []string:
		return NormalizeTagList(typed), true
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				values = append(values, text)
			}
		}
		return NormalizeTagList(values), true
	case string:
		if strings.TrimSpace(typed) == "" {
			return []string{}, true
		}
		return NormalizeTagList(strings.Split(typed, ",")), true
	default:
		return []string{}, true
	}
}
