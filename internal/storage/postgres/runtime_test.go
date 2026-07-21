package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRuntimeMigrationsCoverCoreTables(t *testing.T) {
	migrations := RuntimeMigrations()
	if len(migrations) != 21 {
		t.Fatalf("RuntimeMigrations len = %d, want 21", len(migrations))
	}
	// Latest: append-only account-level daily spending reset history.
	endUserResetEventsSQL := migrations[20].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS end_user_daily_spending_reset_events",
		"effective_used_before",
		"raw_today_cost",
		"actor_username",
	} {
		if !strings.Contains(endUserResetEventsSQL, fragment) {
			t.Fatalf("end-user daily spending reset events migration missing %q", fragment)
		}
	}
	// Prior: usage rollup buckets for stats/limits isolation from request_logs.
	rollupSQL := migrations[18].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS usage_rollup_buckets",
		"bucket_kind",
		"effective_input_tokens",
		"CREATE TABLE IF NOT EXISTS request_log_storage_state",
	} {
		if !strings.Contains(rollupSQL, fragment) {
			t.Fatalf("usage rollup migration missing %q", fragment)
		}
	}
	// Prior: account-level daily spending reset baselines.
	endUserResetSQL := migrations[17].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS end_user_daily_spending_resets",
		"cost_baseline",
		"day_key",
	} {
		if !strings.Contains(endUserResetSQL, fragment) {
			t.Fatalf("end-user daily spending reset migration missing %q", fragment)
		}
	}
	// Prior: account-level quota on end_users.
	accountQuotaSQL := migrations[16].SQL
	for _, fragment := range []string{
		"end_users ADD COLUMN IF NOT EXISTS permission_profile_id",
		"end_users ADD COLUMN IF NOT EXISTS daily_limit",
		"WHERE end_user_id IS NOT NULL",
	} {
		if !strings.Contains(accountQuotaSQL, fragment) {
			t.Fatalf("end user account quota migration missing %q", fragment)
		}
	}
	// Prior: end users + refresh tokens.
	endUsersSQL := migrations[15].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS end_users",
		"end_user_sessions",
		"access_token_ttl_seconds",
		"end_users.read",
	} {
		if !strings.Contains(endUsersSQL, fragment) {
			t.Fatalf("end users migration missing %q", fragment)
		}
	}
	// Prior: append-only daily spending reset history.
	resetEventsSQL := migrations[14].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS api_key_daily_spending_reset_events",
		"effective_used_before",
		"raw_today_cost",
		"actor_username",
	} {
		if !strings.Contains(resetEventsSQL, fragment) {
			t.Fatalf("daily spending reset events migration missing %q", fragment)
		}
	}
	// Prior: daily spending limit on permission profiles.
	profileSpendSQL := migrations[13].SQL
	if !strings.Contains(profileSpendSQL, "daily_spending_limit") {
		t.Fatalf("profile daily spending migration missing daily_spending_limit")
	}
	// Prior: API key daily spending reset baselines.
	resetSQL := migrations[12].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS api_key_daily_spending_resets",
		"cost_baseline",
		"day_key",
	} {
		if !strings.Contains(resetSQL, fragment) {
			t.Fatalf("daily spending reset migration missing %q", fragment)
		}
	}
	// Prior migration: AI account status read model tables.
	statusSQL := migrations[11].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS ai_account_status",
		"CREATE TABLE IF NOT EXISTS auth_subject_usage_daily",
		"success_count",
		"usage_projection_markers",
	} {
		if !strings.Contains(statusSQL, fragment) {
			t.Fatalf("ai account status migration missing %q", fragment)
		}
	}
	sharedSQL := migrations[19].SQL
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS ai_account_subjects",
		"CREATE TABLE IF NOT EXISTS ai_account_tenant_bindings",
		"CREATE TABLE IF NOT EXISTS ai_account_subject_status",
		"CREATE TABLE IF NOT EXISTS ai_account_subject_usage_buckets",
		"CREATE TABLE IF NOT EXISTS ai_account_subject_quota_cycles",
		"CREATE TABLE IF NOT EXISTS ai_account_subject_quota_points",
	} {
		if !strings.Contains(sharedSQL, fragment) {
			t.Fatalf("shared AI account migration missing %q", fragment)
		}
	}

	authLookupSQL := migrations[10].SQL
	for _, fragment := range []string{
		"idx_request_logs_tenant_auth_index_time",
		"idx_request_logs_tenant_auth_subject_time_cost",
	} {
		if !strings.Contains(authLookupSQL, fragment) {
			t.Fatalf("auth lookup index migration missing %q", fragment)
		}
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

	scopeSQL := migrations[3].SQL
	for _, table := range []string{"api_keys", "request_logs", "runtime_settings", "identity_fingerprints"} {
		if !strings.Contains(scopeSQL, "ALTER TABLE "+table+" ADD COLUMN IF NOT EXISTS tenant_id") {
			t.Fatalf("tenant scope migration does not alter %s", table)
		}
	}

	constraintsSQL := migrations[4].SQL
	for _, fragment := range []string{
		"ADD PRIMARY KEY (tenant_id, model_id)",
		"FOREIGN KEY (tenant_id, log_id)",
		"idx_ccswitch_import_configs_tenant_route_path",
	} {
		if !strings.Contains(constraintsSQL, fragment) {
			t.Fatalf("tenant constraints migration missing %q", fragment)
		}
	}

	deleteConstraintsSQL := migrations[5].SQL
	for _, fragment := range []string{
		"users_created_by_fkey",
		"audit_logs_actor_user_id_fkey",
		"audit_logs_actor_session_id_fkey",
		"ON DELETE SET NULL",
	} {
		if !strings.Contains(deleteConstraintsSQL, fragment) {
			t.Fatalf("identity delete constraints migration missing %q", fragment)
		}
	}

	ccSwitchConstraintsSQL := migrations[6].SQL
	if !strings.Contains(ccSwitchConstraintsSQL, "ADD PRIMARY KEY (tenant_id, id)") {
		t.Fatal("ccswitch tenant primary key migration is missing composite primary key")
	}

	menuSQL := migrations[7].SQL
	for _, fragment := range []string{"CREATE TABLE IF NOT EXISTS menus", "permission_code", "idx_menus_parent_sort"} {
		if !strings.Contains(menuSQL, fragment) {
			t.Fatalf("dynamic menu migration missing %q", fragment)
		}
	}

	menuV2SQL := migrations[8].SQL
	for _, fragment := range []string{"button", "component", "link_url", "hide_menu", "menus_menu_type_check"} {
		if !strings.Contains(menuV2SQL, fragment) {
			t.Fatalf("menu management v2 migration missing %q", fragment)
		}
	}

	identitySQL := migrations[2].SQL
	for _, table := range []string{"tenants", "users", "roles", "permissions", "role_permissions", "user_roles", "user_sessions", "audit_logs"} {
		if !strings.Contains(identitySQL, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Fatalf("identity migration does not create %s", table)
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

func TestApplyMigrationDirtyErrorIncludesRemediation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close sqlite db: %v", err)
		}
	})
	if _, err := db.Exec(`
		CREATE TABLE schema_migrations (
			version TEXT PRIMARY KEY,
			checksum TEXT NOT NULL,
			dirty BOOLEAN NOT NULL
		)
	`); err != nil {
		t.Fatal(err)
	}
	migration := Migration{Version: "202607210001_slow_ddl", SQL: "CREATE TABLE example (id INTEGER)"}
	if _, err := db.Exec(
		"INSERT INTO schema_migrations (version, checksum, dirty) VALUES (?, ?, true)",
		migration.Version,
		migrationChecksum(migration.SQL),
	); err != nil {
		t.Fatal(err)
	}

	err = applyMigration(context.Background(), db, migration)
	if err == nil {
		t.Fatal("applyMigration() error = nil, want dirty migration error")
	}
	for _, fragment := range []string{
		"migration 202607210001_slow_ddl is dirty",
		"confirm whether the SQL committed",
		"do not only clear dirty",
		"do not automatically replay SQL",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("applyMigration() error = %q, want %q", err, fragment)
		}
	}
}
