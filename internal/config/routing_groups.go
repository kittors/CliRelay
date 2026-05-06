package config

import (
	"strings"

	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
)

// ChannelGroupMatch defines how auth channels are assigned to a routing group.
type ChannelGroupMatch struct {
	Prefixes []string `yaml:"prefixes,omitempty" json:"prefixes,omitempty"`
	Channels []string `yaml:"channels,omitempty" json:"channels,omitempty"`
}

// RoutingChannelGroup defines a named channel group used by routing and API-key permissions.
type RoutingChannelGroup struct {
	Name              string            `yaml:"name" json:"name"`
	Description       string            `yaml:"description,omitempty" json:"description,omitempty"`
	Strategy          string            `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	Match             ChannelGroupMatch `yaml:"match,omitempty" json:"match,omitempty"`
	Priority          int               `yaml:"priority,omitempty" json:"priority,omitempty"`
	ChannelPriorities map[string]int    `yaml:"channel-priorities,omitempty" json:"channel-priorities,omitempty"`
	AllowedModels     []string          `yaml:"allowed-models,omitempty" json:"allowed-models,omitempty"`
}

// RoutingPathRoute maps a URL namespace path to a channel group.
type RoutingPathRoute struct {
	Path        string `yaml:"path" json:"path"`
	Group       string `yaml:"group" json:"group"`
	StripPrefix bool   `yaml:"strip-prefix,omitempty" json:"strip-prefix,omitempty"`
	Fallback    string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

func normalizeStringList(values []string, normalizer func(string) string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		if normalizer != nil {
			normalized = normalizer(normalized)
		}
		if normalized == "" {
			continue
		}
		key := strings.ToLower(normalized)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, normalized)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeChannelPriorities(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]int, len(values))
	for key, priority := range values {
		name := strings.TrimSpace(key)
		if name == "" || priority < 0 {
			continue
		}
		existing, exists := out[name]
		if !exists || priority > existing {
			out[name] = priority
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func NormalizeRoutingStrategy(strategy string) string {
	switch strings.TrimSpace(strings.ToLower(strategy)) {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	default:
		return "round-robin"
	}
}

func normalizeOptionalRoutingStrategy(strategy string) string {
	if strings.TrimSpace(strategy) == "" {
		return ""
	}
	return NormalizeRoutingStrategy(strategy)
}

// SanitizeRouting normalizes routing configuration, deduplicating groups and paths.
func (cfg *Config) SanitizeRouting() {
	if cfg == nil {
		return
	}
	cfg.Routing.Strategy = NormalizeRoutingStrategy(cfg.Routing.Strategy)

	seenGroups := make(map[string]struct{}, len(cfg.Routing.ChannelGroups))
	groups := make([]RoutingChannelGroup, 0, len(cfg.Routing.ChannelGroups))
	for i := range cfg.Routing.ChannelGroups {
		group := cfg.Routing.ChannelGroups[i]
		group.Name = internalrouting.NormalizeGroupName(group.Name)
		group.Description = strings.TrimSpace(group.Description)
		group.Strategy = normalizeOptionalRoutingStrategy(group.Strategy)
		group.Match.Prefixes = normalizeStringList(group.Match.Prefixes, internalrouting.NormalizeGroupName)
		group.Match.Channels = normalizeStringList(group.Match.Channels, func(value string) string {
			return strings.TrimSpace(value)
		})
		group.ChannelPriorities = normalizeChannelPriorities(group.ChannelPriorities)
		group.AllowedModels = normalizeStringList(group.AllowedModels, func(value string) string {
			return strings.TrimSpace(value)
		})
		if group.Name == "" {
			continue
		}
		if _, exists := seenGroups[group.Name]; exists {
			continue
		}
		seenGroups[group.Name] = struct{}{}
		groups = append(groups, group)
	}
	cfg.Routing.ChannelGroups = groups

	seenPaths := make(map[string]struct{}, len(cfg.Routing.PathRoutes))
	pathRoutes := make([]RoutingPathRoute, 0, len(cfg.Routing.PathRoutes))
	for i := range cfg.Routing.PathRoutes {
		route := cfg.Routing.PathRoutes[i]
		route.Path = internalrouting.NormalizeNamespacePath(route.Path)
		route.Group = internalrouting.NormalizeGroupName(route.Group)
		route.Fallback = internalrouting.NormalizeFallback(route.Fallback)
		if route.Path == "" || route.Group == "" {
			continue
		}
		if _, exists := seenPaths[route.Path]; exists {
			continue
		}
		seenPaths[route.Path] = struct{}{}
		pathRoutes = append(pathRoutes, route)
	}
	cfg.Routing.PathRoutes = pathRoutes
}

// SanitizeAPIKeyEntries normalizes API key channel-group restrictions.
func (cfg *Config) SanitizeAPIKeyEntries() {
	if cfg == nil || len(cfg.APIKeyEntries) == 0 {
		return
	}
	for i := range cfg.APIKeyEntries {
		entry := &cfg.APIKeyEntries[i]
		entry.AllowedChannelGroups = normalizeStringList(entry.AllowedChannelGroups, internalrouting.NormalizeGroupName)
	}
}
