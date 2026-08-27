package authfiles

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

type ModelSource interface {
	GetModelsForClient(clientID string) []*registry.ModelInfo
}

type ModelRegistrar interface {
	RegisterClient(clientID, clientProvider string, models []*registry.ModelInfo)
}

// Provider-level discovery cache for claude/codex/xai (Grok)/kimi.
// Live manifests are shared across accounts of the same provider+tenant so we
// only hit upstream once (or on force refresh), then every auth-file models
// panel reuses the same list without RegisterClient-replacing the static catalog.
const discoveryCacheTTL = 24 * time.Hour

type discoveryCacheEntry struct {
	models    []*registry.ModelInfo
	fetchedAt time.Time
}

type discoveryInflight struct {
	done   chan struct{}
	models []*registry.ModelInfo
	ok     bool
}

var (
	discoveryCacheMu   sync.Mutex
	discoveryCache     = map[string]discoveryCacheEntry{}
	discoveryInflightM = map[string]*discoveryInflight{}
)

// ResetDiscoveryCacheForTest clears provider discovery cache (tests only).
func ResetDiscoveryCacheForTest() {
	discoveryCacheMu.Lock()
	discoveryCache = map[string]discoveryCacheEntry{}
	discoveryInflightM = map[string]*discoveryInflight{}
	discoveryCacheMu.Unlock()
}

// StoreDiscoveryCacheForTest seeds the provider discovery cache (tests only).
func StoreDiscoveryCacheForTest(tenantID, provider string, models []*registry.ModelInfo) {
	storeDiscoveryCache(tenantID, provider, models)
}

// normalizeDiscoveryProvider maps provider aliases onto a stable cache key.
// xAI accounts may appear as xai / x-ai / grok depending on auth metadata.
func normalizeDiscoveryProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "x-ai", "grok":
		return "xai"
	default:
		return provider
	}
}

func discoveryCacheKey(tenantID, provider string) string {
	return NormalizeTenantID(tenantID) + "|" + normalizeDiscoveryProvider(provider)
}

func supportsSharedDiscovery(provider string) bool {
	switch normalizeDiscoveryProvider(provider) {
	case "claude", "codex", "xai", "kimi":
		return true
	default:
		return false
	}
}

// SupportsSharedDiscovery reports whether provider uses the shared live-discovery cache.
func SupportsSharedDiscovery(provider string) bool {
	return supportsSharedDiscovery(provider)
}

func loadDiscoveryCache(tenantID, provider string) []*registry.ModelInfo {
	if !supportsSharedDiscovery(provider) {
		return nil
	}
	key := discoveryCacheKey(tenantID, provider)
	discoveryCacheMu.Lock()
	defer discoveryCacheMu.Unlock()
	entry, ok := discoveryCache[key]
	if !ok || len(entry.models) == 0 {
		return nil
	}
	if time.Since(entry.fetchedAt) > discoveryCacheTTL {
		delete(discoveryCache, key)
		return nil
	}
	return cloneRegistryModels(entry.models)
}

func storeDiscoveryCache(tenantID, provider string, models []*registry.ModelInfo) {
	if !supportsSharedDiscovery(provider) || len(models) == 0 {
		return
	}
	key := discoveryCacheKey(tenantID, provider)
	discoveryCacheMu.Lock()
	discoveryCache[key] = discoveryCacheEntry{
		models:    cloneRegistryModels(models),
		fetchedAt: time.Now(),
	}
	discoveryCacheMu.Unlock()
}

// EnsureProviderDiscovery returns the shared live model list for
// claude/codex/xai/kimi. Cache hit is preferred; on miss it warms once from the
// first active auth of that provider in the tenant (same single-flight path as
// the auth-file models panel). force re-fetches upstream even when the cache
// is warm.
func EnsureProviderDiscovery(
	ctx context.Context,
	manager *coreauth.Manager,
	cfg *config.Config,
	tenantID, provider string,
	force bool,
) []*registry.ModelInfo {
	provider = normalizeDiscoveryProvider(provider)
	if !supportsSharedDiscovery(provider) {
		return nil
	}
	if !force {
		if cached := loadDiscoveryCache(tenantID, provider); len(cached) > 0 {
			return cached
		}
	}
	auth := firstActiveAuthForProvider(manager, tenantID, provider)
	if auth == nil {
		return loadDiscoveryCache(tenantID, provider)
	}
	live, ok := warmSharedDiscovery(ctx, auth, cfg, tenantID, provider, force)
	if ok && len(live) > 0 {
		return live
	}
	return loadDiscoveryCache(tenantID, provider)
}

