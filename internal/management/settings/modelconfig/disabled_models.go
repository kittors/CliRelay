package modelconfig

// The disabled-model surface: one definition of what an operator's "off" toggle
// means, and one cached read of which models carry it.
//
// Every other consumer of Enabled reads it additively ("should this row
// contribute a model?"), which made the toggle a no-op for models the static
// registry already provides. This file is the subtractive half.

import (
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"golang.org/x/sync/singleflight"
)

// NormalizeModelKey renders a model ID as used for the lookup keys below.
func NormalizeModelKey(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// IsDisabled reports whether a model falls inside a DisabledModelIDsForTenant set.
//
// The single definition of "switched off", shared by the listing filters and the
// request path. When they each kept a copy, only the request path handled
// prefixed aliases, so a disabled model stayed listed under its alias.
func IsDisabled(modelID string, disabled map[string]struct{}) bool {
	if len(disabled) == 0 {
		return false
	}
	key := NormalizeModelKey(modelID)
	if key == "" {
		return false
	}
	if _, off := disabled[key]; off {
		return true
	}
	// Prefixed wrappers ("ollama/gpt-oss:20b", "cline-pass/gpt-oss:20b") route to
	// the bare model, so disabling the bare model cascades to every wrapper.
	//
	// Deliberately one-way. Each wrapper is its own model_configs row with its
	// own toggle (see openRouterWrapperModelIDs), so disabling one wrapper must
	// switch off that wrapper only — never the bare model or its siblings.
	if idx := strings.Index(key, "/"); idx >= 0 {
		if bare := strings.TrimSpace(key[idx+1:]); bare != "" {
			if _, off := disabled[bare]; off {
				return true
			}
		}
	}
	return false
}

// DisabledModelIDs returns the disabled set for the system tenant.
func DisabledModelIDs() map[string]struct{} { return DisabledModelIDsForTenant("") }

// disabledModelCacheTTL bounds how long a replica may serve a stale set.
//
// Toggling a model on one replica cannot invalidate another's copy — there is no
// cross-process invalidation bus here — so the TTL is what bounds the divergence.
//
// The TTL is also the only thing that covers writers outside this package:
// usage/openrouter_model_sync.go writes model_configs through
// usage.UpsertModelConfigForTenant directly and never calls
// InvalidateDisabledModelCache. It only ever writes Enabled=true today, so the
// worst case is a re-enabled model staying refused for one TTL on this replica.
const disabledModelCacheTTL = 15 * time.Second

type disabledModelCacheEntry struct {
	ids       map[string]struct{}
	fetchedAt time.Time
}

var (
	disabledCacheMu sync.RWMutex
	disabledCache   = make(map[string]disabledModelCacheEntry)
	// disabledCacheGen is bumped by every invalidation. A load that started
	// before an invalidation must not store its (now stale) rows afterwards.
	disabledCacheGen uint64
	// disabledCacheLoads collapses concurrent refreshes of one tenant into a
	// single SELECT, so a TTL expiry under load does not fan out to the database.
	disabledCacheLoads singleflight.Group
)

// InvalidateDisabledModelCache drops this process's cached sets.
//
// Call it after a successful write, never before: invalidating first lets a
// concurrent reader refill the cache from the rows the write is about to change.
func InvalidateDisabledModelCache() {
	disabledCacheMu.Lock()
	disabledCache = make(map[string]disabledModelCacheEntry)
	disabledCacheGen++
	disabledCacheMu.Unlock()
}

// DisabledModelIDsForTenant returns the models the operator switched off, keyed
// by NormalizeModelKey.
//
// Cached per tenant so the proxy hot path does not run a SELECT per request.
// Treat the returned map as read-only; it is shared between callers.
func DisabledModelIDsForTenant(tenantID string) map[string]struct{} {
	tenantID = strings.TrimSpace(tenantID)

	disabledCacheMu.RLock()
	entry, ok := disabledCache[tenantID]
	disabledCacheMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < disabledModelCacheTTL {
		return entry.ids
	}

	loaded, _, _ := disabledCacheLoads.Do(tenantID, func() (any, error) {
		// A concurrent refresh may have landed while this call waited on the
		// singleflight; take it rather than issuing another SELECT.
		disabledCacheMu.RLock()
		fresh, ok := disabledCache[tenantID]
		gen := disabledCacheGen
		disabledCacheMu.RUnlock()
		if ok && time.Since(fresh.fetchedAt) < disabledModelCacheTTL {
			return fresh.ids, nil
		}

		// This runs every TTL for every tenant on every node, and reading whole
		// rows here was the database host's largest outbound flow (see
		// usage.ModelEnabledState). Most catalogs disable nothing (none did on
		// 2026-11-21), so ask first and list only when a disabled row exists; the
		// listing reads only model_id and enabled and merges like the full one.
		disabled := make(map[string]struct{})
		if has, err := usage.HasDisabledModelConfigsForTenant(tenantID); err != nil || has {
			for _, row := range usage.ListModelEnabledStatesForTenant(tenantID) {
				if row.Enabled {
					continue
				}
				if key := NormalizeModelKey(row.ModelID); key != "" {
					disabled[key] = struct{}{}
				}
			}
		}

		disabledCacheMu.Lock()
		// Only publish if nothing was invalidated while the SELECT ran; the
		// result is still correct for this caller, just not worth caching.
		if gen == disabledCacheGen {
			disabledCache[tenantID] = disabledModelCacheEntry{ids: disabled, fetchedAt: time.Now()}
		}
		disabledCacheMu.Unlock()
		return disabled, nil
	})
	return loaded.(map[string]struct{})
}

// IsModelDisabledForTenant reports whether one model is switched off.
func IsModelDisabledForTenant(tenantID, modelID string) bool {
	return IsDisabled(modelID, DisabledModelIDsForTenant(tenantID))
}

// FilterOutDisabled removes disabled models from a listing. Reads only "id" and
// never mutates the input.
func FilterOutDisabled(tenantID string, models []map[string]any) []map[string]any {
	if len(models) == 0 {
		return models
	}
	disabled := DisabledModelIDsForTenant(tenantID)
	if len(disabled) == 0 {
		return models
	}
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if IsDisabled(id, disabled) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// FilterOutDisabledIDs is FilterOutDisabled for a bare ID set.
func FilterOutDisabledIDs(tenantID string, ids map[string]struct{}) map[string]struct{} {
	if len(ids) == 0 {
		return ids
	}
	disabled := DisabledModelIDsForTenant(tenantID)
	if len(disabled) == 0 {
		return ids
	}
	out := make(map[string]struct{}, len(ids))
	for id := range ids {
		if IsDisabled(id, disabled) {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}
