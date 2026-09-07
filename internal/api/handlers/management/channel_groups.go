package management

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type channelDescriptor struct {
	Name              string
	Prefix            string
	Source            string
	Disabled          bool
	DisabledAuthority int
	DefaultTags       []string
	CustomTags        []string
	HiddenDefaultTags []string
	DisplayTags       []string
}

const (
	channelDisabledAuthorityRuntime = 1
	channelDisabledAuthorityConfig  = 2
)

// channelGroupScheduling mirrors config.GroupScheduling on the wire. It is
// always emitted (no omitempty on the block) so the panel can render the
// resolved defaults for implicit groups such as "default", which have no stored
// configuration of their own.
type channelGroupScheduling struct {
	Distribution        string         `json:"distribution"`
	StickyEnabled       bool           `json:"sticky-enabled"`
	StickyMaxRequests   int            `json:"sticky-max-requests,omitempty"`
	StickyReleaseAtLoad float64        `json:"sticky-release-at-load,omitempty"`
	ChannelWeights      map[string]int `json:"channel-weights,omitempty"`
}

type channelGroupItem struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Strategy is the legacy mirror of Scheduling, retained so an older panel
	// build keeps working against a newer backend during a staged rollout.
	Strategy           string                      `json:"strategy,omitempty"`
	Scheduling         channelGroupScheduling      `json:"scheduling"`
	Priority           int                         `json:"priority,omitempty"`
	ExcludeFromDefault bool                        `json:"exclude-from-default,omitempty"`
	Implicit           bool                        `json:"implicit"`
	Prefixes           []string                    `json:"prefixes,omitempty"`
	Tags               []string                    `json:"tags,omitempty"`
	Channels           []string                    `json:"channels,omitempty"`
	ChannelDetails     []channelGroupChannelDetail `json:"channel-details,omitempty"`
	AllowedModels      []string                    `json:"allowed-models,omitempty"`
	PathRoutes         []string                    `json:"path-routes,omitempty"`
}

func schedulingItemFrom(group config.RoutingChannelGroup) channelGroupScheduling {
	scheduling := group.Scheduling
	if strings.TrimSpace(scheduling.Distribution) == "" && !scheduling.Sticky.Enabled && len(scheduling.ChannelWeights) == 0 {
		scheduling = config.SchedulingFromLegacyGroup(group)
	}
	return channelGroupScheduling{
		Distribution:        config.NormalizeDistribution(scheduling.Distribution),
		StickyEnabled:       scheduling.Sticky.Enabled,
		StickyMaxRequests:   scheduling.Sticky.MaxRequests,
		StickyReleaseAtLoad: scheduling.Sticky.ReleaseAtLoad,
		ChannelWeights:      scheduling.ChannelWeights,
	}
}

// defaultSchedulingItem describes an implicit group that has no stored config.
func defaultSchedulingItem() channelGroupScheduling {
	return channelGroupScheduling{Distribution: config.DistributionWeighted}
}