// EnsureSharedDiscoveryForTenant warms/returns discovery lists for every
// shared-discovery provider that has at least one active auth in the tenant.
// Used by model plaza / catalog so they show the same live list as the
// auth-file models panel (not the static registry catalog).
func EnsureSharedDiscoveryForTenant(
	ctx context.Context,
	manager *coreauth.Manager,
	cfg *config.Config,
	tenantID string,
	force bool,
) map[string][]*registry.ModelInfo {
	out := make(map[string][]*registry.ModelInfo)
	if manager == nil {
		return out
	}
	seen := make(map[string]struct{})
	for _, auth := range manager.ListForTenant(NormalizeTenantID(tenantID)) {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		provider := normalizeDiscoveryProvider(auth.Provider)
		if !supportsSharedDiscovery(provider) {
			continue
		}
		if _, ok := seen[provider]; ok {
			continue
		}
		seen[provider] = struct{}{}
		models := EnsureProviderDiscovery(ctx, manager, cfg, tenantID, provider, force)
		if len(models) > 0 {
			out[provider] = models
		}
	}
	return out
}

func firstActiveAuthForProvider(manager *coreauth.Manager, tenantID, provider string) *coreauth.Auth {
	if manager == nil {
		return nil
	}
	provider = normalizeDiscoveryProvider(provider)
	for _, auth := range manager.ListForTenant(NormalizeTenantID(tenantID)) {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if normalizeDiscoveryProvider(auth.Provider) == provider {
			return auth
		}
	}
	return nil
}

func ModelLookupAuthID(manager *coreauth.Manager, name string) string {
	return ModelLookupAuthIDForTenant(manager, "", name)
}

func ModelLookupAuthIDForTenant(manager *coreauth.Manager, tenantID, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if manager != nil {
		for _, auth := range manager.ListForTenant(NormalizeTenantID(tenantID)) {
			if auth == nil {
				continue
			}
			if auth.FileName == name || auth.ID == name {
				return auth.ID
			}
		}
	}
	return name
}

// FindAuthForTenant resolves an auth by file name or ID within a tenant.
func FindAuthForTenant(manager *coreauth.Manager, tenantID, name string) *coreauth.Auth {
	name = strings.TrimSpace(name)
	if name == "" || manager == nil {
		return nil
	}
	for _, auth := range manager.ListForTenant(NormalizeTenantID(tenantID)) {
		if auth == nil {
			continue
		}
		if auth.FileName == name || auth.ID == name {
			return auth
		}
	}
	return nil
}

func ListModelEntries(manager *coreauth.Manager, source ModelSource, name string) []map[string]any {
	return ListModelEntriesForTenant(manager, source, "", name)
}

func ListModelEntriesForTenant(manager *coreauth.Manager, source ModelSource, tenantID, name string) []map[string]any {
	if source == nil {
		return nil
	}
	authID := ModelLookupAuthIDForTenant(manager, tenantID, name)
	models := source.GetModelsForClient(authID)
	return modelEntriesFromRegistry(models)
}

