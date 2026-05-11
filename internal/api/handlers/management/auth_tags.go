package management

import (
	"fmt"
	"sort"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const maxCustomAuthTags = 3

var codexPlanDisplayTags = map[string]struct{}{
	"business":   {},
	"edu":        {},
	"enterprise": {},
	"free":       {},
	"plus":       {},
	"pro":        {},
	"team":       {},
}

type authTagPayload struct {
	DefaultTags       []string
	CustomTags        []string
	HiddenDefaultTags []string
	DisplayTags       []string
}

func buildAuthTagPayload(auth *coreauth.Auth) authTagPayload {
	if auth == nil {
		return authTagPayload{
			DefaultTags:       []string{},
			CustomTags:        []string{},
			HiddenDefaultTags: []string{},
			DisplayTags:       []string{},
		}
	}
	return buildAuthTagPayloadFromValues(strings.TrimSpace(auth.Provider), auth.Metadata)
}

func buildAuthTagPayloadFromValues(provider string, metadata map[string]any) authTagPayload {
	defaultTags := defaultAuthTags(provider, metadata)
	customTags := metadataStringSlice(metadata, "custom_tags")
	hiddenDefaultTags := metadataStringSlice(metadata, "hidden_default_tags")
	explicitDisplayTags, hasExplicitDisplayTags := metadataStringSliceWithPresence(
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
			if containsNormalizedTag(displayTags, tag) {
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

	return authTagPayload{
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
		if normalized := normalizeTagValue(tag); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	for _, tag := range customTags {
		if normalized := normalizeTagValue(tag); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}

	providerTag := normalizeTagValue(provider)
	currentPlan := normalizeTagValue(metadataString(metadata, "plan_type", "planType"))
	out := make([]string, 0, len(explicitDisplayTags))
	for _, tag := range explicitDisplayTags {
		normalized := normalizeTagValue(tag)
		if normalized == "" {
			continue
		}
		if _, ok := allowed[normalized]; ok {
			if !containsNormalizedTag(out, normalized) {
				out = append(out, normalized)
			}
			continue
		}
		if isStaleCodexPlanDisplayTag(providerTag, currentPlan, normalized) {
			if _, ok := allowed[currentPlan]; ok && !containsNormalizedTag(out, currentPlan) {
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
	if normalizedProvider := normalizeTagValue(provider); normalizedProvider != "" && normalizedProvider != "unknown" {
		tags = append(tags, normalizedProvider)
	}
	if metadata != nil {
		if planType := normalizeTagValue(metadataString(metadata, "plan_type", "planType")); planType != "" {
			if !containsNormalizedTag(tags, planType) {
				tags = append(tags, planType)
			}
		}
	}
	return tags
}

func normalizeEditableTags(values []string, max int) ([]string, error) {
	normalized := normalizeTagList(values)
	if max > 0 && len(normalized) > max {
		return nil, fmt.Errorf("custom_tags supports at most %d items", max)
	}
	return normalized, nil
}

func normalizeTagList(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		tag := normalizeTagValue(value)
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

func normalizeTagValue(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" {
		return ""
	}
	return strings.Join(strings.Fields(trimmed), "-")
}

func containsNormalizedTag(values []string, target string) bool {
	normalizedTarget := normalizeTagValue(target)
	for _, value := range values {
		if normalizeTagValue(value) == normalizedTarget {
			return true
		}
	}
	return false
}

func metadataStringSlice(metadata map[string]any, key string) []string {
	values, _ := metadataStringSliceWithPresence(metadata, key)
	return values
}

func metadataStringSliceWithPresence(metadata map[string]any, key string) ([]string, bool) {
	if len(metadata) == 0 {
		return []string{}, false
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return []string{}, ok
	}
	switch typed := raw.(type) {
	case []string:
		return normalizeTagList(typed), true
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				values = append(values, text)
			}
		}
		return normalizeTagList(values), true
	case string:
		if strings.TrimSpace(typed) == "" {
			return []string{}, true
		}
		return normalizeTagList(strings.Split(typed, ",")), true
	default:
		return []string{}, true
	}
}

func metadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if raw, ok := metadata[key].(string); ok {
			if trimmed := strings.TrimSpace(raw); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

type channelGroupChannelDetail struct {
	Name              string   `json:"name"`
	Source            string   `json:"source,omitempty"`
	Disabled          bool     `json:"disabled,omitempty"`
	DefaultTags       []string `json:"default_tags"`
	CustomTags        []string `json:"custom_tags"`
	HiddenDefaultTags []string `json:"hidden_default_tags"`
	DisplayTags       []string `json:"display_tags"`
}

func uniqueSortedChannelDetails(values []channelGroupChannelDetail) []channelGroupChannelDetail {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]channelGroupChannelDetail, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value.Name)
		if name == "" {
			continue
		}
		value.Name = name
		if value.DefaultTags == nil {
			value.DefaultTags = []string{}
		}
		if value.CustomTags == nil {
			value.CustomTags = []string{}
		}
		if value.HiddenDefaultTags == nil {
			value.HiddenDefaultTags = []string{}
		}
		if value.DisplayTags == nil {
			value.DisplayTags = []string{}
		}
		key := strings.ToLower(name)
		existing, ok := seen[key]
		if !ok || tagPayloadScore(value) >= tagPayloadScore(existing) {
			seen[key] = value
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]channelGroupChannelDetail, 0, len(seen))
	for _, value := range seen {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func tagPayloadScore(value channelGroupChannelDetail) int {
	score := len(value.DisplayTags)*100 + len(value.DefaultTags)*10 + len(value.CustomTags)
	if value.Disabled {
		score -= 10000
	}
	return score
}