func collectChannelDescriptors(cfg *config.Config, auths []*coreauth.Auth) []channelDescriptor {
	items := make([]channelDescriptor, 0)
	push := func(name, prefix, source string, disabled bool, disabledAuthority int, tags managementauthfiles.TagPayload) {
		name = strings.TrimSpace(name)
		prefix = internalrouting.NormalizeGroupName(prefix)
		if name == "" && prefix == "" {
			return
		}
		items = append(items, channelDescriptor{
			Name:              name,
			Prefix:            prefix,
			Source:            source,
			Disabled:          disabled,
			DisabledAuthority: disabledAuthority,
			DefaultTags:       append([]string{}, tags.DefaultTags...),
			CustomTags:        append([]string{}, tags.CustomTags...),
			HiddenDefaultTags: append([]string{}, tags.HiddenDefaultTags...),
			DisplayTags:       append([]string{}, tags.DisplayTags...),
		})
	}

	if cfg != nil {
		for _, entry := range cfg.GeminiKey {
			push(entry.Name, entry.Prefix, "gemini", providerExcludesAllModels(entry.ExcludedModels), channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("gemini", nil))
		}
		for _, entry := range cfg.ClaudeKey {
			push(entry.Name, entry.Prefix, "claude", providerExcludesAllModels(entry.ExcludedModels), channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("claude", nil))
		}
		for _, entry := range cfg.BedrockKey {
			push(entry.Name, entry.Prefix, "bedrock", providerExcludesAllModels(entry.ExcludedModels), channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("bedrock", nil))
		}
		for _, entry := range cfg.CodexKey {
			push(entry.Name, entry.Prefix, "codex", providerExcludesAllModels(entry.ExcludedModels), channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("codex", nil))
		}
		for _, entry := range cfg.OpenCodeGoKey {
			push(entry.Name, entry.Prefix, "opencode-go", providerExcludesAllModels(entry.ExcludedModels), channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("opencode-go", nil))
		}
		for _, entry := range cfg.ClineKey {
			push(entry.Name, entry.Prefix, "cline", providerExcludesAllModels(entry.ExcludedModels), channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("cline", nil))
		}
		for _, entry := range cfg.OllamaCloudKey {
			push(entry.Name, entry.Prefix, "ollama-cloud", providerExcludesAllModels(entry.ExcludedModels), channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("ollama-cloud", nil))
		}
		for _, entry := range cfg.CommandCodeKey {
			push(entry.Name, entry.Prefix, "commandcode", providerExcludesAllModels(entry.ExcludedModels), channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("commandcode", nil))
		}
		for _, entry := range cfg.VertexCompatAPIKey {
			push("", entry.Prefix, "vertex", false, channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("vertex", nil))
		}
		for _, entry := range cfg.OpenAICompatibility {
			push(entry.Name, entry.Prefix, "openai", entry.Disabled, channelDisabledAuthorityConfig, managementauthfiles.BuildTagPayloadFromValues("openai", nil))
		}
	}

	for _, auth := range auths {
		if !includeAuthInChannelGroups(auth) {
			continue
		}
		push(
			auth.ChannelName(),
			auth.Prefix,
			auth.Provider,
			authChannelDisabled(auth),
			channelDisabledAuthorityRuntime,
			managementauthfiles.BuildTagPayload(auth),
		)
	}

	return items
}

func providerExcludesAllModels(excludedModels []string) bool {
	for _, model := range excludedModels {
		if strings.TrimSpace(model) == "*" {
			return true
		}
	}
	return false
}

func authExcludesAllModels(auth *coreauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "apikey") {
		return false
	}
	return providerExcludesAllModels(strings.Split(auth.Attributes["excluded_models"], ","))
}

func authChannelDisabled(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return auth.Disabled || auth.Status == coreauth.StatusDisabled || authExcludesAllModels(auth)
}

func includeAuthInChannelGroups(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	statusMessage := strings.TrimSpace(auth.StatusMessage)
	if strings.EqualFold(statusMessage, "removed via management api") ||
		strings.EqualFold(statusMessage, "removed via config update") {
		return false
	}
	if managementauthfiles.IsRuntimeOnly(auth) && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return false
	}
	return true
}

func routingConfigFrom(cfg *config.Config) config.RoutingConfig {
	// Callers (GetChannelGroups / PutRoutingConfig) already put the correct
	// tenant-scoped Routing on cfg. Do not re-read System tenant store here —
	// that leaked other tenants' channel groups into business-tenant UIs.
	if cfg == nil {
		return config.RoutingConfig{IncludeDefaultGroup: true}
	}
	return cfg.Routing
}

