package auth

import (
	"strings"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type runtimeConfigSnapshot struct {
	Routing             runtimeRoutingConfigSnapshot
	GeminiKey           []runtimeAPIKeyModelConfig
	ClaudeKey           []runtimeAPIKeyModelConfig
	CodexKey            []runtimeAPIKeyModelConfig
	ClineKey            []runtimeAPIKeyModelConfig
	OllamaCloudKey      []runtimeAPIKeyModelConfig
	CommandCodeKey      []runtimeAPIKeyModelConfig
	BedrockKey          []runtimeBedrockKeyConfig
	VertexCompatAPIKey  []runtimeAPIKeyModelConfig
	OpenAICompatibility []runtimeOpenAICompatibilityConfig
}

type runtimeRoutingConfigSnapshot struct {
	Strategy            string
	IncludeDefaultGroup bool
	ChannelGroups       []runtimeRoutingChannelGroup
}

type runtimeRoutingChannelGroup struct {
	Name string
	// Strategy is the legacy mirror of Scheduling; selection reads Scheduling.
	Strategy           string
	Scheduling         runtimeGroupScheduling
	Match              runtimeChannelGroupMatch
	ExcludeFromDefault bool
	Priority           int
	ChannelPriorities  map[string]int
	AllowedModels      []string
}

type runtimeGroupScheduling struct {
	Distribution   string
	StickyEnabled  bool
	StickyMax      int
	StickyRelease  float64
	ChannelWeights map[string]int
}

type runtimeChannelGroupMatch struct {
	Prefixes []string
	Channels []string
	Tags     []string
}

type runtimeAPIKeyModelConfig struct {
	APIKey  string
	BaseURL string
	Models  []runtimeModelAliasEntry
}

func (k runtimeAPIKeyModelConfig) GetAPIKey() string  { return k.APIKey }
func (k runtimeAPIKeyModelConfig) GetBaseURL() string { return k.BaseURL }

type runtimeBedrockKeyConfig struct {
	AuthMode    string
	APIKey      string
	AccessKeyID string
	BaseURL     string
	Region      string
	Models      []runtimeModelAliasEntry
}

func (k runtimeBedrockKeyConfig) GetAPIKey() string {
	if strings.EqualFold(strings.TrimSpace(k.AuthMode), "api-key") {
		return k.APIKey
	}
	if strings.TrimSpace(k.APIKey) != "" {
		return k.APIKey
	}
	return k.AccessKeyID
}

func (k runtimeBedrockKeyConfig) GetBaseURL() string { return k.BaseURL }

type runtimeOpenAICompatibilityConfig struct {
	Name   string
	Models []runtimeModelAliasEntry
}

type runtimeModelAliasEntry struct {
	Name  string
	Alias string
}

func (m runtimeModelAliasEntry) GetName() string  { return m.Name }
func (m runtimeModelAliasEntry) GetAlias() string { return m.Alias }

var emptyRuntimeConfigSnapshot = &runtimeConfigSnapshot{}

func newRuntimeConfigSnapshot(cfg *sdkconfig.Config) *runtimeConfigSnapshot {
	if cfg == nil {
		return &runtimeConfigSnapshot{}
	}
	return &runtimeConfigSnapshot{
		Routing: runtimeRoutingConfigSnapshot{
			Strategy:            normalizeOptionalRuntimeRoutingStrategy(cfg.Routing.Strategy),
			IncludeDefaultGroup: cfg.Routing.IncludeDefaultGroup,
			ChannelGroups:       cloneRuntimeRoutingChannelGroups(cfg.Routing.ChannelGroups),
		},
		GeminiKey:           cloneRuntimeAPIKeyModelConfigs(cfg.GeminiKey),
		ClaudeKey:           cloneRuntimeAPIKeyModelConfigs(cfg.ClaudeKey),
		CodexKey:            cloneRuntimeAPIKeyModelConfigs(cfg.CodexKey),
		ClineKey:            cloneRuntimeClineModelConfigs(cfg.ClineKey),
		OllamaCloudKey:      cloneRuntimeAPIKeyModelConfigs(cfg.OllamaCloudKey),
		CommandCodeKey:      cloneRuntimeAPIKeyModelConfigs(cfg.CommandCodeKey),
		BedrockKey:          cloneRuntimeBedrockKeyConfigs(cfg.BedrockKey),
		VertexCompatAPIKey:  cloneRuntimeAPIKeyModelConfigs(cfg.VertexCompatAPIKey),
		OpenAICompatibility: cloneRuntimeOpenAICompatibilityConfigs(cfg.OpenAICompatibility),
	}
}

type runtimeConfigSnapshotSet map[string]*runtimeConfigSnapshot

func (m *Manager) currentRuntimeConfig() *runtimeConfigSnapshot {
	return m.currentRuntimeConfigForTenant(defaultTenantID)
}

func (m *Manager) currentRuntimeConfigForTenant(tenantID string) *runtimeConfigSnapshot {
	if m == nil {
		return emptyRuntimeConfigSnapshot
	}
	set, _ := m.runtimeConfig.Load().(runtimeConfigSnapshotSet)
	if cfg := set[normalizedTenantID(tenantID)]; cfg != nil {
		return cfg
	}
	return emptyRuntimeConfigSnapshot
}

func cloneRuntimeRoutingChannelGroups(groups []sdkconfig.RoutingChannelGroup) []runtimeRoutingChannelGroup {
	if len(groups) == 0 {
		return nil
	}
	out := make([]runtimeRoutingChannelGroup, 0, len(groups))
	for i := range groups {
		group := groups[i]
		out = append(out, runtimeRoutingChannelGroup{
			Name:               group.Name,
			Strategy:           normalizeOptionalRuntimeRoutingStrategy(group.Strategy),
			Scheduling:         cloneRuntimeGroupScheduling(group),
			Match:              cloneRuntimeChannelGroupMatch(group.Match),
			ExcludeFromDefault: group.ExcludeFromDefault,
			Priority:           group.Priority,
			ChannelPriorities:  cloneStringIntMap(group.ChannelPriorities),
			AllowedModels:      cloneStringSlice(group.AllowedModels),
		})
	}
	return out
}

// cloneRuntimeGroupScheduling reads the group's scheduling block, falling back
// to the legacy strategy/priorities pair for configs that predate it or that
// were written by an older binary between deploys.
func cloneRuntimeGroupScheduling(group sdkconfig.RoutingChannelGroup) runtimeGroupScheduling {
	scheduling := group.Scheduling
	if strings.TrimSpace(scheduling.Distribution) == "" && !scheduling.Sticky.Enabled && len(scheduling.ChannelWeights) == 0 {
		switch normalizeOptionalRuntimeRoutingStrategy(group.Strategy) {
		case "session-sticky":
			scheduling.Distribution = sdkconfig.DistributionWeighted
			scheduling.Sticky.Enabled = true
			scheduling.Sticky.MaxRequests = sdkconfig.DefaultStickyMaxRequests
		case "fill-first":
			scheduling.Distribution = sdkconfig.DistributionFillFirst
		default:
			scheduling.Distribution = sdkconfig.DistributionWeighted
		}
		scheduling.ChannelWeights = sdkconfig.MigrateLegacyPriorities(group.ChannelPriorities)
	}
	return runtimeGroupScheduling{
		Distribution:   sdkconfig.NormalizeDistribution(scheduling.Distribution),
		StickyEnabled:  scheduling.Sticky.Enabled,
		StickyMax:      scheduling.Sticky.MaxRequests,
		StickyRelease:  scheduling.Sticky.ReleaseAtLoad,
		ChannelWeights: cloneStringIntMap(scheduling.ChannelWeights),
	}
}

func cloneRuntimeChannelGroupMatch(match sdkconfig.ChannelGroupMatch) runtimeChannelGroupMatch {
	return runtimeChannelGroupMatch{
		Prefixes: cloneStringSlice(match.Prefixes),
		Channels: cloneStringSlice(match.Channels),
		Tags:     cloneStringSlice(match.Tags),
	}
}

func cloneRuntimeAPIKeyModelConfigs[T interface {
	GetAPIKey() string
	GetBaseURL() string
}](entries []T) []runtimeAPIKeyModelConfig {
	if len(entries) == 0 {
		return nil
	}
	out := make([]runtimeAPIKeyModelConfig, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		out = append(out, runtimeAPIKeyModelConfig{
			APIKey:  entry.GetAPIKey(),
			BaseURL: entry.GetBaseURL(),
			Models:  modelsForRuntimeConfigEntry(entry),
		})
	}
	return out
}

func cloneRuntimeClineModelConfigs(entries []sdkconfig.ClineKey) []runtimeAPIKeyModelConfig {
	if len(entries) == 0 {
		return nil
	}
	out := make([]runtimeAPIKeyModelConfig, 0, len(entries))
	for i := range entries {
		out = append(out, runtimeAPIKeyModelConfig{
			APIKey:  entries[i].APIKey,
			BaseURL: entries[i].BaseURL,
			Models:  cloneRuntimeModelAliasEntries(entries[i].Models),
		})
	}
	return out
}

func modelsForRuntimeConfigEntry[T any](entry T) []runtimeModelAliasEntry {
	switch typed := any(entry).(type) {
	case sdkconfig.GeminiKey:
		return cloneRuntimeModelAliasEntries(typed.Models)
	case sdkconfig.ClaudeKey:
		return cloneRuntimeModelAliasEntries(typed.Models)
	case sdkconfig.CodexKey:
		return cloneRuntimeModelAliasEntries(typed.Models)
	case sdkconfig.ClineKey:
		return cloneRuntimeModelAliasEntries(typed.Models)
	case sdkconfig.OllamaCloudKey:
		return cloneRuntimeModelAliasEntries(typed.Models)
	case sdkconfig.CommandCodeKey:
		return cloneRuntimeModelAliasEntries(typed.Models)
	case sdkconfig.VertexCompatKey:
		return cloneRuntimeModelAliasEntries(typed.Models)
	default:
		return nil
	}
}

func cloneRuntimeBedrockKeyConfigs(entries []sdkconfig.BedrockKey) []runtimeBedrockKeyConfig {
	if len(entries) == 0 {
		return nil
	}
	out := make([]runtimeBedrockKeyConfig, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		out = append(out, runtimeBedrockKeyConfig{
			AuthMode:    entry.AuthMode,
			APIKey:      entry.APIKey,
			AccessKeyID: entry.AccessKeyID,
			BaseURL:     entry.BaseURL,
			Region:      entry.Region,
			Models:      cloneRuntimeModelAliasEntries(entry.Models),
		})
	}
	return out
}

func cloneRuntimeOpenAICompatibilityConfigs(entries []sdkconfig.OpenAICompatibility) []runtimeOpenAICompatibilityConfig {
	if len(entries) == 0 {
		return nil
	}
	out := make([]runtimeOpenAICompatibilityConfig, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		out = append(out, runtimeOpenAICompatibilityConfig{
			Name:   entry.Name,
			Models: cloneRuntimeModelAliasEntries(entry.Models),
		})
	}
	return out
}

func cloneRuntimeModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []runtimeModelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]runtimeModelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, runtimeModelAliasEntry{
			Name:  models[i].GetName(),
			Alias: models[i].GetAlias(),
		})
	}
	return out
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneStringIntMap(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]int, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func normalizeOptionalRuntimeRoutingStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "":
		return ""
	case "session-sticky", "sessionsticky", "sticky", "ss":
		return "session-sticky"
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	default:
		return "round-robin"
	}
}
