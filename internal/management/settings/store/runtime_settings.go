package store

import (
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

func PersistRuntimeSettingsFromConfig(cfg *config.Config) {
	usage.PersistRuntimeSettingsFromConfig(cfg)
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
	payload, ok := GetRuntimeSettingPayload(RuntimeSettingImageSizePresets)
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
	return UpsertRuntimeSetting(RuntimeSettingImageSizePresets, ImageSizePresetsSetting{
		Sizes: append([]string(nil), sizes...),
	})
}

func SaveConfig(cfg *config.Config, configFilePath string) error {
	return runtimeconfigpersist.SaveConfig(cfg, configFilePath)
}

func PersistRuntimeSetting(cfg *config.Config, configFilePath string, key string, value any) error {
	return runtimeconfigpersist.PersistRuntimeSetting(cfg, configFilePath, key, value)
}

func SyncReloadedConfigAfterYAMLSave(cfg *config.Config, configFilePath string, yamlContent []byte) {
	runtimeconfigpersist.SyncReloadedConfigAfterYAMLSave(cfg, configFilePath, yamlContent)
}
