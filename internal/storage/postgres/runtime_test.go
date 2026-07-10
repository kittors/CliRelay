package postgres

import (
	"strings"
	"testing"
)

func TestRuntimeMigrationsCoverCoreTables(t *testing.T) {
	migrations := RuntimeMigrations()
	if len(migrations) != 2 {
		t.Fatalf("RuntimeMigrations len = %d, want 2", len(migrations))
	}
	sqlText := migrations[0].SQL
	for _, table := range []string{
		"request_logs",
		"request_log_content",
		"api_keys",
		"api_key_permission_profiles",
		"model_configs",
		"model_pricing",
		"proxy_pool",
		"routing_config",
		"runtime_settings",
		"identity_fingerprints",
		"ccswitch_import_configs",
	} {
		if !strings.Contains(sqlText, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Fatalf("runtime migration does not create %s", table)
		}
	}

	profileSQL := migrations[1].SQL
	for _, fragment := range []string{
		"ADD COLUMN IF NOT EXISTS profile_key",
		"codex_quarantined",
		"ADD PRIMARY KEY (provider, account_key, profile_key)",
		"CREATE TABLE IF NOT EXISTS identity_fingerprint_account_policies",
		"strategy = 'cli_preferred' AND active_profile_key = ''",
	} {
		if !strings.Contains(profileSQL, fragment) {
			t.Fatalf("identity profile migration missing %q", fragment)
		}
	}
}

func TestMigrationChecksumChangesWithSQL(t *testing.T) {
	first := migrationChecksum("SELECT 1")
	second := migrationChecksum("SELECT 2")
	if first == second {
		t.Fatal("migrationChecksum returned identical values for different SQL")
	}
}
