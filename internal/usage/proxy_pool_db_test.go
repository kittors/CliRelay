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

func setupProxyPoolTestDB(t *testing.T) func() {
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
	initProxyPoolTable(db)

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

func TestProxyPoolReplaceListAndGet(t *testing.T) {
	cleanup := setupProxyPoolTestDB(t)
	defer cleanup()

	err := ReplaceProxyPool([]config.ProxyPoolEntry{
		{ID: " HK ", Name: " HK Proxy ", URL: " http://127.0.0.1:7890 ", Enabled: true, Description: " primary "},
		{ID: "bad", Name: "bad", URL: "ftp://invalid", Enabled: true},
	})
	if err != nil {
		t.Fatalf("ReplaceProxyPool: %v", err)
	}

	rows := ListProxyPool()
	if len(rows) != 1 {
		t.Fatalf("ListProxyPool length = %d, want 1: %#v", len(rows), rows)
	}
	if rows[0].ID != "hk" || rows[0].Name != "HK Proxy" || rows[0].URL != "http://127.0.0.1:7890" || !rows[0].Enabled {
		t.Fatalf("stored proxy row = %#v", rows[0])
	}
	if rows[0].Description != "primary" {
		t.Fatalf("description = %q, want primary", rows[0].Description)
	}

	got := GetProxyPoolEntry(" HK ")
	if got == nil || got.ID != "hk" {
		t.Fatalf("GetProxyPoolEntry = %#v, want hk", got)
	}
}

func TestProxyPoolMigrationMovesConfigEntriesIntoSQLite(t *testing.T) {
	cleanup := setupProxyPoolTestDB(t)
	defer cleanup()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("proxy-pool:\n  - id: hk\n    name: HK\n    url: http://127.0.0.1:7890\n    enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "hk", Name: "HK", URL: "http://127.0.0.1:7890", Enabled: true},
		},
	}

	if migrated := MigrateProxyPoolFromConfig(cfg, configPath); migrated != 1 {
		t.Fatalf("MigrateProxyPoolFromConfig = %d, want 1", migrated)
	}
	if len(cfg.ProxyPool) != 0 {
		t.Fatalf("cfg.ProxyPool after migration = %#v, want empty before DB apply", cfg.ProxyPool)
	}
	if !ApplyStoredProxyPool(cfg) {
		t.Fatal("ApplyStoredProxyPool returned false")
	}
	if len(cfg.ProxyPool) != 1 || cfg.ProxyPool[0].ID != "hk" {
		t.Fatalf("cfg.ProxyPool after apply = %#v", cfg.ProxyPool)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "proxy-pool:") {
		t.Fatalf("proxy-pool should be removed from YAML after migration:\n%s", string(data))
	}
	assertMigrationBackupMode(t, configPath+".pre-proxy-pool-sqlite-migration", 0o600)
}

func TestProxyPoolMigrationCleansYAMLWhenSQLiteAlreadyHasEntries(t *testing.T) {
	cleanup := setupProxyPoolTestDB(t)
	defer cleanup()

	if err := ReplaceProxyPool([]config.ProxyPoolEntry{
		{ID: "db-proxy", Name: "DB Proxy", URL: "http://127.0.0.1:7890", Enabled: true},
	}); err != nil {
		t.Fatalf("ReplaceProxyPool: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("proxy-pool:\n  - id: yaml-proxy\n    url: http://127.0.0.1:7891\nlogging-to-file: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := &config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "yaml-proxy", URL: "http://127.0.0.1:7891", Enabled: true},
		},
	}

	if migrated := MigrateProxyPoolFromConfig(cfg, configPath); migrated != 0 {
		t.Fatalf("MigrateProxyPoolFromConfig = %d, want 0 when DB already has entries", migrated)
	}
	if len(cfg.ProxyPool) != 0 {
		t.Fatalf("cfg.ProxyPool after cleanup = %#v, want empty before DB apply", cfg.ProxyPool)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "proxy-pool:") {
		t.Fatalf("stale proxy-pool should be removed from YAML:\n%s", string(data))
	}
	if !strings.Contains(string(data), "logging-to-file: true") {
		t.Fatalf("ordinary config should remain in YAML:\n%s", string(data))
	}
}
