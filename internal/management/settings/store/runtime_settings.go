package store

import (
	"context"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfigpersist"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

const (
	RuntimeSettingGeminiKeys           = runtimeconfig.RuntimeSettingGeminiKeys
	RuntimeSettingCodexKeys            = runtimeconfig.RuntimeSettingCodexKeys
	RuntimeSettingClaudeKeys           = runtimeconfig.RuntimeSettingClaudeKeys
	RuntimeSettingBedrockKeys          = runtimeconfig.RuntimeSettingBedrockKeys
	RuntimeSettingOpenCodeGoKeys       = runtimeconfig.RuntimeSettingOpenCodeGoKeys
	RuntimeSettingClineKeys            = runtimeconfig.RuntimeSettingClineKeys
	RuntimeSettingOllamaCloudKeys      = runtimeconfig.RuntimeSettingOllamaCloudKeys
	RuntimeSettingCommandCodeKeys      = runtimeconfig.RuntimeSettingCommandCodeKeys
	RuntimeSettingOpenAICompatibility  = runtimeconfig.RuntimeSettingOpenAICompatibility
	RuntimeSettingVertexCompatKeys     = runtimeconfig.RuntimeSettingVertexCompatKeys
	RuntimeSettingClaudeHeaderDefaults = runtimeconfig.RuntimeSettingClaudeHeaderDefaults
	RuntimeSettingKimiHeaderDefaults   = runtimeconfig.RuntimeSettingKimiHeaderDefaults
	RuntimeSettingIdentityFingerprint  = runtimeconfig.RuntimeSettingIdentityFingerprint
	RuntimeSettingCodexOAuthAdmission  = runtimeconfig.RuntimeSettingCodexOAuthAdmission
	RuntimeSettingOAuthExcludedModels  = runtimeconfig.RuntimeSettingOAuthExcludedModels
	RuntimeSettingOAuthModelAlias      = runtimeconfig.RuntimeSettingOAuthModelAlias
	RuntimeSettingPayload              = runtimeconfig.RuntimeSettingPayload
	RuntimeSettingImageSizePresets     = "image-generation-size-presets"
)

type ImageSizePresetsSetting struct {
	Sizes []string `json:"sizes"`
}

func PersistRuntimeSettingsPresentInYAML(cfg *config.Config, yamlContent []byte) {
	usage.PersistRuntimeSettingsPresentInYAML(cfg, yamlContent)
}

func MigrateRuntimeSettingsFromConfig(cfg *config.Config, configFilePath string) {
	usage.MigrateRuntimeSettingsFromConfig(cfg, configFilePath)
}

func ApplyStoredRuntimeSettings(cfg *config.Config) bool {
	return usage.ApplyStoredRuntimeSettings(cfg)
}

func UpsertRuntimeSetting(key string, value any) error {
	return usage.UpsertRuntimeSetting(key, value)
}

func GetRuntimeSettingPayload(key string) (json.RawMessage, bool) {
	return usage.GetRuntimeSettingPayload(key)
}

func LoadImageSizePresetsSetting() []string {
	return LoadImageSizePresetsSettingForTenant("")
}

func LoadImageSizePresetsSettingForTenant(tenantID string) []string {
	payload, ok := usage.GetRuntimeSettingPayloadForTenant(tenantID, RuntimeSettingImageSizePresets)
	if !ok {
		return nil
	}
	var body ImageSizePresetsSetting
	if err := json.Unmarshal(payload, &body); err != nil {
		var legacy []string
		if legacyErr := json.Unmarshal(payload, &legacy); legacyErr != nil {
			return nil
		}
		body.Sizes = legacy
	}
	return append([]string(nil), body.Sizes...)
}

func StoreImageSizePresetsSetting(sizes []string) error {
	return StoreImageSizePresetsSettingForTenant("", sizes)
}

func StoreImageSizePresetsSettingForTenant(tenantID string, sizes []string) error {
	return usage.UpsertRuntimeSettingForTenant(tenantID, RuntimeSettingImageSizePresets, ImageSizePresetsSetting{
		Sizes: append([]string(nil), sizes...),
	})
}

func SaveConfig(cfg *config.Config, configFilePath string) error {
	return runtimeconfigpersist.SaveConfig(cfg, configFilePath)
}

// CommitLiveConfig persists what the live system config changed; see
// runtimeconfigpersist.CommitLiveConfig.
func CommitLiveConfig(ctx context.Context, cfg *config.Config, configFilePath string, clientVersion *int64) ([]string, error) {
	return runtimeconfigpersist.CommitLiveConfig(ctx, cfg, configFilePath, clientVersion)
}

// CommitTenantConfig persists what a request changed on a fresh config copy.
func CommitTenantConfig(ctx context.Context, tenantID string, cfg *config.Config, clientVersion *int64, configFilePath string) ([]string, error) {
	return runtimeconfigpersist.CommitTenantConfig(ctx, tenantID, cfg, clientVersion, configFilePath)
}

// SaveNodeLocalConfig writes config.yaml for per-node settings.
func SaveNodeLocalConfig(cfg *config.Config, configFilePath string) error {
	return runtimeconfigpersist.SaveNodeLocalConfig(cfg, configFilePath)
}

// FreshConfig returns a copy of base with tenantID's settings freshly read from
// the database, for a management request to edit.
func FreshConfig(base *config.Config, tenantID string) *config.Config {
	return usage.FreshRuntimeConfig(base, tenantID)
}

// ReloadKeys re-reads keys of tenantID into cfg.
func ReloadKeys(tenantID string, cfg *config.Config, keys ...string) {
	usage.ReloadRuntimeSettingKeys(tenantID, cfg, keys...)
}

// AdoptKeys copies committed keys from a request copy into the live config.
func AdoptKeys(dst, src *config.Config, keys ...string) {
	usage.AdoptRuntimeSettingKeys(dst, src, keys...)
}

// ChangedKeys lists the settings cfg changed relative to what it was loaded with.
func ChangedKeys(cfg *config.Config) []string {
	return usage.ChangedRuntimeSettingKeys(cfg)
}

// StoreAvailable reports whether settings are stored in the database.
func StoreAvailable() bool {
	return usage.ConfigStoreAvailable()
}

// RuntimeSettingVersion returns the stored version of key, 0 when absent.
func RuntimeSettingVersion(tenantID, key string) int64 {
	return usage.RuntimeSettingVersion(tenantID, key)
}

func SyncReloadedConfigAfterYAMLSave(cfg *config.Config, configFilePath string, yamlContent []byte) {
	runtimeconfigpersist.SyncReloadedConfigAfterYAMLSave(cfg, configFilePath, yamlContent)
}
