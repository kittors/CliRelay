package runtimeconfigpersist

import (
	"context"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// SaveConfig persists the settings the live (system tenant) config changed.
// It writes only those keys, each against the version the config was loaded
// with, and leaves config.yaml alone: every setting the management API edits
// lives in the database. Without a database it writes config.yaml as before.
func SaveConfig(cfg *config.Config, configFilePath string) error {
	_, err := CommitLiveConfig(context.Background(), cfg, configFilePath, nil)
	return err
}

// CommitLiveConfig is SaveConfig with the version the client edited, if it
// sent one. On a conflict the changed keys are re-read into cfg, so the node
// neither keeps serving the rejected value nor re-submits it later.
func CommitLiveConfig(ctx context.Context, cfg *config.Config, configFilePath string, clientVersion *int64) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	if !usage.ConfigStoreAvailable() {
		return nil, config.SaveConfigPreserveComments(configFilePath, cfg)
	}
	keys, err := usage.CommitRuntimeSettings(ctx, "", cfg, clientVersion)
	if err != nil && errors.Is(err, configsync.ErrVersionConflict) {
		usage.ReloadRuntimeSettingKeys("", cfg, keys...)
	}
	return keys, err
}

// CommitTenantConfig persists what a request changed on cfg, a copy built by
// usage.FreshRuntimeConfig for tenantID. After a system write it also strips
// database-owned sections that are still in config.yaml, as the full save it
// replaces did; in steady state there are none and the file is not touched.
func CommitTenantConfig(ctx context.Context, tenantID string, cfg *config.Config, clientVersion *int64, configFilePath string) ([]string, error) {
	if cfg == nil || !usage.ConfigStoreAvailable() {
		return nil, nil
	}
	keys, err := usage.CommitRuntimeSettings(ctx, tenantID, cfg, clientVersion)
	if err == nil && configsync.NormalizeTenantID(tenantID) == configsync.NormalizeTenantID("") && configFilePath != "" {
		usage.CleanDBBackedConfigFromYAML(configFilePath)
	}
	return keys, err
}

// SaveNodeLocalConfig writes config.yaml for the settings that stay per node
// (see runtimeconfig for the list). Database-owned sections that the writer
// renders from memory are stripped again right after, as before.
func SaveNodeLocalConfig(cfg *config.Config, configFilePath string) error {
	if err := config.SaveConfigPreserveComments(configFilePath, cfg); err != nil {
		return err
	}
	if usage.ConfigStoreAvailable() {
		usage.CleanDBBackedConfigFromYAML(configFilePath)
	}
	return nil
}

func SyncReloadedConfigAfterYAMLSave(cfg *config.Config, configFilePath string, yamlContent []byte) {
	if cfg == nil || !usage.ConfigStoreAvailable() {
		return
	}
	usage.PersistRuntimeSettingsPresentInYAML(cfg, yamlContent)
	usage.MigrateRuntimeSettingsFromConfig(cfg, configFilePath)
	usage.ApplyStoredRuntimeSettings(cfg)
}
