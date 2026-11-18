package contentmoderation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type Channel struct {
	ChannelType string   `json:"channel_type"`
	ChannelID   string   `json:"channel_id"`
	Name        string   `json:"name"`
	Provider    string   `json:"provider"`
	Tags        []string `json:"tags"`
	Disabled    bool     `json:"disabled"`
	ProfileID   string   `json:"profile_id,omitempty"`
}

type ChannelRef struct {
	ChannelType string
	ChannelID   string
}

type ChannelQuery struct {
	ChannelType string
	Query       string
	Tags        []string
	TagMode     string
	Provider    string
	ProfileID   string
	Page        int
	PageSize    int
}

type ChannelPage struct {
	Items    []Channel `json:"items"`
	Page     int       `json:"page"`
	PageSize int       `json:"page_size"`
	Total    int       `json:"total"`
}

func ListChannels(cfg *config.Config, auths []*coreauth.Auth, bindings []Binding, query ChannelQuery) ChannelPage {
	bindingByChannel := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		bindingByChannel[binding.ChannelType+"\x00"+binding.ChannelID] = binding.ProfileID
	}
	channels := buildChannels(cfg, auths, bindingByChannel)
	filtered := channels[:0]
	for _, channel := range channels {
		if channelMatches(channel, query) {
			filtered = append(filtered, channel)
		}
	}
	page := query.Page
	if page < 1 {
		page = 1
	}
	pageSize := query.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	items := append([]Channel(nil), filtered[start:end]...)
	return ChannelPage{Items: items, Page: page, PageSize: pageSize, Total: len(filtered)}
}

func ChannelExists(cfg *config.Config, auths []*coreauth.Auth, channelType, channelID string) bool {
	channelType = strings.TrimSpace(channelType)
	channelID = strings.TrimSpace(channelID)
	if channelID == "" || !IsChannelType(channelType) {
		return false
	}
	for _, channel := range buildChannels(cfg, auths, nil) {
		if channel.ChannelType == channelType && channel.ChannelID == channelID {
			return true
		}
	}
	return false
}

func ProviderChannelRefs(cfg *config.Config) []ChannelRef {
	if cfg == nil {
		return nil
	}
	refs := make([]ChannelRef, 0, len(cfg.GeminiKey)+len(cfg.ClaudeKey)+len(cfg.BedrockKey)+len(cfg.CodexKey)+len(cfg.OpenCodeGoKey)+len(cfg.ClineKey)+len(cfg.OllamaCloudKey)+len(cfg.CommandCodeKey)+len(cfg.VertexCompatAPIKey)+len(cfg.OpenAICompatibility))
	addKey := func(id string) {
		if id = strings.TrimSpace(id); id != "" {
			refs = append(refs, ChannelRef{ChannelType: ChannelTypeProviderKey, ChannelID: id})
		}
	}
	for _, entry := range cfg.GeminiKey {
		addKey(entry.ID)
	}
	for _, entry := range cfg.ClaudeKey {
		addKey(entry.ID)
	}
	for _, entry := range cfg.BedrockKey {
		addKey(entry.ID)
	}
	for _, entry := range cfg.CodexKey {
		addKey(entry.ID)
	}
	for _, entry := range cfg.OpenCodeGoKey {
		addKey(entry.ID)
	}
	for _, entry := range cfg.ClineKey {
		addKey(entry.ID)
	}
	for _, entry := range cfg.OllamaCloudKey {
		addKey(entry.ID)
	}
	for _, entry := range cfg.CommandCodeKey {
		addKey(entry.ID)
	}
	for _, entry := range cfg.VertexCompatAPIKey {
		addKey(entry.ID)
	}
	for _, provider := range cfg.OpenAICompatibility {
		if id := strings.TrimSpace(provider.ID); id != "" {
			refs = append(refs, ChannelRef{ChannelType: ChannelTypeProvider, ChannelID: id})
		}
		for _, entry := range provider.APIKeyEntries {
			addKey(entry.ID)
		}
	}
	return refs
}

func NormalizeAuthFileChannelID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if parent := strings.TrimSpace(auth.Attributes["gemini_virtual_parent"]); parent != "" {
		return parent
	}
	return strings.TrimSpace(auth.ID)
}

