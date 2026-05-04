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
	}

	if migrated := MigrateRuntimeSettingsFromConfig(cfg, configPath); migrated != 3 {
		t.Fatalf("MigrateRuntimeSettingsFromConfig = %d, want 3", migrated)
	}

	cfg.KimiHeaderDefaults = config.KimiHeaderDefaults{}
	cfg.IdentityFingerprint = config.IdentityFingerprintConfig{}
	cfg.OAuthModelAlias = nil
	if !ApplyStoredRuntimeSettings(cfg) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	if cfg.KimiHeaderDefaults.UserAgent != "KimiCLI/1.24.0" {
		t.Fatalf("KimiHeaderDefaults.UserAgent = %q", cfg.KimiHeaderDefaults.UserAgent)
	}
	if !cfg.IdentityFingerprint.Codex.Enabled || cfg.IdentityFingerprint.Codex.UserAgent != "codex_cli_rs/0.125.0" {
		t.Fatalf("IdentityFingerprint.Codex = %#v", cfg.IdentityFingerprint.Codex)
	}
	if !cfg.IdentityFingerprint.Claude.Enabled || cfg.IdentityFingerprint.Claude.UserAgent != "claude-cli/2.1.88 (external, cli)" {
		t.Fatalf("IdentityFingerprint.Claude = %#v", cfg.IdentityFingerprint.Claude)
	}
	if got := cfg.OAuthModelAlias["antigravity"]; len(got) != 1 || got[0].Alias != "gemini-2.5-computer-use-preview-10-2025" {
		t.Fatalf("OAuthModelAlias = %#v", cfg.OAuthModelAlias)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, forbidden := range []string{"kimi-header-defaults:", "identity-fingerprint:", "oauth-model-alias:"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("%s should be removed from YAML after migration:\n%s", forbidden, string(data))
		}
	}
	if !strings.Contains(string(data), "port: 8318") || !strings.Contains(string(data), "debug: true") {
		t.Fatalf("ordinary config should remain in YAML:\n%s", string(data))
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
	}

	if migrated := MigrateRuntimeSettingsFromConfig(cfg, configPath); migrated != 2 {
		t.Fatalf("MigrateRuntimeSettingsFromConfig = %d, want 2", migrated)
	}

	cfg.CodexKey = nil
	cfg.ClaudeKey = nil
	if !ApplyStoredRuntimeSettings(cfg) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	if len(cfg.CodexKey) != 1 || cfg.CodexKey[0].APIKey != "sk-codex-test" || cfg.CodexKey[0].BaseURL != "https://codex.example.com" {
		t.Fatalf("CodexKey after DB apply = %#v", cfg.CodexKey)
	}
	if len(cfg.ClaudeKey) != 1 || cfg.ClaudeKey[0].APIKey != "sk-claude-test" || cfg.ClaudeKey[0].Name != "claude-primary" {
		t.Fatalf("ClaudeKey after DB apply = %#v", cfg.ClaudeKey)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, forbidden := range []string{"codex-api-key:", "claude-api-key:"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("%s should be removed from YAML after migration:\n%s", forbidden, string(data))
		}
	}
	if !strings.Contains(string(data), "port: 8318") || !strings.Contains(string(data), "debug: true") {
		t.Fatalf("ordinary config should remain in YAML:\n%s", string(data))
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
	if !strings.Contains(string(data), "logging-to-file: true") {
		t.Fatalf("ordinary config should remain in YAML:\n%s", string(data))
	}
}
