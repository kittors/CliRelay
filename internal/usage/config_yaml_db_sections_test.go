package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

const everyDatabaseOwnedSectionYAML = `port: 8318
routing:
  strategy: round-robin
proxy-pool:
  - id: yaml-proxy
    url: http://yaml-proxy.example:8080
    enabled: true
api-keys:
  - sk-yaml-key-0000000000001
api-key-entries:
  - key: sk-yaml-entry-00000000002
api-key-permission-profiles:
  - id: yaml-profile
    name: From YAML
`

// Every database-owned section has a check and a place to send the operator,
// and is stripped by the YAML cleanup; a section added to the list without
// them would let saves through unchecked or fail every save naming it.
func TestDatabaseOwnedSectionsChangedByYAMLChecksEverySection(t *testing.T) {
	defer setupConfigMigrationTestDB(t)()
	ctx := context.Background()
	cfg := loadYAMLForSectionCheck(t, everyDatabaseOwnedSectionYAML)

	changed, err := DatabaseOwnedSectionsChangedByYAML(ctx, cfg, []byte(everyDatabaseOwnedSectionYAML), nil)
	if err != nil || len(changed) != 0 {
		t.Fatalf("nothing stored yet: changed = %+v, err = %v; want none, left to the first import", changed, err)
	}

	if err := UpsertRoutingConfig(config.RoutingConfig{Strategy: "fill-first", IncludeDefaultGroup: true}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceProxyPool([]config.ProxyPoolEntry{{ID: "stored-proxy", URL: "http://stored-proxy.example:8080", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAPIKey(APIKeyRow{Key: "sk-stored-key-00000000003", Name: "stored"}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceAllAPIKeyPermissionProfiles([]APIKeyPermissionProfileRow{{ID: "stored-profile", Name: "Stored"}}); err != nil {
		t.Fatal(err)
	}

	changed, err = DatabaseOwnedSectionsChangedByYAML(ctx, cfg, []byte(everyDatabaseOwnedSectionYAML), nil)
	if err != nil {
		t.Fatalf("DatabaseOwnedSectionsChangedByYAML: %v", err)
	}
	if len(changed) != len(databaseOwnedSectionOrder) {
		t.Fatalf("changed = %+v, want every section of %v", changed, databaseOwnedSectionOrder)
	}
	cleaned := dbBackedConfigYAMLKeys()
	for i, section := range changed {
		if section.Key != databaseOwnedSectionOrder[i] || section.Home == "" {
			t.Fatalf("section %d = %+v, want %s with a place to edit it", i, section, databaseOwnedSectionOrder[i])
		}
		if !cleaned[section.Key] {
			t.Fatalf("%s is not stripped by the DB-backed YAML cleanup", section.Key)
		}
	}
}

// API key sections count only for what a first import would take: the
// published example keys are never imported and stay in config.yaml on
// purpose, blank keys and nameless profiles are skipped. A profile section
// that does not decode counts, since nothing would ever apply it.
func TestDatabaseOwnedSectionsChangedByYAMLIgnoresWhatAnImportWouldSkip(t *testing.T) {
	defer setupConfigMigrationTestDB(t)()
	ctx := context.Background()
	if err := UpsertAPIKey(APIKeyRow{Key: "sk-stored-key-00000000003", Name: "stored"}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceAllAPIKeyPermissionProfiles([]APIKeyPermissionProfileRow{{ID: "stored-profile", Name: "Stored"}}); err != nil {
		t.Fatal(err)
	}

	skipped := "port: 8318\napi-keys:\n  - your-api-key-1\n  - \"  \"\napi-key-entries:\n  - key: your-api-key-2\n  - name: no key\napi-key-permission-profiles:\n  - id: no-name\n"
	changed, err := DatabaseOwnedSectionsChangedByYAML(ctx, loadYAMLForSectionCheck(t, skipped), []byte(skipped), nil)
	if err != nil || len(changed) != 0 {
		t.Fatalf("changed = %+v, err = %v; want none", changed, err)
	}

	undecodable := "port: 8318\napi-key-permission-profiles: not a list\n"
	changed, err = DatabaseOwnedSectionsChangedByYAML(ctx, loadYAMLForSectionCheck(t, undecodable), []byte(undecodable), nil)
	if err != nil || len(changed) != 1 || changed[0].Key != "api-key-permission-profiles" {
		t.Fatalf("changed = %+v, err = %v; want api-key-permission-profiles", changed, err)
	}
}

// A file edited by hand cannot be rejected; the reload that drops a routing or
// proxy-pool section the database owns says so when the section differed,
// and stays quiet about a copy of the stored value.
func TestReloadWarnsWhenItIgnoresADifferentSection(t *testing.T) {
	defer setupConfigMigrationTestDB(t)()
	if err := UpsertRoutingConfig(config.RoutingConfig{Strategy: "fill-first", IncludeDefaultGroup: true}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceProxyPool([]config.ProxyPoolEntry{{ID: "stored-proxy", Name: "stored-proxy", URL: "http://stored-proxy.example:8080", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()

	cases := []struct {
		name, yaml, warning string
	}{
		{"same routing", "routing:\n  strategy: ff\n", ""},
		{"same proxy pool", "proxy-pool:\n  - id: Stored-Proxy\n    url: http://stored-proxy.example:8080\n    enabled: true\n", ""},
		{"other routing", "routing:\n  strategy: round-robin\n", "ignored the routing section"},
		{"other proxy pool", "proxy-pool:\n  - id: other\n    url: http://other.example:8080\n    enabled: true\n", "ignored the proxy-pool section"},
	}
	for _, tc := range cases {
		hook.Reset()
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(configPath, []byte("port: 8318\n"+tc.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			t.Fatal(err)
		}

		MigrateRoutingConfigFromConfig(cfg, configPath)
		MigrateProxyPoolFromConfig(cfg, configPath)

		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "routing:") || strings.Contains(string(data), "proxy-pool:") {
			t.Fatalf("%s: the section should be dropped from config.yaml:\n%s", tc.name, data)
		}
		warned := ""
		for _, entry := range hook.AllEntries() {
			if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "ignored the") {
				warned = entry.Message
			}
		}
		if tc.warning == "" && warned != "" {
			t.Fatalf("%s: unexpected warning %q", tc.name, warned)
		}
		if tc.warning != "" && !strings.Contains(warned, tc.warning) {
			t.Fatalf("%s: warning = %q, want one containing %q", tc.name, warned, tc.warning)
		}
	}
}

// Dropping config.yaml keys from a node whose database might hold none could
// leave it without any key, and allow-unauthenticated would let everyone in:
// when the database cannot say, the keys stay. The published example keys are
// dropped once it holds keys, without a warning about keys nobody used.
func TestDropYAMLAPIKeysWhenStoredOnlyDropsWhatTheDatabaseReplaces(t *testing.T) {
	defer setupConfigMigrationTestDB(t)()
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	write := func(body string) {
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := UpsertAPIKey(APIKeyRow{Key: "sk-stored-key-00000000003", Name: "stored"}); err != nil {
		t.Fatal(err)
	}
	write("port: 8318\napi-keys:\n  - your-api-key-1\n")
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"your-api-key-1"}}}
	if !DropYAMLAPIKeysWhenStored(cfg, configPath) || len(cfg.APIKeys) != 0 {
		t.Fatalf("example keys should be dropped once the database holds keys: %v", cfg.APIKeys)
	}
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "ignored the api-keys") {
			t.Fatalf("warned about example keys: %q", entry.Message)
		}
	}

	if _, err := getDB().Exec("DROP TABLE api_keys"); err != nil {
		t.Fatal(err)
	}
	write("port: 8318\napi-keys:\n  - sk-yaml-key-0000000000001\n")
	cfg = &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"sk-yaml-key-0000000000001"}}}
	if DropYAMLAPIKeysWhenStored(cfg, configPath) || len(cfg.APIKeys) != 1 {
		t.Fatalf("keys must stay when the database cannot answer: %v", cfg.APIKeys)
	}
	if data, _ := os.ReadFile(configPath); !strings.Contains(string(data), "sk-yaml-key-0000000000001") {
		t.Fatalf("config.yaml lost its keys:\n%s", data)
	}
}

func loadYAMLForSectionCheck(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("load %q: %v", body, err)
	}
	return cfg
}
