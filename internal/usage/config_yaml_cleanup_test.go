package usage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "modernc.org/sqlite"
)

func setupConfigMigrationTestDB(t *testing.T) func() {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(createTableSQL); err != nil {
		_ = db.Close()
		t.Fatalf("create request_logs table: %v", err)
	}
	initAPIKeysTable(db)
	initRoutingConfigTable(db)
	initProxyPoolTable(db)
	initRuntimeSettingsTable(db)

	usageDBMu.Lock()
	usageDB = db
	usageDBPath = dbPath
	usageDBMu.Unlock()

	return func() {
		usageDBMu.Lock()
		if usageDB != nil {
			_ = usageDB.Close()
			usageDB = nil
		}
		usageDBPath = ""
		usageDBMu.Unlock()
	}
}

func TestMigrateRoutingConfigFromConfigCleansYAML(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8318\nrouting:\n  strategy: round-robin\n  channel-groups:\n    - name: chatgpt-pro\n      match:\n        channels:\n          - GptPro1\ndebug: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			Strategy: "round-robin",
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "chatgpt-pro", Match: config.ChannelGroupMatch{Channels: []string{"GptPro1"}}},
			},
			IncludeDefaultGroup: true,
		},
	}

	if !MigrateRoutingConfigFromConfig(cfg, configPath) {
		t.Fatal("MigrateRoutingConfigFromConfig returned false")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "routing:") {
		t.Fatalf("routing should be removed from YAML after migration:\n%s", string(data))
	}
	if !strings.Contains(string(data), "port: 8318") || !strings.Contains(string(data), "debug: true") {
		t.Fatalf("non-DB-backed config should remain in YAML:\n%s", string(data))
	}
	assertMigrationBackupMode(t, configPath+".pre-routing-sqlite-migration", 0o600)
}

func TestMigrateRoutingConfigFromConfigKeepsYAMLWhenDBUnavailable(t *testing.T) {
	usageDBMu.Lock()
	usageDB = nil
	usageDBPath = ""
	usageDBMu.Unlock()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("routing:\n  strategy: round-robin\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{Routing: config.RoutingConfig{Strategy: "round-robin", IncludeDefaultGroup: true}}

	if MigrateRoutingConfigFromConfig(cfg, configPath) {
		t.Fatal("MigrateRoutingConfigFromConfig returned true without a DB")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "routing:") {
		t.Fatalf("routing should remain when DB is unavailable:\n%s", string(data))
	}
}

func TestCleanDBBackedConfigFromYAMLCleansPersistedSections(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("port: 8318\napi-keys:\n  - sk-test\napi-key-entries:\n  - key: sk-entry\nrouting:\n  strategy: round-robin\nproxy-pool:\n  - id: hk\n    url: http://127.0.0.1:7890\ngemini-api-key:\n  - api-key: sk-gemini\ncodex-api-key:\n  - api-key: sk-codex\n    base-url: https://codex.example.com\nclaude-api-key:\n  - api-key: sk-claude\n    base-url: https://claude.example.com\nbedrock-api-key:\n  - name: bedrock\nopencode-go-api-key:\n  - api-key: sk-opencode\nopenai-compatibility:\n  - name: compat\n    base-url: https://compat.example.com\nvertex-api-key:\n  - api-key: sk-vertex\n    base-url: https://vertex.example.com\nclaude-header-defaults:\n  user-agent: ClaudeCLI/1.0\nkimi-header-defaults:\n  user-agent: KimiCLI/1.24.0\nidentity-fingerprint:\n  codex:\n    enabled: true\noauth-excluded-models:\n  codex:\n    - gpt-4\noauth-model-alias:\n  codex:\n    - name: gpt-5\n      alias: codex-gpt5\npayload:\n  default:\n    - models:\n        - name: gpt-5\n      params:\n        temperature: 0.2\nlogging-to-file: true\n")
	if err := os.WriteFile(configPath, content, 0o640); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if removed := CleanDBBackedConfigFromYAML(configPath); removed != 17 {
		t.Fatalf("CleanDBBackedConfigFromYAML removed %d sections, want 17", removed)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, forbidden := range []string{"api-keys:", "api-key-entries:", "routing:", "proxy-pool:", "gemini-api-key:", "codex-api-key:", "claude-api-key:", "bedrock-api-key:", "opencode-go-api-key:", "openai-compatibility:", "vertex-api-key:", "claude-header-defaults:", "kimi-header-defaults:", "identity-fingerprint:", "oauth-excluded-models:", "oauth-model-alias:", "payload:"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("%s should be removed from YAML:\n%s", forbidden, string(data))
		}
	}
	if !strings.Contains(string(data), "port: 8318") || !strings.Contains(string(data), "logging-to-file: true") {
		t.Fatalf("ordinary config should remain in YAML:\n%s", string(data))
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("cleaned config mode = %o, want 640", got)
	}
}

func TestAPIKeyMigrationBackupUsesPrivatePermissions(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("api-keys:\n  - sk-test\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"sk-test"}}}

	if migrated := MigrateAPIKeysFromConfig(cfg, configPath); migrated != 1 {
		t.Fatalf("MigrateAPIKeysFromConfig = %d, want 1", migrated)
	}
	assertMigrationBackupMode(t, configPath+".pre-sqlite-migration", 0o600)
}

func assertMigrationBackupMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected migration backup %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("backup mode for %s = %o, want %o", path, got, want)
	}
}