func buildChannels(cfg *config.Config, auths []*coreauth.Auth, bindings map[string]string) []Channel {
	channels := make([]Channel, 0, len(auths)+16)
	seen := make(map[string]struct{})
	add := func(channel Channel) {
		channel.ChannelType = strings.TrimSpace(channel.ChannelType)
		channel.ChannelID = strings.TrimSpace(channel.ChannelID)
		if channel.ChannelID == "" || !IsChannelType(channel.ChannelType) {
			return
		}
		key := channel.ChannelType + "\x00" + channel.ChannelID
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		channel.Name = strings.TrimSpace(channel.Name)
		if channel.Name == "" {
			channel.Name = channel.Provider
		}
		channel.Provider = strings.ToLower(strings.TrimSpace(channel.Provider))
		if channel.Tags == nil {
			channel.Tags = []string{}
		}
		channel.ProfileID = bindings[key]
		channels = append(channels, channel)
	}

	for _, auth := range auths {
		if auth == nil || (strings.TrimSpace(auth.FileName) == "" && strings.TrimSpace(auth.Attributes["path"]) == "") {
			continue
		}
		channelID := NormalizeAuthFileChannelID(auth)
		name := strings.TrimSpace(auth.Label)
		if name == "" {
			name = strings.TrimSpace(auth.FileName)
		}
		if name == "" {
			name = channelID
		}
		add(Channel{
			ChannelType: ChannelTypeAuthFile,
			ChannelID:   channelID,
			Name:        name,
			Provider:    auth.Provider,
			Tags:        authDisplayTags(auth),
			Disabled:    auth.Disabled,
		})
	}
	if cfg != nil {
		for _, entry := range cfg.GeminiKey {
			add(simpleProviderChannel(entry.ID, entry.Name, "gemini", false))
		}
		for _, entry := range cfg.ClaudeKey {
			add(simpleProviderChannel(entry.ID, entry.Name, "claude", false))
		}
		for _, entry := range cfg.BedrockKey {
			add(simpleProviderChannel(entry.ID, entry.Name, "bedrock", false))
		}
		for _, entry := range cfg.CodexKey {
			add(simpleProviderChannel(entry.ID, entry.Name, "codex", false))
		}
		for _, entry := range cfg.OpenCodeGoKey {
			add(simpleProviderChannel(entry.ID, entry.Name, "opencode-go", entry.Disabled))
		}
		for _, entry := range cfg.ClineKey {
			add(simpleProviderChannel(entry.ID, entry.Name, "cline", entry.Disabled))
		}
		for _, entry := range cfg.OllamaCloudKey {
			add(simpleProviderChannel(entry.ID, entry.Name, "ollama-cloud", entry.Disabled))
		}
		for _, entry := range cfg.CommandCodeKey {
			add(simpleProviderChannel(entry.ID, entry.Name, "commandcode", entry.Disabled))
		}
		for _, entry := range cfg.VertexCompatAPIKey {
			add(simpleProviderChannel(entry.ID, "vertex", "vertex", false))
		}
		for _, provider := range cfg.OpenAICompatibility {
			providerName := strings.ToLower(strings.TrimSpace(provider.Name))
			add(Channel{ChannelType: ChannelTypeProvider, ChannelID: provider.ID, Name: provider.Name, Provider: providerName, Tags: []string{}, Disabled: provider.Disabled})
			for index, entry := range provider.APIKeyEntries {
				name := fmt.Sprintf("%s key %d", strings.TrimSpace(provider.Name), index+1)
				add(Channel{ChannelType: ChannelTypeProviderKey, ChannelID: entry.ID, Name: name, Provider: providerName, Tags: []string{}, Disabled: provider.Disabled || entry.Disabled})
			}
		}
	}
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].ChannelType != channels[j].ChannelType {
			return channels[i].ChannelType < channels[j].ChannelType
		}
		left, right := strings.ToLower(channels[i].Name), strings.ToLower(channels[j].Name)
		if left != right {
			return left < right
		}
		return channels[i].ChannelID < channels[j].ChannelID
	})
	return channels
}

func simpleProviderChannel(id, name, provider string, disabled bool) Channel {
	name = strings.TrimSpace(name)
	if name == "" {
		name = provider
	}
	return Channel{ChannelType: ChannelTypeProviderKey, ChannelID: id, Name: name, Provider: provider, Tags: []string{}, Disabled: disabled}
}

func channelMatches(channel Channel, query ChannelQuery) bool {
	if channelType := strings.TrimSpace(query.ChannelType); channelType != "" && channel.ChannelType != channelType {
		return false
	}
	if provider := strings.ToLower(strings.TrimSpace(query.Provider)); provider != "" && channel.Provider != provider {
		return false
	}
	if profileID := strings.TrimSpace(query.ProfileID); profileID != "" && channel.ProfileID != profileID {
		return false
	}
	if needle := strings.ToLower(strings.TrimSpace(query.Query)); needle != "" {
		haystack := strings.ToLower(channel.Name + "\n" + channel.Provider + "\n" + channel.ChannelID)
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	if len(query.Tags) == 0 {
		return true
	}
	tagSet := make(map[string]struct{}, len(channel.Tags))
	for _, tag := range channel.Tags {
		tagSet[strings.ToLower(strings.TrimSpace(tag))] = struct{}{}
	}
	matched := 0
	for _, tag := range query.Tags {
		if _, ok := tagSet[strings.ToLower(strings.TrimSpace(tag))]; ok {
			matched++
		}
	}
	if strings.EqualFold(strings.TrimSpace(query.TagMode), "all") {
		return matched == len(query.Tags)
	}
	return matched > 0
}

func authDisplayTags(auth *coreauth.Auth) []string {
	if auth == nil {
		return []string{}
	}
	if tags, present := metadataStringSlice(auth.Metadata, "display_tags"); present {
		return tags
	}
	values := []string{auth.Provider}
	if plan, _ := auth.Metadata["plan_type"].(string); strings.TrimSpace(plan) != "" {
		values = append(values, plan)
	}
	if custom, _ := metadataStringSlice(auth.Metadata, "custom_tags"); len(custom) > 0 {
		values = append(values, custom...)
	}
	return normalizeTags(values)
}

func metadataStringSlice(metadata map[string]any, key string) ([]string, bool) {
	if metadata == nil {
		return nil, false
	}
	raw, exists := metadata[key]
	if !exists {
		return nil, false
	}
	switch value := raw.(type) {
	case []string:
		return normalizeTags(value), true
	case []any:
		items := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				items = append(items, text)
			}
		}
		return normalizeTags(items), true
	case string:
		return normalizeTags(strings.Split(value, ",")), true
	default:
		return []string{}, true
	}
}

func normalizeTags(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
