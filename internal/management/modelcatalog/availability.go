package modelcatalog

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Availability contract:
// - Owner: model availability query boundary.
// - Responsibility: turn registry state plus stored capabilities into management-facing availability DTOs.
func (s *Service) ConfiguredAvailability(allowedChannelsRaw, allowedGroupsRaw string) map[string]any {
	modelRegistry := registry.GetGlobalRegistry()
	allModels := s.effectiveModels(modelRegistry.GetAvailableModels("openai"), allowedChannelsRaw, allowedGroupsRaw)
	authByID := s.authByID()
	usesMappedOwners := false
	var ownerMappings map[string]string
	var ownerKeys map[string]bool
	if shouldUseDefaultMappedOwnerScope(allowedChannelsRaw, allowedGroupsRaw) {
		if rows, keys, ok := s.defaultMappedOwnerRows(); ok {
			usesMappedOwners = true
			ownerKeys = keys
			ownerMappings = authGroupOwnerMappingMap()
			allModels = withDefaultMappedOwnerRows(modelRegistry, allModels, rows, ownerKeys, authByID, ownerMappings)
		}
	}

	allConfigRows := modelconfigsettings.ListAllConfigs()
	configByID := make(map[string]usage.ModelConfigRow, len(allConfigRows))
	for _, row := range allConfigRows {
		configByID[strings.ToLower(strings.TrimSpace(row.ModelID))] = row
	}
	data := make([]map[string]any, 0, len(allModels))
	activeMetadata := make([]map[string]any, 0, len(allModels))
	for _, model := range allModels {
		id, _ := model["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		entry := map[string]any{
			"id":     id,
			"object": "model",
			"source": "registry",
		}
		if ownedBy, exists := model["owned_by"]; exists {
			entry["owned_by"] = ownedBy
		}
		row, hasConfig := configByID[strings.ToLower(id)]
		modelOwnedByMappedOwner := hasConfig && row.Enabled && ownerKeys[normalizeModelOwnerKey(row.OwnedBy)]
		if sources := s.modelSourceEntries(modelRegistry, id, authByID, ownerMappings, ownerKeys, modelOwnedByMappedOwner); len(sources) > 0 {
			entry["sources"] = sources
		}
		if row, ok := configByID[strings.ToLower(id)]; ok {
			attachModelConfigCapabilities(entry, row)
			entry["pricing"] = map[string]any{
				"mode":                          row.PricingMode,
				"input_price_per_million":       row.InputPricePerMillion,
				"output_price_per_million":      row.OutputPricePerMillion,
				"cached_price_per_million":      row.CachedPricePerMillion,
				"cache_read_price_per_million":  row.CacheReadPricePerMillion,
				"cache_write_price_per_million": row.CacheWritePricePerMillion,
				"price_per_call":                row.PricePerCall,
			}
			if row.Description != "" {
				entry["description"] = row.Description
			}
			if row.Source != "" {
				entry["metadata_source"] = row.Source
			}
			if row.Enabled {
				activeMetadata = append(activeMetadata, map[string]any{
					"id":       row.ModelID,
					"owned_by": row.OwnedBy,
					"source":   row.Source,
					"enabled":  row.Enabled,
				})
			}
		}
		data = append(data, entry)
	}

	return map[string]any{
		"object":             "list",
		"scoped":             s.authManager != nil || usesMappedOwners,
		"data":               data,
		"active_metadata":    activeMetadata,
		"uses_mapped_owners": usesMappedOwners,
	}
}

func (s *Service) Models(allowedChannelsRaw, allowedGroupsRaw string) map[string]any {
	modelRegistry := registry.GetGlobalRegistry()
	allModels := s.effectiveModels(modelRegistry.GetAvailableModels("openai"), allowedChannelsRaw, allowedGroupsRaw)

	pricingMap := usage.GetAllModelPricing()
	filteredModels := make([]map[string]any, len(allModels))
	for i, model := range allModels {
		filteredModel := map[string]any{
			"id":     model["id"],
			"object": model["object"],
		}
		if created, exists := model["created"]; exists {
			filteredModel["created"] = created
		}
		if ownedBy, exists := model["owned_by"]; exists {
			filteredModel["owned_by"] = ownedBy
		}
		if modelID, ok := model["id"].(string); ok {
			if row, exists := modelconfigsettings.GetConfig(modelID); exists {
				attachModelConfigCapabilities(filteredModel, row)
			}
			if pricing, exists := pricingMap[modelID]; exists {
				filteredModel["pricing"] = map[string]any{
					"input_price_per_million":  pricing.InputPricePerMillion,
					"output_price_per_million": pricing.OutputPricePerMillion,
					"cached_price_per_million": pricing.CachedPricePerMillion,
				}
			}
		}
		filteredModels[i] = filteredModel
	}

	return map[string]any{
		"object": "list",
		"data":   filteredModels,
	}
}

