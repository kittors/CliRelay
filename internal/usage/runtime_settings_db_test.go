package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestRuntimeSettingsMigrationMovesConfigIntoSQLiteAndCleansYAML(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := `port: 8318
kimi-header-defaults:
  user-agent: KimiCLI/1.24.0
identity-fingerprint:
  codex:
    enabled: true
    user-agent: codex_cli_rs/0.125.0
  claude:
    enabled: true
    cli-version: 2.1.88
    entrypoint: cli
codex-oauth-admission:
  allowed-clients:
    - claude_code
oauth-model-alias:
  antigravity:
    - name: rev19-uic3-1p
      alias: gemini-2.5-computer-use-preview-10-2025
debug: true
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{
		KimiHeaderDefaults: config.KimiHeaderDefaults{UserAgent: "KimiCLI/1.24.0"},
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{Enabled: true, UserAgent: "codex_cli_rs/0.125.0"},
			Claude: config.ClaudeIdentityFingerprintConfig{
				Enabled:    true,
				CLIVersion: "2.1.88",
				Entrypoint: "cli",
			},
		},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"antigravity": {{Name: "rev19-uic3-1p", Alias: "gemini-2.5-computer-use-preview-10-2025"}},
		},
		CodexOAuthAdmission: config.CodexOAuthAdmissionConfig{AllowedClientPresets: []string{"claude_code"}},
		// Mirrors "debug: true" above; debug is database-backed now too.
		Debug: true,
	}
	withLoadDefaults(cfg)

	if migrated := MigrateRuntimeSettingsFromConfig(cfg, configPath); migrated != 5 {
		t.Fatalf("MigrateRuntimeSettingsFromConfig = %d, want 5", migrated)
	}

	cfg.KimiHeaderDefaults = config.KimiHeaderDefaults{}
	cfg.IdentityFingerprint = config.IdentityFingerprintConfig{}
	cfg.OAuthModelAlias = nil
	cfg.CodexOAuthAdmission = config.CodexOAuthAdmissionConfig{}
	if !ApplyStoredRuntimeSettings(cfg) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	if cfg.KimiHeaderDefaults.UserAgent != "KimiCLI/1.24.0" {
		t.Fatalf("KimiHeaderDefaults.UserAgent = %q", cfg.KimiHeaderDefaults.UserAgent)
	}
	if !cfg.IdentityFingerprint.Codex.Enabled || cfg.IdentityFingerprint.Codex.UserAgent != "codex_cli_rs/0.125.0" {
		t.Fatalf("IdentityFingerprint.Codex = %#v", cfg.IdentityFingerprint.Codex)
	}
	if !cfg.IdentityFingerprint.Claude.Enabled || cfg.IdentityFingerprint.Claude.UserAgent != "" ||
		cfg.IdentityFingerprint.Claude.CLIVersion != "2.1.88" ||
		cfg.IdentityFingerprint.Claude.Entrypoint != "cli" {
		t.Fatalf("IdentityFingerprint.Claude = %#v", cfg.IdentityFingerprint.Claude)
	}
	if got := cfg.OAuthModelAlias["antigravity"]; len(got) != 1 || got[0].Alias != "gemini-2.5-computer-use-preview-10-2025" {
		t.Fatalf("OAuthModelAlias = %#v", cfg.OAuthModelAlias)
	}
	if got := cfg.CodexOAuthAdmission.AllowedClientPresets; len(got) != 1 || got[0] != "claude_code" {
		t.Fatalf("CodexOAuthAdmission = %#v, want [claude_code]", cfg.CodexOAuthAdmission)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, forbidden := range []string{"kimi-header-defaults:", "identity-fingerprint:", "codex-oauth-admission:", "oauth-model-alias:"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("%s should be removed from YAML after migration:\n%s", forbidden, string(data))
		}
	}
	if !strings.Contains(string(data), "port: 8318") {
		t.Fatalf("node-local config should remain in YAML:\n%s", string(data))
	}
	if strings.Contains(string(data), "debug:") {
		t.Fatalf("debug is database-backed and should be removed from YAML:\n%s", string(data))
	}
	if payload, ok := GetRuntimeSettingPayload("debug"); !ok || string(payload) != "true" {
		t.Fatalf("stored debug = %s (%v), want true", payload, ok)
	}
	assertMigrationBackupMode(t, configPath+".pre-runtime-settings-sqlite-migration", 0o600)
}

func TestRuntimeSettingsMigrationMovesProviderCredentialsIntoSQLite(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := `port: 8318
codex-api-key:
  - api-key: sk-codex-test
    base-url: https://codex.example.com
claude-api-key:
  - api-key: sk-claude-test
    name: claude-primary
    base-url: https://claude.example.com
opencode-go-api-key:
  - api-key: sk-opencode-test
    models:
      - name: deepseek-v4-flash
        alias: deepseek-v4-flash
cline-api-key:
  - api-key: sk-cline-test
    name: cline-primary
    base-url: https://api.cline.bot/api/v1
    models:
      - name: cline-pass/qwen3.7-max
        alias: qwen3.7-max
ollama-cloud-api-key:
  - api-key: sk-ollama-test
    base-url: https://ollama.com
    models:
      - name: gpt-oss:120b
debug: true
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{APIKey: "sk-codex-test", BaseURL: "https://codex.example.com"},
		},
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "sk-claude-test", Name: "claude-primary", BaseURL: "https://claude.example.com"},
		},
		OpenCodeGoKey: []config.OpenCodeGoKey{
			{APIKey: "sk-opencode-test", Models: []config.OpenCodeGoModel{{Name: "deepseek-v4-flash", Alias: "deepseek-v4-flash"}}},
		},
		ClineKey: []config.ClineKey{
			{APIKey: "sk-cline-test", Name: "cline-primary", BaseURL: "https://api.cline.bot/api/v1", Models: []config.ClineModel{{Name: "cline-pass/qwen3.7-max", Alias: "qwen3.7-max"}}},
		},
		OllamaCloudKey: []config.OllamaCloudKey{
			{APIKey: "sk-ollama-test", BaseURL: "https://ollama.com", Models: []config.OllamaCloudModel{{Name: "gpt-oss:120b"}}},
		},
		Debug: true,
	}
	withLoadDefaults(cfg)

	if migrated := MigrateRuntimeSettingsFromConfig(cfg, configPath); migrated != 6 {
		t.Fatalf("MigrateRuntimeSettingsFromConfig = %d, want 6", migrated)
	}

	cfg.CodexKey = nil
	cfg.ClaudeKey = nil
	cfg.OpenCodeGoKey = nil
	cfg.ClineKey = nil
	cfg.OllamaCloudKey = nil
	if !ApplyStoredRuntimeSettings(cfg) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	if len(cfg.CodexKey) != 1 || cfg.CodexKey[0].APIKey != "sk-codex-test" || cfg.CodexKey[0].BaseURL != "https://codex.example.com" {
		t.Fatalf("CodexKey after DB apply = %#v", cfg.CodexKey)
	}
	if len(cfg.ClaudeKey) != 1 || cfg.ClaudeKey[0].APIKey != "sk-claude-test" || cfg.ClaudeKey[0].Name != "claude-primary" {
		t.Fatalf("ClaudeKey after DB apply = %#v", cfg.ClaudeKey)
	}
	if len(cfg.OpenCodeGoKey) != 1 || cfg.OpenCodeGoKey[0].APIKey != "sk-opencode-test" || cfg.OpenCodeGoKey[0].Models[0].Name != "deepseek-v4-flash" {
		t.Fatalf("OpenCodeGoKey after DB apply = %#v", cfg.OpenCodeGoKey)
	}
	if len(cfg.ClineKey) != 1 || cfg.ClineKey[0].APIKey != "sk-cline-test" || cfg.ClineKey[0].Models[0].Alias != "qwen3.7-max" {
		t.Fatalf("ClineKey after DB apply = %#v", cfg.ClineKey)
	}
	if len(cfg.OllamaCloudKey) != 1 || cfg.OllamaCloudKey[0].APIKey != "sk-ollama-test" || cfg.OllamaCloudKey[0].Models[0].Name != "gpt-oss:120b" {
		t.Fatalf("OllamaCloudKey after DB apply = %#v", cfg.OllamaCloudKey)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, forbidden := range []string{"codex-api-key:", "claude-api-key:", "opencode-go-api-key:", "cline-api-key:", "ollama-cloud-api-key:"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("%s should be removed from YAML after migration:\n%s", forbidden, string(data))
		}
	}
	if !strings.Contains(string(data), "port: 8318") || strings.Contains(string(data), "debug:") {
		t.Fatalf("node-local config should remain and debug should move to the database:\n%s", string(data))
	}
}

