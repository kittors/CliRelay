package contentmoderation

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
	_ "modernc.org/sqlite"
)

// previousReleaseProfilesTableSQL is the profiles table as the release before
// the text-coverage options shipped it: qwen3guard columns, no option columns.
const previousReleaseProfilesTableSQL = `
CREATE TABLE content_moderation_profiles (
  tenant_id TEXT NOT NULL,
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT 'off',
  backend TEXT NOT NULL DEFAULT 'openai_moderations',
  base_url TEXT NOT NULL DEFAULT 'https://api.openai.com',
  model TEXT NOT NULL DEFAULT 'omni-moderation-latest',
  api_key_secret TEXT NOT NULL DEFAULT '',
  timeout_ms INTEGER NOT NULL DEFAULT 3000,
  keyword_mode TEXT NOT NULL DEFAULT 'api_only',
  blocked_keywords_json TEXT NOT NULL DEFAULT '[]',
  thresholds_json TEXT NOT NULL DEFAULT '{}',
  scanners_json TEXT NOT NULL DEFAULT '[]',
  controversial_action TEXT NOT NULL DEFAULT 'elevated_only',
  elevated_categories_json TEXT NOT NULL DEFAULT '["jailbreak","pii","suicide_and_self_harm"]',
  input_limit INTEGER NOT NULL DEFAULT 4000,
  max_chunks INTEGER NOT NULL DEFAULT 4,
  block_http_status INTEGER NOT NULL DEFAULT 403,
  block_message TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, name)
);`

const previousReleaseProfileRow = `INSERT INTO content_moderation_profiles
	(tenant_id,id,name,mode,backend,base_url,model,api_key_secret,timeout_ms,keyword_mode,blocked_keywords_json,thresholds_json,
	 scanners_json,controversial_action,elevated_categories_json,input_limit,max_chunks,block_http_status,block_message,version,created_at,updated_at)
	VALUES ('tenant-a','guard-1','guard','pre_block','qwen3guard','http://guard.internal:8000','qwen3guard','',3000,'keyword_and_api','["bad"]','{}',
	 '["pii"]','block','["jailbreak"]',2000,2,451,'blocked',5,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`

func TestNewProfileTextCoverageOptionsDefaultOff(t *testing.T) {
	profile, err := NewProfile("tenant-a", "profile-1", CreateProfileInput{Name: "defaults"}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if profile.APIIncludesToolOutput || profile.KeywordFoldTraditionalChinese {
		t.Fatalf("options = %v/%v, want both off", profile.APIIncludesToolOutput, profile.KeywordFoldTraditionalChinese)
	}
}

// requireMigratedPreviousReleaseProfile checks a profile stored by the previous
// release after InitTables: options off, everything else as stored.
func requireMigratedPreviousReleaseProfile(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := InitTables(db); err != nil {
		t.Fatalf("InitTables: %v", err)
	}
	if err := InitTables(db); err != nil {
		t.Fatalf("InitTables is not idempotent: %v", err)
	}
	store := NewStore(db)
	ctx := context.Background()
	profile, err := store.GetProfile(ctx, "tenant-a", "guard-1")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if profile.APIIncludesToolOutput || profile.KeywordFoldTraditionalChinese {
		t.Fatalf("migrated options = %v/%v, want both off", profile.APIIncludesToolOutput, profile.KeywordFoldTraditionalChinese)
	}
	if profile.Version != 5 || profile.Backend != BackendQwen3Guard || profile.KeywordMode != KeywordModeKeywordAndAPI ||
		profile.InputLimit != 2000 || profile.MaxChunks != 2 || profile.BlockHTTPStatus != 451 || len(profile.BlockedKeywords) != 1 {
		t.Fatalf("stored values changed in migration: %#v", profile)
	}

	// An update that omits the options leaves them as they are; one that sets
	// them is persisted.
	untouched, err := ApplyProfilePatch(profile, PatchProfileInput{Name: strPtr("renamed"), Version: profile.Version}, time.Now())
	if err != nil {
		t.Fatalf("ApplyProfilePatch: %v", err)
	}
	if err = store.UpdateProfile(ctx, untouched, profile.Version); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	on := true
	enabled, err := ApplyProfilePatch(untouched, PatchProfileInput{APIIncludesToolOutput: &on, KeywordFoldTraditionalChinese: &on, Version: untouched.Version}, time.Now())
	if err != nil {
		t.Fatalf("ApplyProfilePatch: %v", err)
	}
	if err = store.UpdateProfile(ctx, enabled, untouched.Version); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	reloaded, err := store.GetProfile(ctx, "tenant-a", "guard-1")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !reloaded.APIIncludesToolOutput || !reloaded.KeywordFoldTraditionalChinese || reloaded.Name != "renamed" {
		t.Fatalf("reloaded = %#v", reloaded)
	}
}

func TestInitTablesAddsTextCoverageOptionsToPreviousReleaseSQLite(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "previous.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.Exec(previousReleaseProfilesTableSQL); err != nil {
		t.Fatalf("create previous table: %v", err)
	}
	if _, err = db.Exec(previousReleaseProfileRow); err != nil {
		t.Fatalf("seed previous row: %v", err)
	}
	requireMigratedPreviousReleaseProfile(t, db)
}