func (s *Service) effectiveModels(models []map[string]any, allowedChannelsRaw, allowedGroupsRaw string) []map[string]any {
	filtered := s.filterModelsByScopes(models, allowedChannelsRaw, allowedGroupsRaw)
	return s.withScopedModelConfigRows(filtered, allowedChannelsRaw, allowedGroupsRaw)
}

func (s *Service) filterModelsByScopes(models []map[string]any, allowedChannelsRaw, allowedGroupsRaw string) []map[string]any {
	allowedChannelsRaw = strings.TrimSpace(allowedChannelsRaw)
	allowedGroups := internalrouting.ParseNormalizedSet(strings.TrimSpace(allowedGroupsRaw), internalrouting.NormalizeGroupName)
	if s == nil || s.authManager == nil {
		return models
	}
	if allowedChannelsRaw != "" && allowedChannelsRaw != "*" && !strings.EqualFold(allowedChannelsRaw, "all") {
		allowed := make(map[string]struct{})
		for _, part := range strings.Split(allowedChannelsRaw, ",") {
			key := strings.ToLower(strings.TrimSpace(part))
			if key == "" {
				continue
			}
			allowed[key] = struct{}{}
		}
		if len(allowed) == 0 {
			return models
		}
		filtered := make([]map[string]any, 0, len(models))
		for _, model := range models {
			id, _ := model["id"].(string)
			if id == "" {
				continue
			}
			if s.authManager.CanServeModelWithScopes(id, allowed, allowedGroups, "") {
				filtered = append(filtered, model)
			}
		}
		return filtered
	}
	if len(allowedGroups) == 0 {
		return models
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if id == "" {
			continue
		}
		if s.authManager.CanServeModelWithScopes(id, nil, allowedGroups, "") {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func (s *Service) withScopedModelConfigRows(models []map[string]any, allowedChannelsRaw, allowedGroupsRaw string) []map[string]any {
	rows := s.scopedModelConfigRows(allowedChannelsRaw, allowedGroupsRaw)
	if len(rows) == 0 {
		return models
	}
	out := make([]map[string]any, 0, len(models)+len(rows))
	seen := make(map[string]struct{}, len(models)+len(rows))
	for _, model := range models {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	for _, row := range rows {
		key := strings.ToLower(strings.TrimSpace(row.ModelID))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, modelConfigRowAsOpenAIModel(row))
	}
	return out
}

func (s *Service) scopedModelConfigRows(allowedChannelsRaw, allowedGroupsRaw string) []usage.ModelConfigRow {
	allowedGroups := internalrouting.ParseNormalizedSet(strings.TrimSpace(allowedGroupsRaw), internalrouting.NormalizeGroupName)
	allowedChannels := parseScopedChannelList(allowedChannelsRaw)
	if len(allowedGroups) == 0 && len(allowedChannels) == 0 {
		return nil
	}
	ownerKeys, explicitModels := s.modelOwnerScope(allowedChannels, allowedGroups)
	if len(ownerKeys) == 0 && len(explicitModels) == 0 {
		return nil
	}
	rows := modelconfigsettings.ListAllConfigs()
	out := make([]usage.ModelConfigRow, 0, len(rows))
	for _, row := range rows {
		modelID := strings.TrimSpace(row.ModelID)
		if modelID == "" || !row.Enabled {
			continue
		}
		if len(explicitModels) > 0 {
			if _, ok := explicitModels[strings.ToLower(modelID)]; !ok {
				continue
			}
			out = append(out, row)
			continue
		}
		if ownerKeys[normalizeModelOwnerKey(row.OwnedBy)] || ownerKeys[normalizeModelOwnerKey(row.Source)] {
			out = append(out, row)
		}
	}
	return out
}

func shouldUseDefaultMappedOwnerScope(allowedChannelsRaw, allowedGroupsRaw string) bool {
	return strings.TrimSpace(allowedChannelsRaw) == "" && strings.TrimSpace(allowedGroupsRaw) == ""
}

func (s *Service) defaultMappedOwnerRows() ([]usage.ModelConfigRow, map[string]bool, bool) {
	ownerKeys := s.defaultMappedOwnerKeys()
	if len(ownerKeys) == 0 {
		return nil, nil, false
	}
	rows := modelconfigsettings.ListAllConfigs()
	out := make([]usage.ModelConfigRow, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		if ownerKeys[normalizeModelOwnerKey(row.OwnedBy)] {
			out = append(out, row)
		}
	}
	return out, ownerKeys, true
}

func withDefaultMappedOwnerRows(
	modelRegistry *registry.ModelRegistry,
	models []map[string]any,
	rows []usage.ModelConfigRow,
	ownerKeys map[string]bool,
	authByID map[string]*coreauth.Auth,
	ownerMappings map[string]string,
) []map[string]any {
	out := make([]map[string]any, 0, len(models)+len(rows))
	seen := make(map[string]struct{}, len(models)+len(rows))
	for _, model := range models {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if registryModelCoveredByMappedOwners(modelRegistry, id, authByID, ownerMappings, ownerKeys) {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	for _, row := range rows {
		key := strings.ToLower(strings.TrimSpace(row.ModelID))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, modelConfigRowAsOpenAIModel(row))
	}
	return out
}

func registryModelCoveredByMappedOwners(
	modelRegistry *registry.ModelRegistry,
	modelID string,
	authByID map[string]*coreauth.Auth,
	ownerMappings map[string]string,
	ownerKeys map[string]bool,
) bool {
	if modelRegistry == nil || len(ownerMappings) == 0 || len(ownerKeys) == 0 {
		return false
	}
	sources := modelRegistry.GetModelClientSources(modelID)
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if sourceHasExplicitConfigModels(source, authByID) {
			return false
		}
		owner := mappedOwnerForSource(source, authByID, ownerMappings)
		if owner == "" || !ownerKeys[owner] {
			return false
		}
	}
	return true
}

func sourceCoveredByMappedOwners(
	source registry.ModelClientSource,
	authByID map[string]*coreauth.Auth,
	ownerMappings map[string]string,
	ownerKeys map[string]bool,
) bool {
	if len(ownerMappings) == 0 || len(ownerKeys) == 0 || sourceHasExplicitConfigModels(source, authByID) {
		return false
	}
	owner := mappedOwnerForSource(source, authByID, ownerMappings)
	return owner != "" && ownerKeys[owner]
}

func sourceHasExplicitConfigModels(source registry.ModelClientSource, authByID map[string]*coreauth.Auth) bool {
	auth := authByID[strings.TrimSpace(source.ClientID)]
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "apikey") {
		return false
	}
	if strings.TrimSpace(auth.Attributes["models_hash"]) == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth.Attributes["source"])), "config:")
}

