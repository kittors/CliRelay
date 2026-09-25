package usage

import (
	"context"
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

// PersistRuntimeSettingsPresentInYAML stores DB-backed runtime settings that
// were explicitly included in a management config.yaml save.
func PersistRuntimeSettingsPresentInYAML(cfg *config.Config, yamlContent []byte) int {
	if cfg == nil || !ConfigStoreAvailable() {
		return 0
	}
	return runtimeSettingsStore().PersistPresentInYAML(cfg, yamlContent)
}

// ApplyStoredRuntimeSettings overlays the stored settings onto the live config
// and records what was loaded, so a later management save writes only the
// keys it changed and checks them against these versions.
func ApplyStoredRuntimeSettings(cfg *config.Config) bool {
	if cfg == nil || !ConfigStoreAvailable() {
		return false
	}
	return applyStoredRuntimeSettings(runtimeSettingsStore(), cfg)
}

func applyStoredRuntimeSettings(store sqlsettings.RuntimeSettingsStore, cfg *config.Config) bool {
	applied, _ := store.ApplyToConfigRecording(cfg, cfg.RuntimeSettingState())
	if sqlsettings.EnsureStoredProviderIDs(context.Background(), store, cfg) {
		applied = true
	}
	sqlsettings.Rebaseline(cfg)
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

// ApplyStoredRuntimeSettingsForTenant overlays tenantID's stored settings onto
// cfg and records them in cfg's own state. cfg must not share its state with
// the live config; BuildTenantRuntimeConfig detaches it first.
func ApplyStoredRuntimeSettingsForTenant(tenantID string, cfg *config.Config) bool {
	if cfg == nil || !ConfigStoreAvailable() {
		return false
	}
	return applyStoredRuntimeSettings(runtimeSettingsStoreForTenant(tenantID), cfg)
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
	// The copy is loaded from this tenant's rows; sharing the base config's
	// state would record tenant versions as if they were the system's.
	tenantCfg.DetachRuntimeSettingState()

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
	sqlsettings.Rebaseline(&tenantCfg)
	return tenantCfg
}
