package modelconfig

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

const systemTenant = "00000000-0000-0000-0000-000000000001"

func newSeededTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	InitTables(db)
	return db
}

func modelRowOwner(t *testing.T, db *sql.DB, tenantID, modelID string) (string, bool) {
	t.Helper()
	var ownedBy string
	err := db.QueryRow(
		`SELECT owned_by FROM model_configs WHERE tenant_id = ? AND model_id = ?`,
		tenantID, modelID,
	).Scan(&ownedBy)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("query %s/%s: %v", tenantID, modelID, err)
	}
	return ownedBy, true
}

// TestMediaModelsReachEveryTenant reproduces the gap that made gpt-image-2.5
// invisible: seeding only ever wrote the system tenant, and every other tenant got
// its image models as a side effect of pricing import. A model too new to appear in
// a pricing feed therefore reached no tenant but the system one.
func TestMediaModelsReachEveryTenant(t *testing.T) {
	db := newSeededTestDB(t)

	// A tenant that predates the release and already carries the older model.
	const tenant = "9e003dfb-751f-4898-b186-45f765c763a6"
	if _, err := db.Exec(
		`INSERT INTO model_configs (tenant_id, model_id, owned_by, enabled, source, updated_at)
		 VALUES (?, 'gpt-image-2', 'codex', 1, 'seed', '2026-01-01T00:00:00Z')`,
		tenant,
	); err != nil {
		t.Fatalf("seed existing tenant row: %v", err)
	}

	seedMediaGenerationModelsForTenants(db)

	for _, modelID := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		owner, ok := modelRowOwner(t, db, tenant, modelID)
		if !ok {
			t.Fatalf("%s is missing from tenant %s, so it cannot appear in that tenant's library", modelID, tenant)
		}
		// The tenant moved gpt-image-2 to the codex owner group. A sibling that kept
		// the catalog default would land outside that group and stay invisible there.
		if owner != "codex" {
			t.Fatalf("%s owner = %q, want it to follow the sibling's %q", modelID, owner, "codex")
		}
	}
}

// TestMediaSeedFallsBackToCatalogOwner covers a tenant with no sibling to follow.
func TestMediaSeedFallsBackToCatalogOwner(t *testing.T) {
	db := newSeededTestDB(t)

	const tenant = "11f9ab9c-9fa6-4875-a76a-c39f113c57eb"
	// Present in the tenant, but not an image model, so it must not be followed.
	if _, err := db.Exec(
		`INSERT INTO model_configs (tenant_id, model_id, owned_by, enabled, source, updated_at)
		 VALUES (?, 'gpt-5.5', 'some-custom-owner', 1, 'seed', '2026-01-01T00:00:00Z')`,
		tenant,
	); err != nil {
		t.Fatalf("seed tenant chat row: %v", err)
	}

	seedMediaGenerationModelsForTenants(db)

	owner, ok := modelRowOwner(t, db, tenant, "gpt-image-2.5-flare")
	if !ok {
		t.Fatal("gpt-image-2.5-flare missing from a tenant with no image sibling")
	}
	if owner != "openai" {
		t.Fatalf("owner = %q, want the catalog default %q", owner, "openai")
	}
}

// TestMediaSeedPreservesOperatorEdits is the guard against this running on every
// startup and undoing console changes.
func TestMediaSeedPreservesOperatorEdits(t *testing.T) {
	db := newSeededTestDB(t)

	const tenant = "14b1ee9a-6177-4f5f-b5d4-4fba60ad24fa"
	if _, err := db.Exec(
		`INSERT INTO model_configs (tenant_id, model_id, owned_by, enabled, pricing_mode, price_per_call, source, updated_at)
		 VALUES (?, 'gpt-image-2.5-flare', 'operator-owner', 0, 'call', 9.99, 'user', '2026-01-01T00:00:00Z')`,
		tenant,
	); err != nil {
		t.Fatalf("seed operator row: %v", err)
	}

	seedMediaGenerationModelsForTenants(db)

	var (
		ownedBy string
		enabled int
		price   float64
		source  string
	)
	if err := db.QueryRow(
		`SELECT owned_by, enabled, price_per_call, source FROM model_configs
		 WHERE tenant_id = ? AND model_id = 'gpt-image-2.5-flare'`,
		tenant,
	).Scan(&ownedBy, &enabled, &price, &source); err != nil {
		t.Fatalf("query operator row: %v", err)
	}
	if ownedBy != "operator-owner" || enabled != 0 || price != 9.99 || source != "user" {
		t.Fatalf("operator row was overwritten: owner=%q enabled=%d price=%v source=%q", ownedBy, enabled, price, source)
	}
}

// TestChatModelsAreNotSeededPerTenant keeps the change narrow: chat availability
// legitimately differs per tenant, so only media models are broadcast.
func TestChatModelsAreNotSeededPerTenant(t *testing.T) {
	db := newSeededTestDB(t)

	const tenant = "tenant-without-chat-models"
	if _, err := db.Exec(
		`INSERT INTO model_configs (tenant_id, model_id, owned_by, enabled, source, updated_at)
		 VALUES (?, 'gpt-image-2', 'codex', 1, 'seed', '2026-01-01T00:00:00Z')`,
		tenant,
	); err != nil {
		t.Fatalf("seed tenant row: %v", err)
	}

	seedMediaGenerationModelsForTenants(db)

	if _, ok := modelRowOwner(t, db, tenant, "gpt-5.5"); ok {
		t.Fatal("gpt-5.5 was seeded into a tenant, but chat models must stay per-tenant")
	}
	if _, ok := modelRowOwner(t, db, systemTenant, "gpt-5.5"); !ok {
		t.Fatal("gpt-5.5 should still be seeded into the system tenant")
	}
}
