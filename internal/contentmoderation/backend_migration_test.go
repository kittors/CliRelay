package contentmoderation

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// legacyProfilesTableSQL is the schema as it shipped before qwen3guard support.
const legacyProfilesTableSQL = `
CREATE TABLE content_moderation_profiles (
  tenant_id TEXT NOT NULL,
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT 'off',
  base_url TEXT NOT NULL DEFAULT 'https://api.openai.com',
  model TEXT NOT NULL DEFAULT 'omni-moderation-latest',
  api_key_secret TEXT NOT NULL DEFAULT '',
  timeout_ms INTEGER NOT NULL DEFAULT 3000,
  keyword_mode TEXT NOT NULL DEFAULT 'api_only',
  blocked_keywords_json TEXT NOT NULL DEFAULT '[]',
  thresholds_json TEXT NOT NULL DEFAULT '{}',
  block_http_status INTEGER NOT NULL DEFAULT 403,
  block_message TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, name)
);`

// A profile written by the previous release must keep behaving exactly as it
// did: OpenAI moderations, same endpoint, same thresholds.
func TestInitTablesMigratesLegacyProfilesToOpenAIBackend(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.Exec(legacyProfilesTableSQL); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err = db.Exec(`INSERT INTO content_moderation_profiles
		(tenant_id,id,name,mode,base_url,model,api_key_secret,timeout_ms,keyword_mode,blocked_keywords_json,thresholds_json,block_http_status,block_message,version,created_at,updated_at)
		VALUES ('tenant-a','legacy-1','legacy','pre_block','https://api.openai.com','omni-moderation-latest','sk-legacy',3000,'api_only','["bad"]','{"hate":0.5}',403,'blocked',7,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if err = InitTables(db); err != nil {
		t.Fatalf("InitTables: %v", err)
	}
	// Re-running must stay a no-op; duplicate-column errors are expected and ignored.
	if err = InitTables(db); err != nil {
		t.Fatalf("InitTables is not idempotent: %v", err)
	}

	profile, err := NewStore(db).GetProfile(context.Background(), "tenant-a", "legacy-1")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if profile.Backend != BackendOpenAIModerations {
		t.Fatalf("backend = %q, want the pre-migration behaviour", profile.Backend)
	}
	if profile.ControversialAction != ControversialActionElevatedOnly {
		t.Fatalf("controversial_action = %q", profile.ControversialAction)
	}
	if profile.InputLimit != DefaultInputLimit || profile.MaxChunks != DefaultMaxChunks {
		t.Fatalf("chunking defaults = %d/%d", profile.InputLimit, profile.MaxChunks)
	}
	if len(profile.Scanners) != 0 {
		t.Fatalf("scanners = %v, want empty", profile.Scanners)
	}
	if len(profile.ElevatedCategories) != len(DefaultElevatedCategories()) {
		t.Fatalf("elevated categories = %v", profile.ElevatedCategories)
	}
	// Untouched fields must survive the migration verbatim.
	if profile.Version != 7 || profile.APIKeySecret != "sk-legacy" || profile.Thresholds["hate"] != 0.5 {
		t.Fatalf("legacy values changed: %#v", profile)
	}

	// And the runtime path must still speak the moderations protocol.
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"hate":0.9}}]}`))
	}))
	defer server.Close()
	profile.BaseURL = server.URL
	decision := NewEvaluator(server.Client()).Evaluate(context.Background(), profile, "input")
	if path != "/v1/moderations" {
		t.Fatalf("legacy profile called %q", path)
	}
	if !decision.WouldBlock || decision.Action != ActionAPIBlock {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.Safety != "" || len(decision.Categories) != 0 {
		t.Fatalf("guard-only fields leaked into an OpenAI decision: %#v", decision)
	}
}

func TestStoreRoundTripsBackendFields(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	profile, err := NewProfile("tenant-a", "guard-1", CreateProfileInput{
		Name:                "guard",
		Mode:                ModePreBlock,
		Backend:             BackendQwen3Guard,
		BaseURL:             "http://guard.internal:8000",
		Model:               "qwen3guard",
		KeywordMode:         KeywordModeAPIOnly,
		Scanners:            []string{ScannerJailbreak, ScannerPII},
		ControversialAction: ControversialActionBlock,
		ElevatedCategories:  []string{ScannerJailbreak},
		InputLimit:          2000,
		MaxChunks:           2,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err = store.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	stored, err := store.GetProfile(ctx, "tenant-a", "guard-1")
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if stored.Backend != BackendQwen3Guard || stored.ControversialAction != ControversialActionBlock {
		t.Fatalf("stored = %#v", stored)
	}
	if stored.InputLimit != 2000 || stored.MaxChunks != 2 {
		t.Fatalf("chunking = %d/%d", stored.InputLimit, stored.MaxChunks)
	}
	if len(stored.Scanners) != 2 || stored.Scanners[0] != ScannerPII || stored.Scanners[1] != ScannerJailbreak {
		t.Fatalf("scanners = %v, want catalog order", stored.Scanners)
	}
	if len(stored.ElevatedCategories) != 1 || stored.ElevatedCategories[0] != ScannerJailbreak {
		t.Fatalf("elevated = %v", stored.ElevatedCategories)
	}

	updated, err := ApplyProfilePatch(stored, PatchProfileInput{
		Scanners:            &[]string{ScannerViolent},
		ControversialAction: strPtr(ControversialActionAllow),
		Version:             stored.Version,
	}, time.Now())
	if err != nil {
		t.Fatalf("ApplyProfilePatch: %v", err)
	}
	if err = store.UpdateProfile(ctx, updated, stored.Version); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	reloaded, err := store.GetProfile(ctx, "tenant-a", "guard-1")
	if err != nil {
		t.Fatalf("GetProfile after patch: %v", err)
	}
	if len(reloaded.Scanners) != 1 || reloaded.Scanners[0] != ScannerViolent {
		t.Fatalf("scanners = %v", reloaded.Scanners)
	}
	if reloaded.ControversialAction != ControversialActionAllow {
		t.Fatalf("controversial_action = %q", reloaded.ControversialAction)
	}
}

// An explicitly empty elevated list means "never escalate" and must not be
// silently refilled with the recommended defaults.
func TestExplicitlyEmptyElevatedCategoriesSurvive(t *testing.T) {
	profile, err := NewProfile("tenant-a", "guard-2", CreateProfileInput{
		Name:               "guard",
		Mode:               ModeOff,
		Backend:            BackendQwen3Guard,
		ElevatedCategories: []string{},
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if len(profile.ElevatedCategories) != 0 {
		t.Fatalf("elevated = %v, want empty", profile.ElevatedCategories)
	}
	omitted, err := NewProfile("tenant-a", "guard-3", CreateProfileInput{
		Name:    "guard-default",
		Mode:    ModeOff,
		Backend: BackendQwen3Guard,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if len(omitted.ElevatedCategories) != len(DefaultElevatedCategories()) {
		t.Fatalf("elevated = %v, want recommended defaults", omitted.ElevatedCategories)
	}
}

func strPtr(value string) *string { return &value }
