package modelconfig

import (
	"database/sql"
	"os"
	"strings"
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

	// The production shape this got wrong once: the tenant holds two older models
	// left on the catalog default alongside one the operator retargeted to codex.
	// Following the first sibling by id picks gpt-image-1 and reproduces the
	// default, missing the retarget.
	const tenant = "9e003dfb-751f-4898-b186-45f765c763a6"
	for _, row := range []struct {
		modelID string
		ownedBy string
	}{
		{"gpt-image-1", "openai"},
		{"gpt-image-1-mini", "openai"},
		{"gpt-image-2", "codex"},
	} {
		if _, err := db.Exec(
			`INSERT INTO model_configs (tenant_id, model_id, owned_by, enabled, source, updated_at)
			 VALUES (?, ?, ?, 1, 'seed', '2026-01-01T00:00:00Z')`,
			tenant, row.modelID, row.ownedBy,
		); err != nil {
			t.Fatalf("seed existing tenant row %s: %v", row.modelID, err)
		}
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

// TestMediaSeedFallsBackToCatalogOwner covers a tenant whose family members all sit
// on the catalog default, so there is no operator retarget to inherit.
func TestMediaSeedFallsBackToCatalogOwner(t *testing.T) {
	db := newSeededTestDB(t)

	const tenant = "11f9ab9c-9fa6-4875-a76a-c39f113c57eb"
	if _, err := db.Exec(
		`INSERT INTO model_configs (tenant_id, model_id, owned_by, enabled, source, updated_at)
		 VALUES (?, 'gpt-image-2', 'openai', 1, 'seed', '2026-01-01T00:00:00Z')`,
		tenant,
	); err != nil {
		t.Fatalf("seed tenant image row: %v", err)
	}
	// A chat model with an unusual owner must not be mistaken for a family sibling.
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
		t.Fatal("gpt-image-2.5-flare missing from a tenant that has gpt-image-2")
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

// TestTenantIDQueryAvoidsEmptyStringComparison guards the engine difference that
// made the first version of this seed a silent no-op in production.
//
// tenant_id is a uuid column on PostgreSQL, where an empty-string comparison fails with
// "invalid input syntax for type uuid", the error is logged as a warning, and the
// seed does nothing. SQLite types loosely and accepts it, so the tests passed. The
// unit suite cannot reach PostgreSQL, so this asserts on the SQL text instead.
func TestTenantIDQueryAvoidsEmptyStringComparison(t *testing.T) {
	source, err := os.ReadFile("tenant_media_seed.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if strings.Contains(string(source), "tenant_id != ''") {
		t.Fatal("tenant_id is a uuid column on PostgreSQL; comparing it to '' aborts the query. Use IS NOT NULL and filter blanks in Go.")
	}
}

// TestSeedSkipsFamiliesTheTenantDoesNotUse is the blast-radius guard.
//
// Broadcasting every media model to every tenant would fill a Codex-only tenant's
// library with grok-imagine and minimax rows it has no credential for — noise the
// operator then has to sift through. Only a family the tenant already uses is
// completed.
func TestSeedSkipsFamiliesTheTenantDoesNotUse(t *testing.T) {
	db := newSeededTestDB(t)

	const tenant = "codex-only-tenant-0000-0000-000000000000"
	if _, err := db.Exec(
		`INSERT INTO model_configs (tenant_id, model_id, owned_by, enabled, source, updated_at)
		 VALUES (?, 'gpt-image-2', 'codex', 1, 'seed', '2026-01-01T00:00:00Z')`,
		tenant,
	); err != nil {
		t.Fatalf("seed tenant row: %v", err)
	}

	seedMediaGenerationModelsForTenants(db)

	// The family it uses is completed.
	if _, ok := modelRowOwner(t, db, tenant, "gpt-image-2.5-flare"); !ok {
		t.Fatal("gpt-image-2.5-flare should be added to a tenant that already has gpt-image-2")
	}
	// Families it does not use are left alone.
	for _, modelID := range []string{"grok-imagine-image", "grok-imagine-image-quality", "image-01"} {
		if _, ok := modelRowOwner(t, db, tenant, modelID); ok {
			t.Fatalf("%s was added to a tenant with no model of that family", modelID)
		}
	}
	// The system tenant still carries the full catalog.
	if _, ok := modelRowOwner(t, db, systemTenant, "grok-imagine-image"); !ok {
		t.Fatal("grok-imagine-image should still be in the system tenant catalog")
	}
}
