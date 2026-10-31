package contentmoderation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

func savedWordPolicy(t *testing.T, store *Store, tenantID string, p WordPolicy, expected *int64) WordPolicy {
	t.Helper()
	p.TenantID = tenantID
	if err := p.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	saved, err := store.PutWordPolicy(context.Background(), p, expected)
	if err != nil {
		t.Fatalf("PutWordPolicy: %v", err)
	}
	return saved
}

func int64Ptr(v int64) *int64 { return &v }

// runWordPolicyStoreChecks holds the store to the same behaviour on SQLite and
// on PostgreSQL.
func runWordPolicyStoreChecks(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("round trip", func(t *testing.T) {
		in := WordPolicy{
			ScopeType: WordScopeTenant, Enabled: true, Action: WordActionReplace, Replacement: "[违禁]", ScanScope: WordScanWithSystem,
			ReplyFilter: boolPtr(true), FoldTraditionalChinese: boolPtr(false), InheritTenant: true, BlockMessage: "nope\nnot here",
			Rules: []WordRule{kw("违禁词"), rx(`\d{11}`), {Kind: WordRuleKeyword, Pattern: "off", Enabled: false, Note: "paused"}},
		}
		saved := savedWordPolicy(t, store, "tenant-rt", in, nil)
		if saved.Version != 1 || saved.CreatedAt.IsZero() {
			t.Fatalf("saved = %+v", saved)
		}
		got, err := store.GetWordPolicy(ctx, "tenant-rt", WordScopeTenant, "")
		if err != nil {
			t.Fatalf("GetWordPolicy: %v", err)
		}
		if got.Action != WordActionReplace || got.Replacement != "[违禁]" || got.ScanScope != WordScanWithSystem || got.BlockMessage != "nope\nnot here" {
			t.Fatalf("settings = %+v", got)
		}
		if got.ReplyFilter == nil || !*got.ReplyFilter || got.FoldTraditionalChinese == nil || *got.FoldTraditionalChinese {
			t.Fatalf("tri-state settings = %v / %v", got.ReplyFilter, got.FoldTraditionalChinese)
		}
		if len(got.Rules) != 3 || got.Rules[0].Pattern != "违禁词" || got.Rules[1].Kind != WordRuleRegex || got.Rules[2].Enabled || got.Rules[2].Note != "paused" {
			t.Fatalf("rules = %+v", got.Rules)
		}
		if got.Rules[0].ID == "" || got.Rules[0].ID != saved.Rules[0].ID {
			t.Fatalf("rule ids must survive a round trip: %+v vs %+v", got.Rules, saved.Rules)
		}
	})

	t.Run("unset settings stay unset", func(t *testing.T) {
		savedWordPolicy(t, store, "tenant-unset", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, InheritTenant: true, Rules: []WordRule{kw("x")}}, nil)
		got, err := store.GetWordPolicy(ctx, "tenant-unset", WordScopeTenant, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Action != "" || got.Replacement != "" || got.ScanScope != "" || got.ReplyFilter != nil || got.FoldTraditionalChinese != nil {
			t.Fatalf("policy = %+v: unset settings must read back unset so they inherit", got)
		}
	})

	t.Run("versions", func(t *testing.T) {
		first := savedWordPolicy(t, store, "tenant-v", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("one")}}, nil)
		second := savedWordPolicy(t, store, "tenant-v", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("two")}}, int64Ptr(first.Version))
		if second.Version != first.Version+1 || !second.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("first %+v second %+v", first, second)
		}
		stale := WordPolicy{TenantID: "tenant-v", ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("three")}}
		if _, err := store.PutWordPolicy(ctx, stale, int64Ptr(first.Version)); !errors.Is(err, ErrWordPolicyVersionConflict) {
			t.Fatalf("stale save = %v, want a version conflict", err)
		}
		if _, err := store.PutWordPolicy(ctx, stale, int64Ptr(0)); !errors.Is(err, ErrWordPolicyVersionConflict) {
			t.Fatalf("create over an existing policy = %v, want a version conflict", err)
		}
		// Without a version the save is unconditional.
		third := savedWordPolicy(t, store, "tenant-v", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("three")}}, nil)
		if third.Version != second.Version+1 {
			t.Fatalf("third = %+v", third)
		}
		if _, err := store.PutWordPolicy(ctx, WordPolicy{TenantID: "tenant-new", ScopeType: WordScopeTenant}, int64Ptr(5)); !errors.Is(err, ErrWordPolicyVersionConflict) {
			t.Fatalf("update of a policy that does not exist = %v, want a version conflict", err)
		}
	})

	t.Run("not found and delete", func(t *testing.T) {
		if _, err := store.GetWordPolicy(ctx, "tenant-none", WordScopeTenant, ""); !errors.Is(err, ErrWordPolicyNotFound) {
			t.Fatalf("Get = %v", err)
		}
		if err := store.DeleteWordPolicy(ctx, "tenant-none", WordScopeTenant, ""); !errors.Is(err, ErrWordPolicyNotFound) {
			t.Fatalf("Delete = %v", err)
		}
		savedWordPolicy(t, store, "tenant-del", WordPolicy{ScopeType: WordScopeTenant, Enabled: true}, nil)
		if err := store.DeleteWordPolicy(ctx, "tenant-del", WordScopeTenant, ""); err != nil {
			t.Fatalf("Delete = %v", err)
		}
		if _, err := store.GetWordPolicy(ctx, "tenant-del", WordScopeTenant, ""); !errors.Is(err, ErrWordPolicyNotFound) {
			t.Fatalf("Get after delete = %v", err)
		}
	})

	t.Run("tenants cannot see each other's policies", func(t *testing.T) {
		savedWordPolicy(t, store, "tenant-a", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("a-word")}}, nil)
		savedWordPolicy(t, store, "tenant-a", WordPolicy{ScopeType: WordScopeKey, ScopeID: "key-1", Enabled: true, Rules: []WordRule{kw("a-key-word")}}, nil)
		savedWordPolicy(t, store, "tenant-b", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("b-word")}}, nil)
		if _, err := store.GetWordPolicy(ctx, "tenant-b", WordScopeKey, "key-1"); !errors.Is(err, ErrWordPolicyNotFound) {
			t.Fatalf("tenant b read tenant a's key policy: %v", err)
		}
		// The listing holds the platform policy, the tenant's own and its keys', and
		// nobody else's.
		savedWordPolicy(t, store, platformWordTenantID(), WordPolicy{ScopeType: WordScopePlatform, Enabled: true, Action: WordActionLog, Rules: []WordRule{kw("p1"), kw("p2")}}, nil)
		scopes := func(list []WordPolicySummary) string {
			var out []string
			for _, item := range list {
				out = append(out, item.ScopeType+":"+item.ScopeID)
			}
			sort.Strings(out)
			return strings.Join(out, ",")
		}
		list, err := store.ListWordPolicySummaries(ctx, "tenant-b")
		if err != nil || scopes(list) != "platform:,tenant:" {
			t.Fatalf("tenant b policies = %+v, %v", list, err)
		}
		list, err = store.ListWordPolicySummaries(ctx, "tenant-a")
		if err != nil || scopes(list) != "key:key-1,platform:,tenant:" {
			t.Fatalf("tenant a policies = %+v, %v", list, err)
		}
		for _, item := range list {
			switch item.ScopeType {
			case WordScopeKey:
				if item.RuleCount != 1 || !item.Enabled || item.ReplyFilter != nil {
					t.Fatalf("tenant a key policy = %+v", item)
				}
			case WordScopePlatform:
				if item.RuleCount != 2 || item.Action != WordActionLog {
					t.Fatalf("platform policy = %+v", item)
				}
			}
		}
		if list, err = store.ListWordPolicySummaries(ctx, "tenant-nobody"); err != nil || scopes(list) != "platform:" {
			t.Fatalf("a tenant without policies sees %+v, %v", list, err)
		}
		// In the system tenant the platform policy and the tenant policy are two
		// rows of one tenant; each is listed once.
		savedWordPolicy(t, store, platformWordTenantID(), WordPolicy{ScopeType: WordScopeTenant, Enabled: true, ReplyFilter: boolPtr(true), Rules: []WordRule{kw("s1")}}, nil)
		list, err = store.ListWordPolicySummaries(ctx, platformWordTenantID())
		if err != nil || scopes(list) != "platform:,tenant:" {
			t.Fatalf("system tenant policies = %+v, %v", list, err)
		}
		for _, item := range list {
			if item.ScopeType == WordScopeTenant && (item.ReplyFilter == nil || !*item.ReplyFilter) {
				t.Fatalf("the tenant policy lost its reply filter setting: %+v", item)
			}
		}
	})

	t.Run("load picks the policies of the request", func(t *testing.T) {
		platform := savedWordPolicy(t, store, platformWordTenantID(), WordPolicy{ScopeType: WordScopePlatform, Enabled: true, Rules: []WordRule{kw("platform-word")}}, nil)
		savedWordPolicy(t, store, "tenant-l", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("tenant-word")}}, nil)
		savedWordPolicy(t, store, "tenant-l", WordPolicy{ScopeType: WordScopeKey, ScopeID: "k-1", Enabled: true, Rules: []WordRule{kw("key-word")}}, nil)
		savedWordPolicy(t, store, "tenant-l", WordPolicy{ScopeType: WordScopeKey, ScopeID: "k-2", Enabled: true, Rules: []WordRule{kw("other-key-word")}}, nil)
		savedWordPolicy(t, store, "tenant-other", WordPolicy{ScopeType: WordScopeKey, ScopeID: "k-1", Enabled: true, Rules: []WordRule{kw("foreign-word")}}, nil)

		got, err := store.LoadWordPolicies(ctx, "tenant-l", "k-1")
		if err != nil {
			t.Fatalf("LoadWordPolicies: %v", err)
		}
		if got.Platform == nil || got.Platform.Version != platform.Version || got.Platform.Rules[0].Pattern != "platform-word" {
			t.Fatalf("platform = %+v", got.Platform)
		}
		if got.Tenant == nil || got.Tenant.Rules[0].Pattern != "tenant-word" {
			t.Fatalf("tenant = %+v", got.Tenant)
		}
		if got.Key == nil || got.Key.Rules[0].Pattern != "key-word" {
			t.Fatalf("key = %+v: another tenant's or another key's policy must not be returned", got.Key)
		}
		// A request made without a key id, or with a key that has no policy.
		for _, keyID := range []string{"", "k-none"} {
			got, err = store.LoadWordPolicies(ctx, "tenant-l", keyID)
			if err != nil || got.Key != nil || got.Tenant == nil || got.Platform == nil {
				t.Fatalf("key %q: %+v, %v", keyID, got, err)
			}
		}
		// The platform policy applies to a tenant with no policy of its own.
		got, err = store.LoadWordPolicies(ctx, "tenant-fresh", "k-9")
		if err != nil || got.Platform == nil || got.Tenant != nil || got.Key != nil {
			t.Fatalf("fresh tenant: %+v, %v", got, err)
		}
	})

	t.Run("load follows saves and deletes", func(t *testing.T) {
		if _, err := store.LoadWordPolicies(ctx, "tenant-f", ""); err != nil {
			t.Fatal(err)
		}
		saved := savedWordPolicy(t, store, "tenant-f", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("first")}}, nil)
		got, _ := store.LoadWordPolicies(ctx, "tenant-f", "")
		if got.Tenant == nil || got.Tenant.Rules[0].Pattern != "first" {
			t.Fatalf("tenant = %+v", got.Tenant)
		}
		savedWordPolicy(t, store, "tenant-f", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("second")}}, int64Ptr(saved.Version))
		got, _ = store.LoadWordPolicies(ctx, "tenant-f", "")
		if got.Tenant == nil || got.Tenant.Rules[0].Pattern != "second" {
			t.Fatalf("a save must show up on the next request: %+v", got.Tenant)
		}
		if err := store.DeleteWordPolicy(ctx, "tenant-f", WordScopeTenant, ""); err != nil {
			t.Fatal(err)
		}
		got, _ = store.LoadWordPolicies(ctx, "tenant-f", "")
		if got.Tenant != nil {
			t.Fatalf("a deleted policy must stop applying: %+v", got.Tenant)
		}
	})

	// A policy that is deleted and created again starts over at version 1. A copy
	// cached before must not be mistaken for it.
	t.Run("a recreated policy is not served from a stale copy", func(t *testing.T) {
		first := savedWordPolicy(t, store, "tenant-r", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("old")}}, nil)
		if got, _ := store.LoadWordPolicies(ctx, "tenant-r", ""); got.Tenant == nil || got.Tenant.Rules[0].Pattern != "old" {
			t.Fatalf("tenant = %+v", got.Tenant)
		}
		if err := NewStore(store.db).DeleteWordPolicy(ctx, "tenant-r", WordScopeTenant, ""); err != nil {
			t.Fatal(err)
		}
		second := savedWordPolicy(t, NewStore(store.db), "tenant-r", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("new")}}, nil)
		if second.Version != first.Version {
			t.Fatalf("the test needs the same version twice: %d and %d", first.Version, second.Version)
		}
		got, _ := store.LoadWordPolicies(ctx, "tenant-r", "")
		if got.Tenant == nil || got.Tenant.Rules[0].Pattern != "new" {
			t.Fatalf("the cached copy of the deleted policy was served: %+v", got.Tenant)
		}
	})
}

