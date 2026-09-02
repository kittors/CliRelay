package usage

import (
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	sqlsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/settings"
)

// Compatibility bridge contract:
// - Owner: runtime settings / management settings boundary.
// - Real implementation: internal/management/settings/runtimeconfig + internal/storage/sqlstore/settings.
// - Allowed callers: bootstrap, legacy reload flow, and narrow adapters that have not finished migrating.
// - Exit condition: callers move to runtimeconfig/sqlstore settings packages directly; do not add new imports here.
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
)

func initRuntimeSettingsTable(db *sql.DB) {
	sqlsettings.InitRuntimeSettingsTable(db)
}

func runtimeSettingsStore() sqlsettings.RuntimeSettingsStore {
	return sqlsettings.NewRuntimeSettingsStore(getDB())
}

func runtimeSettingsStoreForTenant(tenantID string) sqlsettings.RuntimeSettingsStore {
	return sqlsettings.NewTenantRuntimeSettingsStore(getDB(), tenantID)
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
	store := runtimeSettingsStore()
	applied := store.ApplyToConfig(cfg)
	if cfg.EnsureProviderStableIDs() {
		persistProviderStableIDBackfill(store, cfg)
		return true
	}
	return applied
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

func UpsertRuntimeSettingForTenant(tenantID, key string, value any) error {
	return runtimeSettingsStoreForTenant(tenantID).Upsert(key, value)
}
func GetRuntimeSettingPayloadForTenant(tenantID, key string) (json.RawMessage, bool) {
	if !ConfigStoreAvailable() {
		return nil, false
	}
	return runtimeSettingsStoreForTenant(tenantID).Payload(key)
}
func ApplyStoredRuntimeSettingsForTenant(tenantID string, cfg *config.Config) bool {
	if cfg == nil || !ConfigStoreAvailable() {
		return false
	}
	store := runtimeSettingsStoreForTenant(tenantID)
	applied := store.ApplyToConfig(cfg)
	if cfg.EnsureProviderStableIDs() {
		persistProviderStableIDBackfill(store, cfg)
		return true
	}
	return applied
}

func persistProviderStableIDBackfill(store sqlsettings.RuntimeSettingsStore, cfg *config.Config) {
	settings := []struct {
		key   string
		value any
	}{
		{RuntimeSettingGeminiKeys, cfg.GeminiKey},
		{RuntimeSettingCodexKeys, cfg.CodexKey},
		{RuntimeSettingClaudeKeys, cfg.ClaudeKey},
		{RuntimeSettingBedrockKeys, cfg.BedrockKey},
		{RuntimeSettingOpenCodeGoKeys, cfg.OpenCodeGoKey},
		{RuntimeSettingClineKeys, cfg.ClineKey},
		{RuntimeSettingOllamaCloudKeys, cfg.OllamaCloudKey},
		{RuntimeSettingCommandCodeKeys, cfg.CommandCodeKey},
		{RuntimeSettingOpenAICompatibility, cfg.OpenAICompatibility},
		{RuntimeSettingVertexCompatKeys, cfg.VertexCompatAPIKey},
	}
	for _, setting := range settings {
		if !store.Exists(setting.key) {
			continue
		}
		if err := store.Upsert(setting.key, setting.value); err != nil {
			continue
		}
	}
}

// BuildTenantRuntimeConfig returns an isolated runtime snapshot for one tenant.
// Tenant-scoped settings must start empty so missing rows never inherit system credentials.
func BuildTenantRuntimeConfig(base *config.Config, tenantID string) config.Config {
	var tenantCfg config.Config
	if base != nil {
		tenantCfg = *base
	}
	tenantID = normalizeTenantID(tenantID)
	if tenantID == systemTenantID {
		return tenantCfg
	}

	tenantCfg.GeminiKey = nil
	tenantCfg.CodexKey = nil
	tenantCfg.ClaudeKey = nil
	tenantCfg.BedrockKey = nil
	tenantCfg.OpenCodeGoKey = nil
	tenantCfg.ClineKey = nil
	tenantCfg.OllamaCloudKey = nil
	tenantCfg.CommandCodeKey = nil
	tenantCfg.OpenAICompatibility = nil
	tenantCfg.VertexCompatAPIKey = nil
	tenantCfg.ClaudeHeaderDefaults = config.ClaudeHeaderDefaults{}
	tenantCfg.KimiHeaderDefaults = config.KimiHeaderDefaults{}
	// Clear tenant identity presets so system custom UA/version never leak, but
	// re-normalize after apply so missing runtime rows still default providers
	// to enabled (otherwise XAI/Codex learn+apply is silently off for new tenants).
	tenantCfg.IdentityFingerprint = config.IdentityFingerprintConfig{}
	tenantCfg.CodexOAuthAdmission = config.CodexOAuthAdmissionConfig{}
	tenantCfg.OAuthExcludedModels = nil
	tenantCfg.OAuthModelAlias = nil
	tenantCfg.Payload = config.PayloadConfig{}

	if routing := GetRoutingConfigForTenant(tenantID); routing != nil {
		tenantCfg.Routing = *routing
	} else {
		tenantCfg.Routing = config.RoutingConfig{IncludeDefaultGroup: true}
	}
	tenantCfg.ProxyPool = ListProxyPoolForTenant(tenantID)
	ApplyStoredRuntimeSettingsForTenant(tenantID, &tenantCfg)
	tenantCfg.SanitizeIdentityFingerprint()
	return tenantCfg
}
