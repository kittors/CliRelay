package usage

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	sqlsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/settings"
)

// FreshRuntimeConfig returns a copy of base whose runtime settings were just
// read from tenantID's rows, carrying its own state. Management requests edit
// this copy instead of the live config, so a request always starts from the
// stored values even when this node has not yet heard of another node's
// change, and the live config is only touched once the write succeeded.
func FreshRuntimeConfig(base *config.Config, tenantID string) *config.Config {
	tenantID = normalizeTenantID(tenantID)
	if tenantID != systemTenantID {
		built := BuildTenantRuntimeConfig(base, tenantID)
		return &built
	}
	var fresh config.Config
	if base != nil {
		fresh = *base
	}
	fresh.DetachRuntimeSettingState()
	// Re-decode every setting so the copy shares no slices or maps with the
	// live config: handlers edit entries in place.
	for _, spec := range runtimeconfig.Specs() {
		if raw, err := json.Marshal(spec.Value(&fresh)); err == nil {
			spec.Apply(&fresh, raw)
		}
	}
	if ConfigStoreAvailable() {
		applyStoredRuntimeSettings(runtimeSettingsStore(), &fresh)
	} else {
		sqlsettings.Rebaseline(&fresh)
	}
	return &fresh
}

// CommitRuntimeSettings writes the settings cfg changed relative to what it
// was loaded with, for tenantID; see sqlsettings.CommitChanges.
func CommitRuntimeSettings(ctx context.Context, tenantID string, cfg *config.Config, clientVersion *int64) ([]string, error) {
	return sqlsettings.CommitChanges(ctx, runtimeSettingsStoreForTenant(tenantID), cfg, clientVersion)
}

// ReloadRuntimeSettingKeys re-reads keys of tenantID into cfg and its state.
func ReloadRuntimeSettingKeys(tenantID string, cfg *config.Config, keys ...string) {
	if cfg == nil || !ConfigStoreAvailable() {
		return
	}
	sqlsettings.ReloadKeys(runtimeSettingsStoreForTenant(tenantID), cfg, keys...)
}

// RuntimeSettingVersion returns the stored version of key, 0 when absent.
func RuntimeSettingVersion(tenantID, key string) int64 {
	if !ConfigStoreAvailable() {
		return 0
	}
	entry, ok := runtimeSettingsStoreForTenant(tenantID).Load(key)
	if !ok {
		return 0
	}
	return entry.Version
}

// CompareAndSwapRuntimeSetting writes one key of tenantID if it is still at
// expected (0: absent, AnyVersion: unchecked) and returns the new version.
func CompareAndSwapRuntimeSetting(ctx context.Context, tenantID, key string, value any, expected int64) (int64, error) {
	versions, err := runtimeSettingsStoreForTenant(tenantID).CompareAndSwap(ctx, []sqlsettings.Change{{Key: key, Value: value, Expected: expected}})
	if err != nil {
		return 0, err
	}
	return versions[key], nil
}

// AdoptRuntimeSettingKeys copies committed keys from src into the live dst.
func AdoptRuntimeSettingKeys(dst, src *config.Config, keys ...string) {
	sqlsettings.AdoptKeys(dst, src, keys...)
}

// UpdateRuntimeSetting applies a derived change to one stored setting of
// tenantID: mutate edits a copy holding the stored value and reports whether
// it changed anything. The write is checked against the version it was read
// at and retried on conflict, because a derived change (renaming a channel
// everywhere it appears) is valid on top of any concurrent edit. It returns
// the copy holding the stored result.
func UpdateRuntimeSetting(ctx context.Context, base *config.Config, tenantID, key string, mutate func(*config.Config) bool) (*config.Config, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		fresh := FreshRuntimeConfig(base, tenantID)
		if !mutate(fresh) {
			return fresh, false, nil
		}
		if _, err := CommitRuntimeSettings(ctx, tenantID, fresh, nil); err != nil {
			lastErr = err
			if errors.Is(err, configsync.ErrVersionConflict) {
				continue
			}
			return fresh, false, err
		}
		return fresh, true, nil
	}
	return nil, false, lastErr
}

// ChangedRuntimeSettingKeys lists the settings cfg changed relative to what it
// was loaded with.
func ChangedRuntimeSettingKeys(cfg *config.Config) []string {
	pending := sqlsettings.DiffAgainstState(cfg, cfg.RuntimeSettingState())
	keys := make([]string, 0, len(pending))
	for _, p := range pending {
		keys = append(keys, p.Spec.Key)
	}
	return keys
}