// The same migration on PostgreSQL, in a schema of its own so it neither sees
// nor disturbs the shared runtime tables other tests use.
func TestInitTablesAddsTextCoverageOptionsToPreviousReleasePostgres(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	admin, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("cm_options_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	scoped, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	query := scoped.Query()
	query.Set("search_path", schema)
	scoped.RawQuery = query.Encode()
	db, err := sql.Open(compatdriver.DriverName, scoped.String())
	if err != nil {
		t.Fatalf("open scoped postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.Exec(previousReleaseProfilesTableSQL); err != nil {
		t.Fatalf("create previous table: %v", err)
	}
	if _, err = db.Exec(previousReleaseProfileRow); err != nil {
		t.Fatalf("seed previous row: %v", err)
	}
	requireMigratedPreviousReleaseProfile(t, db)
	var columns int
	if err = db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'content_moderation_profiles'
		AND column_name IN ('keyword_fold_traditional_chinese','api_includes_tool_output')`, schema).Scan(&columns); err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	if columns != 2 {
		t.Fatalf("option columns in %s = %d, want 2", schema, columns)
	}
}

func TestStoreRoundTripsTextCoverageOptions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	profile, err := NewProfile("tenant-a", "options-1", CreateProfileInput{
		Name:                          "options",
		APIIncludesToolOutput:         true,
		KeywordFoldTraditionalChinese: true,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err = store.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	stored, err := store.GetProfile(ctx, "tenant-a", "options-1")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !stored.APIIncludesToolOutput || !stored.KeywordFoldTraditionalChinese {
		t.Fatalf("stored = %#v", stored)
	}
	off := false
	updated, err := ApplyProfilePatch(stored, PatchProfileInput{APIIncludesToolOutput: &off, Version: stored.Version}, time.Now())
	if err != nil {
		t.Fatalf("ApplyProfilePatch: %v", err)
	}
	if err = store.UpdateProfile(ctx, updated, stored.Version); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	reloaded, err := store.GetProfile(ctx, "tenant-a", "options-1")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if reloaded.APIIncludesToolOutput || !reloaded.KeywordFoldTraditionalChinese {
		t.Fatalf("reloaded = %#v, want only api_includes_tool_output switched off", reloaded)
	}
	// The runtime reads profiles through channel bindings; the options must come along.
	bindRuntimeChannel(t, store, "tenant-a", ChannelTypeProviderKey, "key-1", reloaded.ID)
	resolved, _, err := store.ResolveProfile(ctx, "tenant-a", "", "key-1", "")
	if err != nil || resolved.APIIncludesToolOutput || !resolved.KeywordFoldTraditionalChinese {
		t.Fatalf("ResolveProfile = %#v, %v", resolved, err)
	}
}