func TestRuntimeSettingsSQLiteWinsOverStaleYAML(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	if err := UpsertRuntimeSetting(RuntimeSettingKimiHeaderDefaults, config.KimiHeaderDefaults{UserAgent: "KimiCLI/db"}); err != nil {
		t.Fatalf("UpsertRuntimeSetting: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("kimi-header-defaults:\n  user-agent: KimiCLI/yaml\nlogging-to-file: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{KimiHeaderDefaults: config.KimiHeaderDefaults{UserAgent: "KimiCLI/yaml"}}
	withLoadDefaults(cfg)

	if migrated := MigrateRuntimeSettingsFromConfig(cfg, configPath); migrated != 0 {
		t.Fatalf("MigrateRuntimeSettingsFromConfig = %d, want 0 when DB already has row", migrated)
	}
	if !ApplyStoredRuntimeSettings(cfg) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	if cfg.KimiHeaderDefaults.UserAgent != "KimiCLI/db" {
		t.Fatalf("KimiHeaderDefaults.UserAgent = %q, want DB value", cfg.KimiHeaderDefaults.UserAgent)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "kimi-header-defaults:") {
		t.Fatalf("stale kimi-header-defaults should be removed from YAML:\n%s", string(data))
	}
	// logging-to-file is database-backed now; the YAML copy was never imported
	// (the config above does not set it), and the cleanup drops it with the
	// other stale database-owned sections.
	if strings.Contains(string(data), "logging-to-file:") {
		t.Fatalf("database-owned logging-to-file should be removed from YAML:\n%s", string(data))
	}
}

// withLoadDefaults gives a literal config the defaults LoadConfig applies, so
// the settings it does not mention compare as "not configured".
func withLoadDefaults(cfg *config.Config) {
	cfg.LogsMaxTotalSizeMB = config.DefaultLogsMaxTotalSizeMB
	cfg.ErrorLogsMaxFiles = config.DefaultErrorLogsMaxFiles
	cfg.RequestLogStorage = config.DefaultRequestLogStorageConfig()
}

func TestBuildTenantRuntimeConfigDoesNotInheritSystemCredentials(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	base := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "system-gemini"}},
		CodexKey:  []config.CodexKey{{APIKey: "system-codex"}},
	}
	const tenantID = "00000000-0000-0000-0000-00000000000a"
	got := BuildTenantRuntimeConfig(base, tenantID)
	if len(got.GeminiKey) != 0 || len(got.CodexKey) != 0 {
		t.Fatalf("tenant inherited system credentials: gemini=%v codex=%v", got.GeminiKey, got.CodexKey)
	}

	if err := UpsertRuntimeSettingForTenant(tenantID, RuntimeSettingGeminiKeys, []config.GeminiKey{{APIKey: "tenant-gemini"}}); err != nil {
		t.Fatalf("UpsertRuntimeSettingForTenant: %v", err)
	}
	got = BuildTenantRuntimeConfig(base, tenantID)
	if len(got.GeminiKey) != 1 || got.GeminiKey[0].APIKey != "tenant-gemini" || len(got.CodexKey) != 0 {
		t.Fatalf("tenant runtime config = %+v", got)
	}
}

