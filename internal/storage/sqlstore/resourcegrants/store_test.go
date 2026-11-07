package resourcegrants

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Every connection to :memory: is a new database; keep the one with the table.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)
	return db
}

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

func TestReplaceSetsGranteesPerResource(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if _, err := store.Replace(ctx, KindProxy, []string{"hk-1", "sg-1"}, []string{tenantB, tenantA, tenantA}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replace(ctx, KindAuth, []string{"codex-a.json"}, []string{AllTenants, tenantA}); err != nil {
		t.Fatal(err)
	}

	proxies, err := store.List(ctx, KindProxy)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{tenantA, tenantB}
	if !slices.Equal(proxies["hk-1"], want) || !slices.Equal(proxies["sg-1"], want) {
		t.Fatalf("proxy grants = %v, want both resources granted to %v", proxies, want)
	}
	auths, err := store.List(ctx, KindAuth)
	if err != nil {
		t.Fatal(err)
	}
	// "*" absorbs explicit tenants: the stored grant is just "everybody".
	if !slices.Equal(auths["codex-a.json"], []string{AllTenants}) {
		t.Fatalf("auth grants = %v, want [*]", auths)
	}
	if _, ok := proxies["codex-a.json"]; ok {
		t.Fatal("kinds must not leak into each other")
	}

	// An empty grantee list stops sharing.
	if _, err := store.Replace(ctx, KindProxy, []string{"hk-1"}, nil); err != nil {
		t.Fatal(err)
	}
	proxies, _ = store.List(ctx, KindProxy)
	if _, ok := proxies["hk-1"]; ok {
		t.Fatalf("hk-1 still granted after unsharing: %v", proxies)
	}
	if !slices.Equal(proxies["sg-1"], want) {
		t.Fatalf("unsharing hk-1 changed sg-1: %v", proxies["sg-1"])
	}
	if version := configsync.CollectionVersion(ctx, db, configsync.DomainResourceGrants, ""); version != 3 {
		t.Fatalf("grants collection version = %d, want 3 after three writes", version)
	}
}

func TestReplaceRejectsUnknownKindAndEmptyIDs(t *testing.T) {
	store := NewStore(openTestDB(t))
	if _, err := store.Replace(context.Background(), "endpoint", []string{"x"}, []string{tenantA}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := store.Replace(context.Background(), KindProxy, []string{" ", ""}, []string{tenantA}); err == nil {
		t.Fatal("blank ids accepted")
	}
}

func TestRetainTxDropsGrantsOfRemovedResources(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()
	if _, err := store.Replace(ctx, KindProxy, []string{"keep", "gone"}, []string{tenantA}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replace(ctx, KindAuth, []string{"gone"}, []string{tenantA}); err != nil {
		t.Fatal(err)
	}
	before := configsync.CollectionVersion(ctx, db, configsync.DomainResourceGrants, "")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := RetainTx(ctx, tx, KindProxy, []string{"keep", "never-granted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("RetainTx reported no change after dropping a grant")
	}
	proxies, _ := store.List(ctx, KindProxy)
	if _, ok := proxies["gone"]; ok || len(proxies["keep"]) != 1 {
		t.Fatalf("proxy grants after retain = %v", proxies)
	}
	auths, _ := store.List(ctx, KindAuth)
	if len(auths["gone"]) != 1 {
		t.Fatalf("retaining proxies touched auth grants: %v", auths)
	}
	if after := configsync.CollectionVersion(ctx, db, configsync.DomainResourceGrants, ""); after != before+1 {
		t.Fatalf("grants version %d -> %d, want one bump", before, after)
	}

	// Nothing stale: no write, no version bump.
	tx, _ = db.BeginTx(ctx, nil)
	changed, err = RetainTx(ctx, tx, KindProxy, []string{"keep"})
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	if changed {
		t.Fatal("RetainTx reported a change with nothing to drop")
	}
	if again := configsync.CollectionVersion(ctx, db, configsync.DomainResourceGrants, ""); again != before+1 {
		t.Fatalf("grants version moved to %d without a change", again)
	}
}

func TestAllowsAndNormalizeTenants(t *testing.T) {
	if !Allows([]string{AllTenants}, tenantA) {
		t.Fatal("* must allow every tenant")
	}
	if !Allows([]string{tenantA}, tenantA) || Allows([]string{tenantA}, tenantB) {
		t.Fatal("explicit grant must allow only its tenant")
	}
	if Allows([]string{AllTenants}, "") {
		t.Fatal("an empty tenant must never be allowed")
	}
	if got := NormalizeTenants([]string{" b ", "a", "b", ""}); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("NormalizeTenants = %v", got)
	}
}