// ListModelEntriesLiveForTenant returns models for an auth file panel.
//
// Behaviour:
//   - claude / codex / xai / kimi (shared discovery):
//     open (refresh=false): serve provider discovery cache if present; otherwise
//     auto-warm once from upstream using this auth, store under provider+tenant,
//     return source=upstream. Never RegisterClient-replace the static catalog.
//     force (refresh=true): re-fetch upstream, refresh provider cache, return
//     source=upstream. Same-type accounts reuse the cache without re-hitting
//     upstream until TTL or the next force.
//   - antigravity:
//     refresh=true updates runtime registry when live succeeds; open uses registry.
//
// When live fetch fails, falls back to the existing registry list so the UI
// still shows known models.
func ListModelEntriesLiveForTenant(
	ctx context.Context,
	manager *coreauth.Manager,
	source ModelSource,
	registrar ModelRegistrar,
	cfg *config.Config,
	tenantID, name string,
	refresh bool,
) (models []map[string]any, sourceLabel string) {
	sourceLabel = "registry"

	auth := FindAuthForTenant(manager, tenantID, name)
	if auth == nil {
		return ListModelEntriesForTenant(manager, source, tenantID, name), sourceLabel
	}
	provider := normalizeDiscoveryProvider(auth.Provider)

	// Shared discovery path for Claude / Codex / xAI (Grok) / Kimi: prefer
	// provider cache on open, auto-warm on first miss, force re-fetch only when
	// refresh=1.
	if supportsSharedDiscovery(provider) {
		if !refresh {
			if cached := loadDiscoveryCache(tenantID, provider); len(cached) > 0 {
				return modelEntriesFromRegistry(mergeDiscoveryWithStaticCatalog(provider, cached)), "upstream"
			}
		}
		live, ok := warmSharedDiscovery(ctx, auth, cfg, tenantID, provider, refresh)
		if ok && len(live) > 0 {
			return modelEntriesFromRegistry(mergeDiscoveryWithStaticCatalog(provider, live)), "upstream"
		}
		// Live miss/fail: keep last good discovery list if any (do not snap back to static).
		if cached := loadDiscoveryCache(tenantID, provider); len(cached) > 0 {
			return modelEntriesFromRegistry(mergeDiscoveryWithStaticCatalog(provider, cached)), "upstream"
		}
		return ListModelEntriesForTenant(manager, source, tenantID, name), sourceLabel
	}

	if !refresh {
		return ListModelEntriesForTenant(manager, source, tenantID, name), sourceLabel
	}

	live, liveProvider, updateRegistry := fetchLiveModelsForAuth(ctx, auth, cfg)
	if len(live) == 0 {
		return ListModelEntriesForTenant(manager, source, tenantID, name), sourceLabel
	}

	// Discovery for these providers is a subset of what the runtime registers, so
	// it supplements the static catalog instead of replacing the panel's view.
	live = mergeDiscoveryWithStaticCatalog(liveProvider, live)

	sourceLabel = "upstream"
	if updateRegistry && registrar != nil {
		providerKey := liveProvider
		if providerKey == "" {
			providerKey = provider
		}
		registrar.RegisterClient(auth.ID, providerKey, live)
	}
	return modelEntriesFromRegistry(live), sourceLabel
}

// warmSharedDiscovery fetches live models for claude/codex/xai/kimi and stores them
// in the provider-level cache. Concurrent warmers for the same key single-flight.
// When force is false and another warmer already populated the cache, waiters
// receive that result without a second upstream call.
func warmSharedDiscovery(
	ctx context.Context,
	auth *coreauth.Auth,
	cfg *config.Config,
	tenantID, provider string,
	force bool,
) ([]*registry.ModelInfo, bool) {
	provider = normalizeDiscoveryProvider(provider)
	if auth == nil || !supportsSharedDiscovery(provider) {
		return nil, false
	}
	key := discoveryCacheKey(tenantID, provider)

	if !force {
		if cached := loadDiscoveryCache(tenantID, provider); len(cached) > 0 {
			return cached, true
		}
	}

	discoveryCacheMu.Lock()
	if !force {
		if entry, ok := discoveryCache[key]; ok && len(entry.models) > 0 && time.Since(entry.fetchedAt) <= discoveryCacheTTL {
			models := cloneRegistryModels(entry.models)
			discoveryCacheMu.Unlock()
			return models, true
		}
	}
	if inflight, ok := discoveryInflightM[key]; ok {
		discoveryCacheMu.Unlock()
		<-inflight.done
		if inflight.ok {
			return cloneRegistryModels(inflight.models), true
		}
		// Leader failed; if we are not force, do not stampede — fall back.
		if !force {
			return nil, false
		}
		// Force path: try again as a new leader below.
		discoveryCacheMu.Lock()
		if inflight2, ok2 := discoveryInflightM[key]; ok2 {
			discoveryCacheMu.Unlock()
			<-inflight2.done
			if inflight2.ok {
				return cloneRegistryModels(inflight2.models), true
			}
			return nil, false
		}
	}

	inflight := &discoveryInflight{done: make(chan struct{})}
	discoveryInflightM[key] = inflight
	discoveryCacheMu.Unlock()

	live, _, _ := fetchLiveModelsForAuth(ctx, auth, cfg)
	ok := len(live) > 0
	if ok {
		storeDiscoveryCache(tenantID, provider, live)
	}

	discoveryCacheMu.Lock()
	inflight.models = cloneRegistryModels(live)
	inflight.ok = ok
	delete(discoveryInflightM, key)
	close(inflight.done)
	discoveryCacheMu.Unlock()

	if !ok {
		return nil, false
	}
	return cloneRegistryModels(live), true
}