func mappedOwnerForSource(source registry.ModelClientSource, authByID map[string]*coreauth.Auth, ownerMappings map[string]string) string {
	values := []string{source.Provider}
	if auth := authByID[strings.TrimSpace(source.ClientID)]; auth != nil {
		values = append(values, auth.Provider, auth.ChannelName())
		values = append(values, auth.ChannelIdentifiers()...)
	}
	for _, value := range values {
		if owner := ownerMappings[normalizeAuthGroupKey(value)]; owner != "" {
			return owner
		}
	}
	return ""
}

func (s *Service) defaultMappedOwnerKeys() map[string]bool {
	ownerMappings := authGroupOwnerMappingMap()
	if len(ownerMappings) == 0 || s == nil || s.authManager == nil {
		return nil
	}
	owners := make(map[string]bool)
	for _, auth := range s.authManager.List() {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		addMappedOwnerForAuthValue(owners, ownerMappings, auth.Provider)
		addMappedOwnerForAuthValue(owners, ownerMappings, auth.ChannelName())
		for _, identifier := range auth.ChannelIdentifiers() {
			addMappedOwnerForAuthValue(owners, ownerMappings, identifier)
		}
	}
	return owners
}

func addMappedOwnerForAuthValue(owners map[string]bool, ownerMappings map[string]string, value string) {
	if owner := ownerMappings[normalizeAuthGroupKey(value)]; owner != "" {
		owners[owner] = true
	}
}

