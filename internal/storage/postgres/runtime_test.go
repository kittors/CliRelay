package postgres

import (
	"strings"
	"testing"
)

func TestRuntimeMigrationsCoverCoreTables(t *testing.T) {
	migrations := RuntimeMigrations()
	if len(migrations) != 1 {
		t.Fatalf("RuntimeMigrations len = %d, want 1", len(migrations))
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
}

func TestMigrationChecksumChangesWithSQL(t *testing.T) {
	first := migrationChecksum("SELECT 1")
	second := migrationChecksum("SELECT 2")
	if first == second {
		t.Fatal("migrationChecksum returned identical values for different SQL")
	}
}
