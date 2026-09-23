package authfiles

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/modeldiscovery"
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

// Provider-level discovery for claude/codex/xai (Grok)/kimi lives in
// internal/modeldiscovery, where credential registration reads the same lists. The
// helpers below adapt it to the registry shape the panels render.

// ResetDiscoveryCacheForTest clears provider discovery cache (tests only).
func ResetDiscoveryCacheForTest() {
	modeldiscovery.ResetForTest()
}

// StoreDiscoveryCacheForTest seeds the provider discovery cache (tests only).
func StoreDiscoveryCacheForTest(tenantID, provider string, models []*registry.ModelInfo) {
	storeDiscoveryCache(tenantID, provider, models)
}

func normalizeDiscoveryProvider(provider string) string {
	return modeldiscovery.NormalizeProvider(provider)
}

func supportsSharedDiscovery(provider string) bool {
	return modeldiscovery.IsShared(provider)
}

// SupportsSharedDiscovery reports whether provider uses the shared live-discovery cache.
func SupportsSharedDiscovery(provider string) bool {
	return supportsSharedDiscovery(provider)
}

func loadDiscoveryCache(tenantID, provider string) []*registry.ModelInfo {
	return registry.ModelInfosFromSDK(modeldiscovery.Fresh(NormalizeTenantID(tenantID), provider))
}

// loadDiscoverySnapshot returns the last list stored however old it is, for when a
// fetch has just failed: a stale upstream list is still closer to the truth than
// the compiled-in catalog.
func loadDiscoverySnapshot(tenantID, provider string) []*registry.ModelInfo {
	return registry.ModelInfosFromSDK(modeldiscovery.Snapshot(NormalizeTenantID(tenantID), provider))
}

func storeDiscoveryCache(tenantID, provider string, models []*registry.ModelInfo) {
	modeldiscovery.Store(NormalizeTenantID(tenantID), provider, registry.ModelInfosToSDK(models))
}

// discoveryContext detaches a panel request from the fetch it triggers. The result
// is shared by every account of the tenant and by routing, so closing the page must
// not throw it away half-way.
func discoveryContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

// EnsureProviderDiscovery returns the shared live model list for
// claude/codex/xai/kimi. Cache hit is preferred; on miss it warms once from the
// first eligible auth of that provider in the tenant (same single-flight path as
// the auth-file models panel). force re-fetches upstream even when the cache
// is warm.
func EnsureProviderDiscovery(
	ctx context.Context,
	manager *coreauth.Manager,
	cfg *config.Config,
	tenantID, provider string,
	force bool,
) []*registry.ModelInfo {
	return registry.ModelInfosFromSDK(
		modeldiscovery.Ensure(discoveryContext(ctx), manager, cfg, NormalizeTenantID(tenantID), provider, force),
	)
}

// EnsureSharedDiscoveryForTenant warms/returns discovery lists for every
// shared-discovery provider that has at least one eligible auth in the tenant.
// Used by model plaza / catalog so they show the same live list as the
// auth-file models panel (not the static registry catalog).
func EnsureSharedDiscoveryForTenant(
	ctx context.Context,
	manager *coreauth.Manager,
	cfg *config.Config,
	tenantID string,
	force bool,
) map[string][]*registry.ModelInfo {
	lists := modeldiscovery.EnsureForTenant(discoveryContext(ctx), manager, cfg, NormalizeTenantID(tenantID), force)
	out := make(map[string][]*registry.ModelInfo, len(lists))
	for provider, models := range lists {
		if converted := registry.ModelInfosFromSDK(models); len(converted) > 0 {
			out[provider] = converted
		}
	}
	return out
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
	// refresh=1. Codex and Claude API keys point at their own relays and take the
	// per-account path below instead (see modeldiscovery.IsDiscoveryCredential).
	if supportsSharedDiscovery(provider) && modeldiscovery.IsDiscoveryCredential(auth, provider) {
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
		if cached := loadDiscoverySnapshot(tenantID, provider); len(cached) > 0 {
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

// warmSharedDiscovery fetches the shared list with this auth and stores it; see
// modeldiscovery.Warm for the single-flight rules.
func warmSharedDiscovery(
	ctx context.Context,
	auth *coreauth.Auth,
	cfg *config.Config,
	tenantID, provider string,
	force bool,
) ([]*registry.ModelInfo, bool) {
	live, ok := modeldiscovery.Warm(discoveryContext(ctx), auth, cfg, NormalizeTenantID(tenantID), provider, force)
	if !ok {
		return nil, false
	}
	converted := registry.ModelInfosFromSDK(live)
	return converted, len(converted) > 0
}

// fetchLiveModelsForAuth serves the per-account refresh path: providers without a
// shared list, and Codex / Claude API keys whose relay has a catalog of its own.
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
		// Display only: registration owns the runtime registry.
		sdkModels = executor.FetchClaudeModels(fetchCtx, auth, cfg)
	case provider == "codex":
		// Display only: registration owns the runtime registry.
		sdkModels = executor.FetchCodexModels(fetchCtx, auth, cfg)
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
	return registry.ModelInfosFromSDK(models)
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
