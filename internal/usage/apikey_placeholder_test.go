package usage

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func storedAPIKeyValues() []string {
	rows := ListAllAPIKeys()
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Key)
	}
	sort.Strings(keys)
	return keys
}

func warnEntriesContaining(hook *test.Hook, fragments ...string) int {
	count := 0
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.WarnLevel {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(entry.Message, fragment) {
				matched = false
				break
			}
		}
		if matched {
			count++
		}
	}
	return count
}

// A config.yaml created from an older config.example.yaml still lists the example
// keys; importing them would turn published strings into stored credentials.
func TestMigrateAPIKeysFromConfigSkipsPlaceholderKeys(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "api-keys:\n  - your-api-key-1\n  - sk-real-legacy\napi-key-entries:\n  - key: your-api-key-2\n  - key: sk-real-entry\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{
		APIKeys: []string{"your-api-key-1", "sk-real-legacy"},
		APIKeyEntries: []config.APIKeyEntry{
			{Key: "your-api-key-2", Name: "example"},
			{Key: "sk-real-entry", Name: "real"},
		},
	}}

	migrated, err := MigrateAPIKeysFromConfig(cfg, configPath)
	if err != nil {
		t.Fatalf("MigrateAPIKeysFromConfig error = %v", err)
	}
	if migrated != 2 {
		t.Fatalf("MigrateAPIKeysFromConfig = %d, want 2 (example keys skipped)", migrated)
	}
	if got := storedAPIKeyValues(); strings.Join(got, ",") != "sk-real-entry,sk-real-legacy" {
		t.Fatalf("stored keys = %q, want only the real keys", got)
	}
	if warnEntriesContaining(hook, "your-api-key-1", "your-api-key-2", "config.example.yaml") != 1 {
		t.Fatalf("expected one warning naming both skipped example keys, got entries %v", hook.AllEntries())
	}
}

// With nothing but example keys there is nothing to import. They stay in the
// in-memory config on purpose: the access provider then still sees configured
// keys and rejects every request, instead of reading "no keys" and letting
// allow-unauthenticated open the client API.
func TestMigrateAPIKeysFromConfigImportsNothingWhenOnlyPlaceholderKeys(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "api-keys:\n  - your-api-key-1\n  - your-api-key-2\n  - your-api-key-3\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{
		APIKeys: []string{"your-api-key-1", "your-api-key-2", "your-api-key-3"},
	}}

	migrated, err := MigrateAPIKeysFromConfig(cfg, configPath)
	if err != nil {
		t.Fatalf("MigrateAPIKeysFromConfig error = %v", err)
	}
	if migrated != 0 {
		t.Fatalf("MigrateAPIKeysFromConfig = %d, want 0", migrated)
	}
	if got := storedAPIKeyValues(); len(got) != 0 {
		t.Fatalf("stored keys = %q, want none", got)
	}
	if len(cfg.APIKeys) != 3 {
		t.Fatalf("in-memory api-keys = %q, want the example keys kept so the access provider stays registered", cfg.APIKeys)
	}
}

// Rows imported before the fix are reported at startup but left untouched: the
// operator has to replace them, and deleting a key someone still relies on is not
// a call startup can make.
func TestWarnEnabledPlaceholderAPIKeysReportsStoredExampleKeys(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()
	for _, row := range []APIKeyRow{
		{Key: "your-api-key-1", Name: "api-key-1"},
		{Key: "your-api-key-2", Name: "api-key-2", Disabled: true},
		{Key: "your-api-key-3", Name: "api-key-3"},
		{Key: "sk-real", Name: "real"},
	} {
		if err := UpsertAPIKey(row); err != nil {
			t.Fatalf("UpsertAPIKey(%s): %v", row.Name, err)
		}
	}
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()

	if got := WarnEnabledPlaceholderAPIKeys(); got != 2 {
		t.Fatalf("WarnEnabledPlaceholderAPIKeys() = %d, want 2 (the disabled one does not count)", got)
	}
	if warnEntriesContaining(hook, "2 enabled API key", "api-key-1", "api-key-3", "config.example.yaml", "rejected") != 1 {
		t.Fatalf("expected one warning naming both enabled example keys, got entries %v", hook.AllEntries())
	}
	if warnEntriesContaining(hook, "api-key-2") != 0 {
		t.Fatal("the disabled example key must not be reported")
	}
	if got := storedAPIKeyValues(); strings.Join(got, ",") != "sk-real,your-api-key-1,your-api-key-2,your-api-key-3" {
		t.Fatalf("stored keys = %q, want every row left in place", got)
	}
	if row := GetAPIKey("your-api-key-1"); row == nil || row.Disabled {
		t.Fatalf("example key row = %+v, want it left enabled", row)
	}
}

func TestWarnEnabledPlaceholderAPIKeysStaysQuietWithoutThem(t *testing.T) {
	cleanup := setupConfigMigrationTestDB(t)
	defer cleanup()
	if err := UpsertAPIKey(APIKeyRow{Key: "sk-real", Name: "real"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()

	if got := WarnEnabledPlaceholderAPIKeys(); got != 0 {
		t.Fatalf("WarnEnabledPlaceholderAPIKeys() = %d, want 0", got)
	}
	if n := warnEntriesContaining(hook); n != 0 {
		t.Fatalf("got %d warnings without any example key, want none", n)
	}
}