func buildChannelGroupItems(cfg *config.Config, auths []*coreauth.Auth) []channelGroupItem {
	items := collectChannelDescriptors(cfg, auths)
	knownPaths := make(map[string][]string)
	routingCfg := routingConfigFrom(cfg)
	if known, err := collectKnownChannels(cfg, auths, ""); err == nil {
		routingCfg = canonicalizeRoutingConfigChannels(routingCfg, known)
	}
	for _, route := range routingCfg.PathRoutes {
		group := internalrouting.NormalizeGroupName(route.Group)
		if group == "" {
			continue
		}
		knownPaths[group] = append(knownPaths[group], route.Path)
	}

	groupMap := make(map[string]*channelGroupItem)
	configuredChannelsByGroup := make(map[string][]string)
	ensureGroup := func(name string, implicit bool) *channelGroupItem {
		name = internalrouting.NormalizeGroupName(name)
		if name == "" {
			return nil
		}
		if existing, ok := groupMap[name]; ok {
			if !implicit {
				existing.Implicit = false
			}
			return existing
		}
		item := &channelGroupItem{Name: name, Implicit: implicit, Scheduling: defaultSchedulingItem()}
		groupMap[name] = item
		return item
	}

	for _, group := range routingCfg.ChannelGroups {
		item := ensureGroup(group.Name, false)
		if item == nil {
			continue
		}
		item.Description = group.Description
		item.Strategy = group.Strategy
		item.Scheduling = schedulingItemFrom(group)
		item.Priority = group.Priority
		item.ExcludeFromDefault = group.ExcludeFromDefault
		item.AllowedModels = append(item.AllowedModels, group.AllowedModels...)
		item.Prefixes = append(item.Prefixes, group.Match.Prefixes...)
		item.Tags = append(item.Tags, group.Match.Tags...)
		configuredChannelsByGroup[item.Name] = append(configuredChannelsByGroup[item.Name], group.Match.Channels...)
	}

	includeDefault := cfg == nil || routingCfg.IncludeDefaultGroup
	if includeDefault {
		ensureGroup("default", true)
	}

	for _, channel := range items {
		if channel.Prefix != "" {
			ensureGroup(channel.Prefix, true)
		} else if includeDefault {
			ensureGroup("default", true)
		}
	}

	for _, channel := range items {
		prefix := internalrouting.NormalizeGroupName(channel.Prefix)
		channelName := strings.TrimSpace(channel.Name)
		excludedFromDefault := channelExcludedFromImplicitDefault(routingCfg, channel)
		for groupName, group := range groupMap {
			matched := false
			for _, candidatePrefix := range group.Prefixes {
				if prefix != "" && prefix == internalrouting.NormalizeGroupName(candidatePrefix) {
					matched = true
					break
				}
			}
			if !matched {
				for _, candidateChannel := range configuredChannelsByGroup[groupName] {
					if channelName != "" && strings.EqualFold(strings.TrimSpace(candidateChannel), channelName) {
						matched = true
						break
					}
				}
			}
			if !matched && channelMatchesAnyTag(channel, group.Tags) {
				matched = true
			}
			if !matched {
				if group.Name == "default" && prefix == "" && includeDefault && !excludedFromDefault {
					matched = true
				} else if prefix != "" && group.Name == prefix {
					matched = true
				}
			}
			if matched && channelName != "" {
				group.Channels = append(group.Channels, channelName)
				group.ChannelDetails = append(group.ChannelDetails, channelGroupChannelDetail{
					Name:              channelName,
					Source:            channel.Source,
					Disabled:          channel.Disabled,
					disabledAuthority: channel.DisabledAuthority,
					DefaultTags:       append([]string{}, channel.DefaultTags...),
					CustomTags:        append([]string{}, channel.CustomTags...),
					HiddenDefaultTags: append([]string{}, channel.HiddenDefaultTags...),
					DisplayTags:       append([]string{}, channel.DisplayTags...),
				})
			}
		}
	}

	out := make([]channelGroupItem, 0, len(groupMap))
	for name, item := range groupMap {
		item.Name = name
		item.Prefixes = uniqueSortedStrings(item.Prefixes, internalrouting.NormalizeGroupName)
		item.Tags = uniqueSortedStrings(item.Tags, config.NormalizeRoutingTag)
		item.Channels = uniqueSortedStrings(item.Channels, func(value string) string { return strings.TrimSpace(value) })
		item.ChannelDetails = uniqueSortedChannelDetails(item.ChannelDetails)
		item.PathRoutes = uniqueSortedStrings(knownPaths[name], internalrouting.NormalizeNamespacePath)
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func channelExcludedFromImplicitDefault(routingCfg config.RoutingConfig, channel channelDescriptor) bool {
	if internalrouting.NormalizeGroupName(channel.Prefix) != "" {
		return false
	}
	for _, group := range routingCfg.ChannelGroups {
		if !group.ExcludeFromDefault {
			continue
		}
		groupName := internalrouting.NormalizeGroupName(group.Name)
		if groupName == "" || groupName == "default" {
			continue
		}
		if channelGroupMatchesDescriptor(group, channel) {
			return true
		}
	}
	return false
}

func channelGroupMatchesDescriptor(group config.RoutingChannelGroup, channel channelDescriptor) bool {
	prefix := internalrouting.NormalizeGroupName(channel.Prefix)
	for _, candidatePrefix := range group.Match.Prefixes {
		if prefix != "" && prefix == internalrouting.NormalizeGroupName(candidatePrefix) {
			return true
		}
	}
	channelName := strings.TrimSpace(channel.Name)
	for _, candidateChannel := range group.Match.Channels {
		if channelName != "" && strings.EqualFold(strings.TrimSpace(candidateChannel), channelName) {
			return true
		}
	}
	return channelMatchesAnyTag(channel, group.Match.Tags)
}

func channelMatchesAnyTag(channel channelDescriptor, tags []string) bool {
	if len(tags) == 0 {
		return false
	}
	displayTags := make(map[string]struct{}, len(channel.DisplayTags))
	for _, tag := range channel.DisplayTags {
		normalized := config.NormalizeRoutingTag(tag)
		if normalized != "" {
			displayTags[normalized] = struct{}{}
		}
	}
	if len(displayTags) == 0 {
		return false
	}
	for _, tag := range tags {
		if _, ok := displayTags[config.NormalizeRoutingTag(tag)]; ok {
			return true
		}
	}
	return false
}

func uniqueSortedStrings(values []string, normalizer func(string) string) []string {
	if len(values) == 0 {
		return nil
	}
	type pair struct {
		key   string
		value string
	}
	seen := make(map[string]pair, len(values))
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		if normalizer != nil {
			normalized = normalizer(normalized)
		}
		if normalized == "" {
			continue
		}
		key := strings.ToLower(normalized)
		seen[key] = pair{key: key, value: normalized}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for _, item := range seen {
		out = append(out, item.value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (h *Handler) GetChannelGroups(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.ListForTenant(tenantID)
	}
	cfg := h.cfg
	if tenantID != identity.SystemTenantID {
		cfg = &config.Config{Routing: currentRoutingConfigForTenant(h.cfg, tenantID)}
	}
	c.JSON(http.StatusOK, gin.H{"items": buildChannelGroupItems(cfg, auths)})
}

func reservedPathRoutePrefixes() []string {
	return []string{
		"/v1",
		"/v1beta",
		"/v0",
		"/api",
		"/manage",
		"/auth",
		"/anthropic",
		"/codex",
		"/google",
		"/iflow",
		"/antigravity",
	}
}

func validateRoutingAndAPIKeyRestrictions(cfg *config.Config, auths []*coreauth.Auth) error {
	if cfg == nil {
		return nil
	}

	routingCfg := routingConfigFrom(cfg)
	known, err := collectKnownChannels(cfg, auths, "")
	if err != nil {
		return err
	}
	routingCfg = canonicalizeRoutingConfigChannels(routingCfg, known)
	apiKeyEntries := canonicalizeAPIKeyEntriesChannels(cfg.APIKeyEntries, known)

	groups := buildChannelGroupItems(cfg, auths)
	knownGroups := make(map[string]channelGroupItem, len(groups))
	for _, group := range groups {
		knownGroups[group.Name] = group
	}

	seenGroupNames := make(map[string]struct{}, len(routingCfg.ChannelGroups))
	for _, group := range routingCfg.ChannelGroups {
		name := internalrouting.NormalizeGroupName(group.Name)
		if name == "" {
			return fmt.Errorf("routing.channel-groups contains an empty name")
		}
		if _, exists := seenGroupNames[name]; exists {
			return fmt.Errorf("duplicate channel group %q", group.Name)
		}
		seenGroupNames[name] = struct{}{}
		known, exists := knownGroups[name]
		if !exists {
			return fmt.Errorf("channel group %q does not match any known channel", group.Name)
		}
		if err := validateGroupWeights(group, known); err != nil {
			return err
		}
	}

	seenPaths := make(map[string]struct{}, len(routingCfg.PathRoutes))
	for _, route := range routingCfg.PathRoutes {
		path := internalrouting.NormalizeNamespacePath(route.Path)
		if path == "" {
			return fmt.Errorf("invalid path route %q", route.Path)
		}
		if _, exists := seenPaths[path]; exists {
			return fmt.Errorf("duplicate path route %q", path)
		}
		seenPaths[path] = struct{}{}
		for _, reserved := range reservedPathRoutePrefixes() {
			if path == reserved {
				return fmt.Errorf("path route %q conflicts with reserved internal path", path)
			}
		}
		group := internalrouting.NormalizeGroupName(route.Group)
		if _, exists := knownGroups[group]; !exists {
			return fmt.Errorf("path route %q references unknown channel group %q", path, route.Group)
		}
	}

	if len(apiKeyEntries) == 0 {
		return nil
	}
	channelGroupMap := make(map[string]map[string]struct{}, len(groups))
	for _, group := range groups {
		set := make(map[string]struct{}, len(group.Channels))
		for _, channel := range group.Channels {
			set[strings.ToLower(strings.TrimSpace(channel))] = struct{}{}
		}
		channelGroupMap[group.Name] = set
	}

	for _, entry := range apiKeyEntries {
		allowedGroups := uniqueChannelGroups(entry.AllowedChannelGroups)
		for _, group := range allowedGroups {
			if _, exists := knownGroups[group]; !exists {
				return fmt.Errorf("api-key %q references unknown channel group %q", strings.TrimSpace(entry.Name), group)
			}
		}
		if len(allowedGroups) == 0 || len(entry.AllowedChannels) == 0 {
			continue
		}
		intersectionFound := false
		for _, channel := range entry.AllowedChannels {
			key := strings.ToLower(strings.TrimSpace(channel))
			if key == "" {
				continue
			}
			for _, group := range allowedGroups {
				if _, ok := channelGroupMap[group][key]; ok {
					intersectionFound = true
					break
				}
			}
			if intersectionFound {
				break
			}
		}
		if !intersectionFound {
			return fmt.Errorf("api-key %q allowed-channels do not belong to allowed-channel-groups", strings.TrimSpace(entry.Name))
		}
	}

	return nil
}

// validateGroupWeights rejects a group whose every resolved member is weighted
// 0. Such a group cannot serve a single request, and letting it save produced a
// runtime error far away from the setting that caused it.
func validateGroupWeights(group config.RoutingChannelGroup, resolved channelGroupItem) error {
	weights := group.Scheduling.ChannelWeights
	if len(weights) == 0 {
		weights = group.ChannelPriorities
	}
	if len(weights) == 0 || len(resolved.Channels) == 0 {
		return nil
	}
	// A member without an entry keeps the default weight 1, so the group stays
	// usable as long as one member is unconfigured or positively weighted.
	for _, channel := range resolved.Channels {
		weight, configured := lookupChannelWeight(weights, channel)
		if !configured || weight > 0 {
			return nil
		}
	}
	return fmt.Errorf("channel group %q has every channel weighted 0, so it cannot serve any request", group.Name)
}

func lookupChannelWeight(weights map[string]int, channel string) (int, bool) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return 0, false
	}
	for name, weight := range weights {
		if strings.EqualFold(strings.TrimSpace(name), channel) {
			return weight, true
		}
	}
	return 0, false
}

func channelGroupMatchesAnyDescriptor(group config.RoutingChannelGroup, descriptors []channelDescriptor) bool {
	name := internalrouting.NormalizeGroupName(group.Name)
	for _, descriptor := range descriptors {
		prefix := internalrouting.NormalizeGroupName(descriptor.Prefix)
		channel := strings.TrimSpace(descriptor.Name)
		if name != "" && prefix != "" && name == prefix {
			return true
		}
		for _, candidatePrefix := range group.Match.Prefixes {
			if prefix != "" && prefix == internalrouting.NormalizeGroupName(candidatePrefix) {
				return true
			}
		}
		for _, candidateChannel := range group.Match.Channels {
			if channel != "" && strings.EqualFold(strings.TrimSpace(candidateChannel), channel) {
				return true
			}
		}
		if channelMatchesAnyTag(descriptor, group.Match.Tags) {
			return true
		}
	}
	return false
}