func TestBuildTenantRuntimeConfigDefaultsIdentityFingerprintEnabledWithoutRuntimeRow(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	// System may carry custom identity presets; tenants must not inherit them,
	// but learn/apply must still be on by default when the tenant has no row.
	base := &config.Config{}
	base.IdentityFingerprint = config.NormalizeIdentityFingerprintConfig(config.IdentityFingerprintConfig{
		XAI: config.XAIIdentityFingerprintConfig{
			Enabled:          true,
			UserAgent:        "system-custom-ua",
			ClientIdentifier: "system-custom-id",
			ClientVersion:    "9.9.9",
		},
	})
	const tenantID = "00000000-0000-0000-0000-00000000000b"
	got := BuildTenantRuntimeConfig(base, tenantID)
	if !got.IdentityFingerprint.XAI.Enabled ||
		!got.IdentityFingerprint.Codex.Enabled ||
		!got.IdentityFingerprint.Claude.Enabled ||
		!got.IdentityFingerprint.Gemini.Enabled {
		t.Fatalf("IdentityFingerprint = %#v, want all providers enabled by default for tenant without runtime row", got.IdentityFingerprint)
	}
	if got.IdentityFingerprint.XAI.UserAgent == "system-custom-ua" ||
		got.IdentityFingerprint.XAI.ClientIdentifier == "system-custom-id" ||
		got.IdentityFingerprint.XAI.ClientVersion == "9.9.9" {
		t.Fatalf("tenant inherited system identity presets: %#v", got.IdentityFingerprint.XAI)
	}
}