func (s *Service) authByID() map[string]*coreauth.Auth {
	if s == nil || s.authManager == nil {
		return nil
	}
	auths := s.authManager.List()
	if len(auths) == 0 {
		return nil
	}
	out := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		out[auth.ID] = auth
	}
	return out
}

func (s *Service) modelSourceEntries(
	modelRegistry *registry.ModelRegistry,
	modelID string,
	authByID map[string]*coreauth.Auth,
	ownerMappings map[string]string,
	ownerKeys map[string]bool,
	modelOwnedByMappedOwner bool,
) []map[string]any {
	if modelRegistry == nil {
		return nil
	}
	rawSources := modelRegistry.GetModelClientSources(modelID)
	if len(rawSources) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(rawSources))
	seen := make(map[string]struct{}, len(rawSources))
	for _, raw := range rawSources {
		if !modelOwnedByMappedOwner && sourceCoveredByMappedOwners(raw, authByID, ownerMappings, ownerKeys) {
			continue
		}
		clientID := strings.TrimSpace(raw.ClientID)
		provider := strings.TrimSpace(raw.Provider)
		channel := ""
		source := ""
		if auth := authByID[clientID]; auth != nil {
			if provider == "" {
				provider = strings.TrimSpace(auth.Provider)
			}
			channel = strings.TrimSpace(auth.ChannelName())
			if auth.Attributes != nil {
				source = strings.TrimSpace(auth.Attributes["source"])
			}
		}

		label := provider
		if channel != "" {
			label = channel
			if provider != "" && !strings.EqualFold(provider, channel) {
				label = provider + " · " + channel
			}
		}
		if label == "" {
			label = clientID
		}
		upstreamModelID := strings.TrimSpace(raw.UpstreamModelID)
		key := provider + "\x00" + channel + "\x00" + label + "\x00" + source + "\x00" + clientID + "\x00" + upstreamModelID
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		entry := map[string]any{
			"label":     label,
			"provider":  provider,
			"client_id": clientID,
			"model_id":  strings.TrimSpace(raw.ModelID),
		}
		if upstreamModelID != "" {
			entry["upstream_model_id"] = upstreamModelID
		}
		if channel != "" {
			entry["channel"] = channel
		}
		if source != "" {
			entry["source"] = source
		}
		out = append(out, entry)
	}
	return out
}

func parseScopedChannelList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || value == "*" || strings.EqualFold(value, "all") {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, part := range strings.Split(value, ",") {
		channel := strings.TrimSpace(part)
		key := strings.ToLower(channel)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, channel)
	}
	return out
}

func (s *Service) modelOwnerScope(channels []string, groups map[string]struct{}) (map[string]bool, map[string]struct{}) {
	ownerMappings := authGroupOwnerMappingMap()
	providerOwners := modelConfigOwnersBySource()
	ownerKeys := make(map[string]bool)
	explicitModels := make(map[string]struct{})
	if s == nil {
		return ownerKeys, explicitModels
	}
	auths := []*coreauth.Auth(nil)
	if s.authManager != nil {
		auths = s.authManager.List()
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
	if s.cfg != nil {
		for _, group := range s.cfg.Routing.ChannelGroups {
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

func authGroupOwnerMappingMap() map[string]string {
	rows := modelconfigsettings.ListAuthGroupOwnerMappings()
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

func modelConfigOwnersBySource() map[string][]string {
	rows := modelconfigsettings.ListAllConfigs()
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

func modelConfigRowAsOpenAIModel(row usage.ModelConfigRow) map[string]any {
	entry := map[string]any{
		"id":          row.ModelID,
		"object":      "model",
		"owned_by":    row.OwnedBy,
		"source":      row.Source,
		"description": row.Description,
		"pricing": map[string]any{
			"mode":                          row.PricingMode,
			"input_price_per_million":       row.InputPricePerMillion,
			"output_price_per_million":      row.OutputPricePerMillion,
			"cached_price_per_million":      row.CachedPricePerMillion,
			"cache_read_price_per_million":  row.CacheReadPricePerMillion,
			"cache_write_price_per_million": row.CacheWritePricePerMillion,
			"price_per_call":                row.PricePerCall,
		},
	}
	attachModelConfigCapabilities(entry, row)
	return entry
}