func TestWordPolicyStoreSQLite(t *testing.T) {
	runWordPolicyStoreChecks(t, newTestStore(t))
}

func TestWordPolicyStorePostgres(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	db := scopedPostgres(t, dsn, "cm_words")
	if err := InitTables(db); err != nil {
		t.Fatalf("InitTables: %v", err)
	}
	runWordPolicyStoreChecks(t, NewStore(db))
}

// scopedPostgres opens a connection whose search_path is a schema of its own,
// so the test neither sees nor disturbs the shared runtime tables.
func scopedPostgres(t *testing.T, dsn, prefix string) *sql.DB {
	t.Helper()
	admin, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
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
	return db
}

// The upgrade only adds a table. Everything a database held before keeps
// working, and running it again changes nothing.
func TestInitTablesAddsWordPolicyTablesWithoutTouchingProfiles(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "previous.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.Exec(previousReleaseProfilesTableSQL); err != nil {
		t.Fatalf("create previous table: %v", err)
	}
	if _, err = db.Exec(previousReleaseProfileRow); err != nil {
		t.Fatalf("seed previous row: %v", err)
	}
	if err = InitTables(db); err != nil {
		t.Fatalf("InitTables: %v", err)
	}
	if err = InitTables(db); err != nil {
		t.Fatalf("InitTables again: %v", err)
	}
	requireMigratedPreviousReleaseProfile(t, db)
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM content_moderation_word_policies`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("word policy table: count %d, err %v", count, err)
	}
	store := NewStore(db)
	runWordPolicyStoreChecks(t, store)
}

func TestInitTablesAddsWordPolicyTablesPostgres(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	db := scopedPostgres(t, dsn, "cm_words_up")
	if _, err := db.Exec(previousReleaseProfilesTableSQL); err != nil {
		t.Fatalf("create previous table: %v", err)
	}
	if _, err := db.Exec(previousReleaseProfileRow); err != nil {
		t.Fatalf("seed previous row: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := InitTables(db); err != nil {
			t.Fatalf("InitTables #%d: %v", i+1, err)
		}
	}
	requireMigratedPreviousReleaseProfile(t, db)
	var columns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'content_moderation_word_policies'`).Scan(&columns); err != nil || columns < 15 {
		t.Fatalf("word policy columns = %d, err %v", columns, err)
	}
}

// A database that stops answering must not switch the guard off for requests
// it already knew how to judge.
func TestLoadWordPoliciesFallsBackToWhatItLastSaw(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "outage.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err = InitTables(db); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db)
	savedWordPolicy(t, store, "tenant-o", WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: []WordRule{kw("remembered")}}, nil)
	if got, err := store.LoadWordPolicies(context.Background(), "tenant-o", ""); err != nil || got.Tenant == nil {
		t.Fatalf("first load: %+v, %v", got, err)
	}
	_ = db.Close()
	got, err := store.LoadWordPolicies(context.Background(), "tenant-o", "")
	if err == nil {
		t.Fatalf("a closed database must report an error")
	}
	if got.Tenant == nil || got.Tenant.Rules[0].Pattern != "remembered" {
		t.Fatalf("the last known policy must still be returned: %+v", got)
	}
	// A tenant it never saw is simply unguarded.
	if got, _ = store.LoadWordPolicies(context.Background(), "tenant-unknown", ""); got.Tenant != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestWordPolicyStoreRejectsNilStore(t *testing.T) {
	var store *Store
	if _, err := store.LoadWordPolicies(context.Background(), "t", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := store.GetWordPolicy(context.Background(), "t", WordScopeTenant, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