func TestBuildTenantRuntimeConfigPreservesExplicitDisabledIdentityFingerprint(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	const tenantID = "00000000-0000-0000-0000-00000000000c"
	if err := UpsertRuntimeSettingForTenant(tenantID, RuntimeSettingIdentityFingerprint, map[string]any{
		"runtime-setting-version": 2,
		"xai": map[string]any{
			"enabled": false,
		},
		"codex": map[string]any{
			"enabled": true,
		},
	}); err != nil {
		t.Fatalf("UpsertRuntimeSettingForTenant: %v", err)
	}
	got := BuildTenantRuntimeConfig(&config.Config{}, tenantID)
	if got.IdentityFingerprint.XAI.Enabled {
		t.Fatalf("XAI.Enabled = true, want explicit false preserved")
	}
	if !got.IdentityFingerprint.Codex.Enabled {
		t.Fatalf("Codex.Enabled = false, want true from tenant runtime row")
	}
}

func TestTenantRuntimeProviderStableIDBackfillPersistsOnce(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	const tenantID = "00000000-0000-0000-0000-0000000000aa"
	if err := UpsertRuntimeSettingForTenant(tenantID, RuntimeSettingGeminiKeys, []config.GeminiKey{{APIKey: "tenant-secret"}}); err != nil {
		t.Fatalf("UpsertRuntimeSettingForTenant: %v", err)
	}
	first := BuildTenantRuntimeConfig(&config.Config{}, tenantID)
	if len(first.GeminiKey) != 1 || first.GeminiKey[0].ID == "" {
		t.Fatalf("first backfill = %#v", first.GeminiKey)
	}
	second := BuildTenantRuntimeConfig(&config.Config{}, tenantID)
	if second.GeminiKey[0].ID != first.GeminiKey[0].ID {
		t.Fatalf("tenant stable ID changed: first=%q second=%q", first.GeminiKey[0].ID, second.GeminiKey[0].ID)
	}
}