func fetchLiveModelsForAuth(ctx context.Context, auth *coreauth.Auth, cfg *config.Config) ([]*registry.ModelInfo, string, bool) {
	if auth == nil {
		return nil, "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	provider := normalizeDiscoveryProvider(auth.Provider)
	// Preserve raw provider for non-shared fetch paths (antigravity, etc.).
	rawProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
	var sdkModels []*sdkmodelcatalog.ModelInfo
	updateRegistry := false
	switch {
	case provider == "claude":
		// Discovery only — do not replace static registry.
		sdkModels = executor.FetchClaudeModels(fetchCtx, auth, cfg)
	case provider == "codex":
		// Discovery only — ChatGPT manifest is gated by client_version and is not a full catalog.
		sdkModels = executor.FetchCodexModels(fetchCtx, auth, cfg)
	case provider == "xai":
		// Discovery only on the shared path (same as claude/codex). Startup still
		// registers live xAI models via service_model_registration; management
		// panels must not RegisterClient-replace per auth-file refresh.
		sdkModels = executor.FetchXAIModels(fetchCtx, auth, cfg)
	case provider == "kimi":
		// Discovery only, same as xAI: startup registration owns the runtime
		// registry, the panel just mirrors what the coding gateway advertises.
		sdkModels = executor.FetchKimiModels(fetchCtx, auth, cfg)
	case rawProvider == "antigravity":
		sdkModels = executor.FetchAntigravityModels(fetchCtx, auth, cfg)
		updateRegistry = true
		provider = rawProvider
	default:
		return nil, rawProvider, false
	}
	return cloneSDKModelsToRegistry(sdkModels), provider, updateRegistry
}

func cloneSDKModelsToRegistry(models []*sdkmodelcatalog.ModelInfo) []*registry.ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		out = append(out, &registry.ModelInfo{
			ID:                  model.ID,
			Object:              model.Object,
			Created:             model.Created,
			OwnedBy:             model.OwnedBy,
			Type:                model.Type,
			DisplayName:         model.DisplayName,
			UpstreamModelID:     model.UpstreamModelID,
			Name:                model.Name,
			Version:             model.Version,
			Description:         model.Description,
			InputTokenLimit:     model.InputTokenLimit,
			OutputTokenLimit:    model.OutputTokenLimit,
			ContextLength:       model.ContextLength,
			MaxCompletionTokens: model.MaxCompletionTokens,
			UserDefined:         model.UserDefined,
		})
	}
	return out
}

func cloneRegistryModels(models []*registry.ModelInfo) []*registry.ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		clone := *model
		out = append(out, &clone)
	}
	return out
}

func modelEntriesFromRegistry(models []*registry.ModelInfo) []map[string]any {
	result := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		entry := map[string]any{
			"id": model.ID,
		}
		if model.DisplayName != "" {
			entry["display_name"] = model.DisplayName
		}
		if model.Type != "" {
			entry["type"] = model.Type
		}
		if model.OwnedBy != "" {
			entry["owned_by"] = model.OwnedBy
		}
		result = append(result, entry)
	}
	return result
}
