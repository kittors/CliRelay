package modelcatalog

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Model owner scope: resolves which model owners a channel/group selection maps
// to, so DB-backed catalog rows can be scoped the same way registry models are.

func (s *Service) modelOwnerScope(channels []string, groups map[string]struct{}) (map[string]bool, map[string]struct{}) {
	ownerMappings := s.authGroupOwnerMappingMap()
	providerOwners := s.modelConfigOwnersBySource()
	ownerKeys := make(map[string]bool)
	explicitModels := make(map[string]struct{})
	if s == nil {
		return ownerKeys, explicitModels
	}
	auths := []*coreauth.Auth(nil)
	if s.authManager != nil {
		auths = s.authManager.ListUsableForTenant(s.tenantID)
	}
	addOwnersForChannel := func(channel string) {
		for _, owner := range ownersForChannel(channel, auths, ownerMappings, providerOwners) {
			ownerKeys[owner] = true
		}
	}
	addOwnersForAuth := func(auth *coreauth.Auth) {
		for _, channel := range auth.ChannelIdentifiers() {
			addOwnersForChannel(channel)
		}
	}
	if routing := tenantRoutingConfig(s.tenantID, s.cfg); routing != nil {
		for _, group := range routing.ChannelGroups {
			groupName := internalrouting.NormalizeGroupName(group.Name)
			if _, ok := groups[groupName]; !ok {
				continue
			}
			for _, model := range group.AllowedModels {
				model = strings.ToLower(strings.TrimSpace(model))
				if model != "" {
					explicitModels[model] = struct{}{}
				}
			}
			for _, channel := range group.Match.Channels {
				addOwnersForChannel(channel)
			}
			for _, auth := range auths {
				if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
					continue
				}
				if routingGroupMatchesAuthForModelScope(group, auth) {
					addOwnersForAuth(auth)
				}
			}
		}
	}
	if len(groups) == 0 {
		for _, channel := range channels {
			addOwnersForChannel(channel)
		}
	}
	// The default pool is not a configured group with match rules, so the loop
	// above resolves no accounts for it and its DB-backed catalog rows would never
	// find an owner. Ask the auth manager instead, which owns the membership rule
	// (include-default-group, prefixed accounts, exclude-from-default).
	if _, wantsDefault := groups[defaultChannelGroupName]; wantsDefault && s.authManager != nil {
		for _, auth := range auths {
			if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
				continue
			}
			if _, inPool := s.authManager.ChannelGroupsForAuthForTenant(s.tenantID, auth)[defaultChannelGroupName]; inPool {
				addOwnersForAuth(auth)
			}
		}
	}
	return ownerKeys, explicitModels
}

func routingGroupMatchesAuthForModelScope(group config.RoutingChannelGroup, auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	prefix := internalrouting.NormalizeGroupName(auth.Prefix)
	for _, candidate := range group.Match.Prefixes {
		if prefix != "" && prefix == internalrouting.NormalizeGroupName(candidate) {
			return true
		}
	}
	for _, channel := range group.Match.Channels {
		if authChannelMatches(auth, channel) {
			return true
		}
	}
	return authMatchesRoutingTags(auth, group.Match.Tags)
}

func authMatchesRoutingTags(auth *coreauth.Auth, tags []string) bool {
	if auth == nil || len(tags) == 0 {
		return false
	}
	displayTags := make(map[string]struct{})
	for _, tag := range managementauthfiles.BuildTagPayload(auth).DisplayTags {
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

func (s *Service) authGroupOwnerMappingMap() map[string]string {
	rows := modelconfigsettings.ListAuthGroupOwnerMappingsForTenant(s.tenantID)
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		authGroup := normalizeAuthGroupKey(row.AuthGroup)
		owner := normalizeModelOwnerKey(row.Owner)
		if authGroup == "" || owner == "" {
			continue
		}
		out[authGroup] = owner
	}
	return out
}

func (s *Service) modelConfigOwnersBySource() map[string][]string {
	rows := modelconfigsettings.ListAllConfigsForTenant(s.tenantID)
	ownersBySource := make(map[string]map[string]struct{})
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		source := normalizeAuthGroupKey(row.Source)
		owner := normalizeModelOwnerKey(row.OwnedBy)
		if source == "" || owner == "" {
			continue
		}
		if ownersBySource[source] == nil {
			ownersBySource[source] = make(map[string]struct{})
		}
		ownersBySource[source][owner] = struct{}{}
	}
	out := make(map[string][]string, len(ownersBySource))
	for source, owners := range ownersBySource {
		for owner := range owners {
			out[source] = append(out[source], owner)
		}
	}
	return out
}

func ownersForChannel(channel string, auths []*coreauth.Auth, ownerMappings map[string]string, providerOwners map[string][]string) []string {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return nil
	}
	owners := make(map[string]bool)
	addMappedOwner := func(value string) {
		key := normalizeAuthGroupKey(value)
		if key == "" {
			return
		}
		if owner := ownerMappings[key]; owner != "" {
			owners[owner] = true
		}
	}
	addProviderOwners := func(value string) {
		key := normalizeAuthGroupKey(value)
		if key == "" {
			return
		}
		for _, owner := range providerOwners[key] {
			if owner != "" {
				owners[owner] = true
			}
		}
	}
	addMappedOwner(channel)
	addProviderOwners(channel)
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if !authChannelMatches(auth, channel) {
			continue
		}
		addMappedOwner(auth.Provider)
		addMappedOwner(auth.ChannelName())
		addProviderOwners(auth.Provider)
		addProviderOwners(auth.ChannelName())
		for _, identifier := range auth.ChannelIdentifiers() {
			addMappedOwner(identifier)
			addProviderOwners(identifier)
		}
	}
	out := make([]string, 0, len(owners))
	for owner := range owners {
		out = append(out, owner)
	}
	return out
}

func authChannelMatches(auth *coreauth.Auth, channel string) bool {
	if auth == nil {
		return false
	}
	for _, identifier := range auth.ChannelIdentifiers() {
		if strings.EqualFold(strings.TrimSpace(identifier), channel) {
			return true
		}
	}
	return false
}

func normalizeAuthGroupKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeModelOwnerKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), "-"))
}
