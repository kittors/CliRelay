package usage

import (
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	sqlsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlite/settings"
)

// Compatibility bridge contract:
// - Owner: runtime settings / management settings boundary.
// - Real implementation: internal/management/settings/runtimeconfig + internal/storage/sqlite/settings.
// - Allowed callers: bootstrap, legacy reload flow, and narrow adapters that have not finished migrating.
// - Exit condition: callers move to runtimeconfig/sqlite settings packages directly; do not add new imports here.
const (
	RuntimeSettingGeminiKeys           = runtimeconfig.RuntimeSettingGeminiKeys
	RuntimeSettingCodexKeys            = runtimeconfig.RuntimeSettingCodexKeys
	RuntimeSettingClaudeKeys           = runtimeconfig.RuntimeSettingClaudeKeys
	RuntimeSettingBedrockKeys          = runtimeconfig.RuntimeSettingBedrockKeys
	RuntimeSettingOpenCodeGoKeys       = runtimeconfig.RuntimeSettingOpenCodeGoKeys
	RuntimeSettingOpenAICompatibility  = runtimeconfig.RuntimeSettingOpenAICompatibility
	RuntimeSettingVertexCompatKeys     = runtimeconfig.RuntimeSettingVertexCompatKeys
	RuntimeSettingClaudeHeaderDefaults = runtimeconfig.RuntimeSettingClaudeHeaderDefaults
	RuntimeSettingKimiHeaderDefaults   = runtimeconfig.RuntimeSettingKimiHeaderDefaults
	RuntimeSettingIdentityFingerprint  = runtimeconfig.RuntimeSettingIdentityFingerprint
	RuntimeSettingOAuthExcludedModels  = runtimeconfig.RuntimeSettingOAuthExcludedModels
	RuntimeSettingOAuthModelAlias      = runtimeconfig.RuntimeSettingOAuthModelAlias
	RuntimeSettingPayload              = runtimeconfig.RuntimeSettingPayload
)

func initRuntimeSettingsTable(db *sql.DB) {
	sqlsettings.InitRuntimeSettingsTable(db)
}

func runtimeSettingsStore() sqlsettings.RuntimeSettingsStore {
	return sqlsettings.NewRuntimeSettingsStore(getDB())
}

func UpsertRuntimeSetting(key string, value any) error {
	return runtimeSettingsStore().Upsert(key, value)
}

func GetRuntimeSettingPayload(key string) (json.RawMessage, bool) {
	if !ConfigStoreAvailable() {
		return nil, false
	}
	return runtimeSettingsStore().Payload(key)
}

func PersistRuntimeSettingsFromConfig(cfg *config.Config) int {
	if cfg == nil || !ConfigStoreAvailable() {
		return 0
	}
	return runtimeSettingsStore().PersistFromConfig(cfg)
}

// PersistRuntimeSettingsPresentInYAML stores DB-backed runtime settings that
// were explicitly included in a management config.yaml save.
func PersistRuntimeSettingsPresentInYAML(cfg *config.Config, yamlContent []byte) int {
	if cfg == nil || !ConfigStoreAvailable() {
		return 0
	}
	return runtimeSettingsStore().PersistPresentInYAML(cfg, yamlContent)
}

func ApplyStoredRuntimeSettings(cfg *config.Config) bool {
	if cfg == nil || !ConfigStoreAvailable() {
		return false
	}
	return runtimeSettingsStore().ApplyToConfig(cfg)
}

func MigrateRuntimeSettingsFromConfig(cfg *config.Config, configFilePath string) int {
	if cfg == nil || !ConfigStoreAvailable() {
		return 0
	}
	migrated, hadStored := runtimeSettingsStore().MigrateFromConfig(cfg)
	if strings.TrimSpace(configFilePath) == "" {
		return migrated
	}
	if migrated > 0 {
		if backupConfigForMigration(configFilePath, runtimeSettingsBackupSuffix) {
			cleanRuntimeSettingsFromYAML(configFilePath)
		}
		return migrated
	}
	if hadStored {
		cleanRuntimeSettingsFromYAML(configFilePath)
	}
	return migrated
}
