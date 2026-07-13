package modelcatalog

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// AvailabilityFilterOptions tunes management model listing for specific UI paths.
type AvailabilityFilterOptions struct {
	// IgnoreGroupAllowedModels skips channel-group AllowedModels filtering so the
	// channel-group editor can show every model the group's channels can serve.
	// Plaza/catalog leave this false so AllowedModels still control display.
	IgnoreGroupAllowedModels bool
}

func firstAvailabilityFilterOptions(opts []AvailabilityFilterOptions) AvailabilityFilterOptions {
	if len(opts) > 0 {
		return opts[0]
	}
	return AvailabilityFilterOptions{}
}

// Availability contract:
// - Owner: model availability query boundary.
// - Responsibility: turn registry state plus stored capabilities into management-facing availability DTOs.
//
// Claude/Codex/xAI live discovery is intentionally NOT RegisterClient-written
// into the runtime registry from management panels (subset risk, #673/#674).
// Management surfaces (plaza, catalog) still need the same live list as the
// auth-file models panel, so we merge the provider discovery cache here on
// every read (auto-warm on miss).
func (s *Service) ConfiguredAvailability(allowedChannelsRaw, allowedGroupsRaw string, opts ...AvailabilityFilterOptions) map[string]any {
	filterOpts := firstAvailabilityFilterOptions(opts)
	modelRegistry := registry.GetGlobalRegistry()
	authByID := s.authByID()
	discoveryByProvider := s.sharedDiscoveryByProvider(false)
	// Strip static-only claude/codex registry rows first, then scope-filter.
	// Live discovery models are appended AFTER CanServe filtering because they
	// are intentionally not RegisterClient-written into the runtime registry.
	ownerMappings := s.authGroupOwnerMappingMap()
	baseModels := dropStaticDiscoveryProviderModels(
		managementVisibleModels(modelRegistry),
		modelRegistry,
		discoveryByProvider,
		authByID,
		ownerMappings,
	)
	allModels := s.effectiveModels(baseModels, allowedChannelsRaw, allowedGroupsRaw, filterOpts)
	usesMappedOwners := false
	var ownerKeys map[string]bool
	if shouldUseDefaultMappedOwnerScope(allowedChannelsRaw, allowedGroupsRaw) {
		if rows, keys, configuredModelKeys, ok := s.defaultMappedOwnerRows(); ok {
			usesMappedOwners = true
			ownerKeys = keys
			allModels = withDefaultMappedOwnerRows(modelRegistry, allModels, rows, ownerKeys, configuredModelKeys, authByID, ownerMappings)
		}
	}
	// Mapped-owner config rows can re-introduce the full openai/anthropic library;
	// drop stale rows again, then append the live discovery list.
	allModels = dropStaticDiscoveryProviderModels(allModels, modelRegistry, discoveryByProvider, authByID, ownerMappings)
	// Live discovery models skip CanServe (not registry-backed). Still honor the
	// tenant channel-group allowed-models list so plaza/catalog match the editor,
	// unless the channel-group editor asks for the full channel-servable set.
	allModels = appendSharedDiscoveryModels(allModels, discoveryByProvider)
	if !filterOpts.IgnoreGroupAllowedModels {
		allModels = s.filterModelsByRoutingAllowedModels(allModels, allowedGroupsRaw)
	}

	allConfigRows := modelconfigsettings.ListAllConfigsForTenant(s.tenantID)
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
		if src, _ := model["source"].(string); strings.TrimSpace(src) != "" {
			entry["source"] = src
		}
		if ownedBy, exists := model["owned_by"]; exists {
			entry["owned_by"] = ownedBy
		}
		if sources := s.modelSourceEntries(modelRegistry, id, authByID, ownerMappings, ownerKeys); len(sources) > 0 {
			entry["sources"] = sources
		} else if discoveryProvider, _ := model["discovery_provider"].(string); strings.TrimSpace(discoveryProvider) != "" {
			// Discovery-only rows have no registry sources; synthesize from active auths.
			if sources := s.discoverySourceEntries(discoveryProvider, id, authByID); len(sources) > 0 {
				entry["sources"] = sources
			}
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

func (s *Service) Models(allowedChannelsRaw, allowedGroupsRaw string, opts ...AvailabilityFilterOptions) map[string]any {
	filterOpts := firstAvailabilityFilterOptions(opts)
	modelRegistry := registry.GetGlobalRegistry()
	authByID := s.authByID()
	discoveryByProvider := s.sharedDiscoveryByProvider(false)
	ownerMappings := s.authGroupOwnerMappingMap()
	baseModels := dropStaticDiscoveryProviderModels(
		managementVisibleModels(modelRegistry),
		modelRegistry,
		discoveryByProvider,
		authByID,
		ownerMappings,
	)
	allModels := s.effectiveModels(baseModels, allowedChannelsRaw, allowedGroupsRaw, filterOpts)
	allModels = dropStaticDiscoveryProviderModels(allModels, modelRegistry, discoveryByProvider, authByID, ownerMappings)
	allModels = appendSharedDiscoveryModels(allModels, discoveryByProvider)
	if !filterOpts.IgnoreGroupAllowedModels {
		allModels = s.filterModelsByRoutingAllowedModels(allModels, allowedGroupsRaw)
	}

	pricingMap := usage.GetAllModelPricingForTenant(s.tenantID)
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
			if row, exists := modelconfigsettings.GetConfigForTenant(s.tenantID, modelID); exists {
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

func managementVisibleModels(modelRegistry *registry.ModelRegistry) []map[string]any {
	if modelRegistry == nil {
		return nil
	}
	out := make([]map[string]any, 0)
	seen := make(map[string]struct{})
	add := func(model map[string]any) {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	for _, model := range modelRegistry.GetAvailableModels("openai") {
		add(model)
	}
	for _, provider := range []string{"opencode-go", "cline", "ollama-cloud"} {
		for _, info := range modelRegistry.GetAvailableModelsByProvider(provider) {
			add(registryModelInfoAsOpenAIModel(info))
		}
	}
	return out
}

// sharedDiscoveryByProvider auto-warms (or reads) the claude/codex provider
// discovery cache for the current tenant. force is reserved for future refresh
// query support; management list endpoints currently always use force=false.
func (s *Service) sharedDiscoveryByProvider(force bool) map[string][]*registry.ModelInfo {
	if s == nil || s.authManager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return managementauthfiles.EnsureSharedDiscoveryForTenant(ctx, s.authManager, s.cfg, s.tenantID, force)
}

func liveDiscoveryProviders(discoveryByProvider map[string][]*registry.ModelInfo) map[string]struct{} {
	out := make(map[string]struct{}, len(discoveryByProvider))
	for provider, list := range discoveryByProvider {
		if len(list) == 0 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(provider))] = struct{}{}
	}
	return out
}

func discoveryModelIDSet(discoveryByProvider map[string][]*registry.ModelInfo) map[string]struct{} {
	out := make(map[string]struct{})
	for _, list := range discoveryByProvider {
		for _, info := range list {
			if info == nil {
				continue
			}
			if id := strings.ToLower(strings.TrimSpace(info.ID)); id != "" {
				out[id] = struct{}{}
			}
		}
	}
	return out
}

// dropStaticDiscoveryProviderModels removes models that would otherwise keep the
// stale static Claude/Codex catalog visible after live discovery is warm:
//  1. Registry rows only served by live-discovery providers and not on the live list.
//  2. Owner-mapped library rows (openai/anthropic catalog) for owners that map
//     from those providers, when the model is not on the live list and has no
//     non-discovery runtime source (e.g. opencode-go).
//
// Models still served by other providers are kept.
func dropStaticDiscoveryProviderModels(
	models []map[string]any,
	modelRegistry *registry.ModelRegistry,
	discoveryByProvider map[string][]*registry.ModelInfo,
	authByID map[string]*coreauth.Auth,
	ownerMappings map[string]string,
) []map[string]any {
	liveProviders := liveDiscoveryProviders(discoveryByProvider)
	if len(liveProviders) == 0 {
		return models
	}
	discoveryIDs := discoveryModelIDSet(discoveryByProvider)
	discoveryOwners := ownersMappedFromProviders(ownerMappings, liveProviders)
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, onLive := discoveryIDs[key]; onLive {
			out = append(out, model)
			continue
		}
		if modelOnlyServedByDiscoveryProviders(modelRegistry, id, authByID, liveProviders) {
			continue
		}
		if modelIsStaleMappedOwnerLibraryRow(model, modelRegistry, id, authByID, liveProviders, discoveryOwners) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// ownersMappedFromProviders returns owner keys that map from any auth-group in liveProviders.
func ownersMappedFromProviders(ownerMappings map[string]string, liveProviders map[string]struct{}) map[string]bool {
	if len(ownerMappings) == 0 || len(liveProviders) == 0 {
		return nil
	}
	out := make(map[string]bool)
	for authGroup, owner := range ownerMappings {
		if _, ok := liveProviders[normalizeAuthGroupKey(authGroup)]; !ok {
			continue
		}
		if key := normalizeModelOwnerKey(owner); key != "" {
			out[key] = true
		}
	}
	return out
}

// modelIsStaleMappedOwnerLibraryRow drops owner-mapped catalog rows that only
// exist because of a codex→openai / claude→anthropic mapping, once live
// discovery has replaced that provider's callable set.
func modelIsStaleMappedOwnerLibraryRow(
	model map[string]any,
	modelRegistry *registry.ModelRegistry,
	modelID string,
	authByID map[string]*coreauth.Auth,
	liveProviders map[string]struct{},
	discoveryOwners map[string]bool,
) bool {
	if len(discoveryOwners) == 0 {
		return false
	}
	ownedBy, _ := model["owned_by"].(string)
	ownerKey := normalizeModelOwnerKey(ownedBy)
	if ownerKey == "" || !discoveryOwners[ownerKey] {
		return false
	}
	// If any non-discovery provider still serves this model, keep it.
	if modelRegistry != nil {
		sources := modelRegistry.GetModelClientSources(modelID)
		for _, source := range sources {
			provider := strings.ToLower(strings.TrimSpace(source.Provider))
			if auth := authByID[strings.TrimSpace(source.ClientID)]; auth != nil && strings.TrimSpace(auth.Provider) != "" {
				provider = strings.ToLower(strings.TrimSpace(auth.Provider))
			}
			if _, isLiveProvider := liveProviders[provider]; !isLiveProvider {
				return false
			}
		}
		// Has only discovery-provider sources (or none): stale library row for this owner.
		if len(sources) > 0 {
			return true
		}
	}
	// No registry sources: pure mapped-owner library row for a discovery-backed owner.
	return true
}

// appendSharedDiscoveryModels adds live discovery models that are not already
// present. Called AFTER tenant/channel scope filtering because discovery models
// are not RegisterClient-written into the runtime registry (so CanServe would
// drop them). Tenant ownership is already enforced by EnsureSharedDiscoveryForTenant
// which only warms providers with active auths in this tenant.
func appendSharedDiscoveryModels(
	models []map[string]any,
	discoveryByProvider map[string][]*registry.ModelInfo,
) []map[string]any {
	if len(discoveryByProvider) == 0 {
		return models
	}
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if key := strings.ToLower(strings.TrimSpace(id)); key != "" {
			seen[key] = struct{}{}
		}
	}
	out := models
	for provider, list := range discoveryByProvider {
		provider = strings.ToLower(strings.TrimSpace(provider))
		for _, info := range list {
			entry := registryModelInfoAsOpenAIModel(info)
			if entry == nil {
				continue
			}
			id, _ := entry["id"].(string)
			key := strings.ToLower(strings.TrimSpace(id))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			entry["source"] = "upstream-discovery"
			entry["discovery_provider"] = provider
			out = append(out, entry)
		}
	}
	return out
}

// modelOnlyServedByDiscoveryProviders reports whether every registry source for
// modelID belongs to a provider that has an active live discovery overlay.
// If so, the static catalog row should be replaced by the discovery list.
func modelOnlyServedByDiscoveryProviders(
	modelRegistry *registry.ModelRegistry,
	modelID string,
	authByID map[string]*coreauth.Auth,
	liveProviders map[string]struct{},
) bool {
	if modelRegistry == nil || len(liveProviders) == 0 {
		return false
	}
	sources := modelRegistry.GetModelClientSources(modelID)
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		provider := strings.ToLower(strings.TrimSpace(source.Provider))
		if auth := authByID[strings.TrimSpace(source.ClientID)]; auth != nil && strings.TrimSpace(auth.Provider) != "" {
			provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
		if _, ok := liveProviders[provider]; !ok {
			return false
		}
	}
	return true
}

// discoverySourceEntries builds synthetic source labels for discovery-only
// models so plaza cards can still show which codex/claude accounts serve them.
func (s *Service) discoverySourceEntries(
	provider string,
	modelID string,
	authByID map[string]*coreauth.Auth,
) []map[string]any {
	provider = strings.ToLower(strings.TrimSpace(provider))
	modelID = strings.TrimSpace(modelID)
	if provider == "" || modelID == "" {
		return nil
	}
	out := make([]map[string]any, 0)
	seen := make(map[string]struct{})
	for _, auth := range authByID {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		clientID := strings.TrimSpace(auth.ID)
		channel := strings.TrimSpace(auth.ChannelName())
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
		key := clientID + "\x00" + label
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		entry := map[string]any{
			"label":     label,
			"provider":  provider,
			"client_id": clientID,
			"model_id":  modelID,
		}
		if channel != "" {
			entry["channel"] = channel
		}
		if auth.Attributes != nil {
			if src := strings.TrimSpace(auth.Attributes["source"]); src != "" {
				entry["source"] = src
			}
		}
		out = append(out, entry)
	}
	return out
}

func registryModelInfoAsOpenAIModel(info *registry.ModelInfo) map[string]any {
	if info == nil {
		return nil
	}
	model := map[string]any{
		"id":       info.ID,
		"object":   "model",
		"owned_by": info.OwnedBy,
	}
	if info.Created > 0 {
		model["created"] = info.Created
	}
	if info.Type != "" {
		model["type"] = info.Type
	}
	if info.DisplayName != "" {
		model["display_name"] = info.DisplayName
	}
	if info.Version != "" {
		model["version"] = info.Version
	}
	if info.Description != "" {
		model["description"] = info.Description
	}
	if info.ContextLength > 0 {
		model["context_length"] = info.ContextLength
	}
	if info.MaxCompletionTokens > 0 {
		model["max_completion_tokens"] = info.MaxCompletionTokens
	}
	if len(info.SupportedParameters) > 0 {
		model["supported_parameters"] = info.SupportedParameters
	}
	return model
}

func (s *Service) effectiveModels(models []map[string]any, allowedChannelsRaw, allowedGroupsRaw string, opts AvailabilityFilterOptions) []map[string]any {
	filtered := s.filterModelsByScopes(models, allowedChannelsRaw, allowedGroupsRaw, opts)
	return s.withScopedModelConfigRows(filtered, allowedChannelsRaw, allowedGroupsRaw)
}

func (s *Service) filterModelsByScopes(models []map[string]any, allowedChannelsRaw, allowedGroupsRaw string, opts AvailabilityFilterOptions) []map[string]any {
	allowedChannelsRaw = strings.TrimSpace(allowedChannelsRaw)
	allowedGroups := internalrouting.ParseNormalizedSet(strings.TrimSpace(allowedGroupsRaw), internalrouting.NormalizeGroupName)
	if s == nil || s.authManager == nil {
		return models
	}

	// Always scope the global registry to models this tenant can actually serve.
	// Without this, non-system tenants inherit every system-tenant client model
	// whenever no channel/group restriction is present (the models page default).
	var allowedChannels map[string]struct{}
	if allowedChannelsRaw != "" && allowedChannelsRaw != "*" && !strings.EqualFold(allowedChannelsRaw, "all") {
		allowedChannels = make(map[string]struct{})
		for _, part := range strings.Split(allowedChannelsRaw, ",") {
			key := strings.ToLower(strings.TrimSpace(part))
			if key == "" {
				continue
			}
			allowedChannels[key] = struct{}{}
		}
		if len(allowedChannels) == 0 {
			allowedChannels = nil
		}
	}

	serveOpts := coreauth.ServeModelScopeOptions{
		IgnoreGroupAllowedModels: opts.IgnoreGroupAllowedModels,
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if id == "" {
			continue
		}
		if s.authManager.CanServeModelWithScopesForTenantOpts(s.tenantID, id, allowedChannels, allowedGroups, "", serveOpts) {
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
	rows := modelconfigsettings.ListAllConfigsForTenant(s.tenantID)
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

func (s *Service) defaultMappedOwnerRows() ([]usage.ModelConfigRow, map[string]bool, map[string]bool, bool) {
	ownerKeys := s.defaultMappedOwnerKeys()
	if len(ownerKeys) == 0 {
		return nil, nil, nil, false
	}
	rows := modelconfigsettings.ListAllConfigsForTenant(s.tenantID)
	out := make([]usage.ModelConfigRow, 0, len(rows))
	configuredModelKeys := make(map[string]bool, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		if key := strings.ToLower(strings.TrimSpace(row.ModelID)); key != "" {
			configuredModelKeys[key] = true
		}
		if ownerKeys[normalizeModelOwnerKey(row.OwnedBy)] {
			out = append(out, row)
		}
	}
	return out, ownerKeys, configuredModelKeys, true
}

func withDefaultMappedOwnerRows(
	modelRegistry *registry.ModelRegistry,
	models []map[string]any,
	rows []usage.ModelConfigRow,
	ownerKeys map[string]bool,
	configuredModelKeys map[string]bool,
	authByID map[string]*coreauth.Auth,
	ownerMappings map[string]string,
) []map[string]any {
	out := make([]map[string]any, 0, len(models)+len(rows))
	seen := make(map[string]struct{}, len(models)+len(rows))
	rowModelKeys := mappedOwnerRowModelKeys(rows, ownerKeys)
	for _, model := range models {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if registryModelCoveredByMappedOwners(modelRegistry, id, authByID, ownerMappings, ownerKeys, configuredModelKeys, rowModelKeys) {
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
		// DB-backed model catalog rows are management-authoritative. A newly
		// added owner-mapped model may not have a runtime registry source until a
		// provider advertises it, but the management UI still needs it in default
		// availability so system/default model lists and route editors can select it.
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, modelConfigRowAsOpenAIModel(row))
	}
	return out
}

func mappedOwnerRowModelKeys(rows []usage.ModelConfigRow, ownerKeys map[string]bool) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		key := strings.ToLower(strings.TrimSpace(row.ModelID))
		if key == "" || !row.Enabled || !ownerKeys[normalizeModelOwnerKey(row.OwnedBy)] {
			continue
		}
		out[key] = true
	}
	return out
}

func registryModelCoveredByMappedOwners(
	modelRegistry *registry.ModelRegistry,
	modelID string,
	authByID map[string]*coreauth.Auth,
	ownerMappings map[string]string,
	ownerKeys map[string]bool,
	configuredModelKeys map[string]bool,
	rowModelKeys map[string]bool,
) bool {
	if modelRegistry == nil || len(ownerMappings) == 0 || len(ownerKeys) == 0 {
		return false
	}
	key := strings.ToLower(strings.TrimSpace(modelID))
	if !configuredModelKeys[key] && !rowModelKeys[key] {
		if registryModelOnlyHasDynamicProviderSources(modelRegistry, modelID, authByID) {
			return false
		}
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

func registryModelOnlyHasDynamicProviderSources(modelRegistry *registry.ModelRegistry, modelID string, authByID map[string]*coreauth.Auth) bool {
	sources := modelRegistry.GetModelClientSources(modelID)
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		provider := strings.ToLower(strings.TrimSpace(source.Provider))
		if auth := authByID[strings.TrimSpace(source.ClientID)]; auth != nil && strings.TrimSpace(auth.Provider) != "" {
			provider = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
		// These config-backed providers publish their serviceable models from runtime model lists,
		// so a missing system model-config row must not hide the registry model.
		switch provider {
		case "opencode-go", "cline", "ollama-cloud":
		default:
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
	ownerMappings := s.authGroupOwnerMappingMap()
	if len(ownerMappings) == 0 || s == nil || s.authManager == nil {
		return nil
	}
	owners := make(map[string]bool)
	for _, auth := range s.authManager.ListForTenant(s.tenantID) {
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
	auths := s.authManager.ListForTenant(s.tenantID)
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
	hasExplicitConfigSource := false
	for _, raw := range rawSources {
		if sourceHasExplicitConfigModels(raw, authByID) {
			hasExplicitConfigSource = true
			break
		}
	}
	for _, raw := range rawSources {
		if hasExplicitConfigSource && sourceCoveredByMappedOwners(raw, authByID, ownerMappings, ownerKeys) {
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
	ownerMappings := s.authGroupOwnerMappingMap()
	providerOwners := s.modelConfigOwnersBySource()
	ownerKeys := make(map[string]bool)
	explicitModels := make(map[string]struct{})
	if s == nil {
		return ownerKeys, explicitModels
	}
	auths := []*coreauth.Auth(nil)
	if s.authManager != nil {
		auths = s.authManager.ListForTenant(s.tenantID)
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
