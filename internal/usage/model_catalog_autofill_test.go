package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const otherTenant = "00000000-0000-0000-0000-0000000000cc"

func useOpenRouterListing(t *testing.T, models ...OpenRouterRemoteModel) {
	t.Helper()
	resetModelCatalogAutofillListForTest()
	t.Cleanup(SetOpenRouterModelFetcherForTest(func(context.Context) ([]OpenRouterRemoteModel, error) {
		return models, nil
	}))
}

func listing(id, prompt, completion string) OpenRouterRemoteModel {
	return OpenRouterRemoteModel{
		ID: id, Name: "Listing " + id, Description: "From OpenRouter", ContextLength: 400_000,
		Pricing: OpenRouterRemotePricing{Prompt: prompt, Completion: completion, InputCacheRead: "0.0000002"},
	}
}

func logTenantRequest(t *testing.T, tenantID, model string) {
	t.Helper()
	InsertRequestLog(RequestLogEntry{
		TrustedTenantID: tenantID, APIKey: "sk-autofill", APIKeyID: "key-autofill", Model: model, Source: "codex",
		Timestamp: time.Now().UTC().Add(-time.Minute), Tokens: resyncTokens,
	})
}

func pendingJobs(t *testing.T, tenantID string) []PricingResyncJob {
	t.Helper()
	jobs, err := ListPricingResyncJobs(tenantID, 50)
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	return jobs
}

func TestAutofillPricesAModelInUseAndQueuesTheFill(t *testing.T) {
	initPricingResyncTestDB(t)
	useOpenRouterListing(t, listing("openai/fresh-auto", "0.000002", "0.00001"))
	logTenantRequest(t, resyncTenant, "fresh-auto")

	runModelCatalogAutofill(context.Background(), time.Now())

	row, ok := modelConfigStoreForTenant(systemTenantID).GetModelConfig("fresh-auto")
	if !ok || row.Source != openRouterModelSource || row.InputPricePerMillion != 2 || row.OutputPricePerMillion != 10 ||
		row.ContextLength != 400_000 || row.Description == "" {
		t.Fatalf("system row = %+v (found %v)", row, ok)
	}
	if fingerprint := EffectivePricingFingerprint(resyncTenant, "fresh-auto"); fingerprint == "" {
		t.Fatal("the tenant inheriting the system catalog still has no price")
	}
	jobs := pendingJobs(t, systemTenantID)
	if len(jobs) != 1 || jobs[0].Mode != PricingResyncFill || jobs[0].Origin != PricingResyncOriginAuto || jobs[0].ModelIDs[0] != "fresh-auto" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestAutofillNeverTouchesAnOperatorsRow(t *testing.T) {
	initPricingResyncTestDB(t)
	useOpenRouterListing(t, listing("openai/kept-free", "0.000002", "0.00001"))
	if err := UpsertModelConfigForTenant(systemTenantID, ModelConfigRow{ModelID: "kept-free", Enabled: true, PricingMode: "token", Source: "user"}); err != nil {
		t.Fatalf("operator row: %v", err)
	}
	logTenantRequest(t, resyncTenant, "kept-free")

	runModelCatalogAutofill(context.Background(), time.Now())

	row, _ := modelConfigStoreForTenant(systemTenantID).GetModelConfig("kept-free")
	if row.Source != "user" || row.InputPricePerMillion != 0 || len(pendingJobs(t, systemTenantID)) != 0 {
		t.Fatalf("operator row changed: %+v", row)
	}
}

func TestAutofillKeepsAPriceATenantGetsFromItsOwnBaseRow(t *testing.T) {
	initPricingResyncTestDB(t)
	useOpenRouterListing(t, listing("openai/family-x-sol", "0.000005", "0.00002"))
	// resyncTenant prices the family through its own base row; otherTenant has no
	// catalog of its own and no price.
	priceModel(t, resyncTenant, "family-x", 1, 4, 0.1)
	logTenantRequest(t, resyncTenant, "family-x-sol")
	logTenantRequest(t, otherTenant, "family-x-sol")
	before := EffectivePricingFingerprint(resyncTenant, "family-x-sol")

	runModelCatalogAutofill(context.Background(), time.Now())

	if _, ok := modelConfigStoreForTenant(systemTenantID).GetModelConfig("family-x-sol"); ok {
		t.Fatal("a system row would have replaced the price resyncTenant bills with")
	}
	if after := EffectivePricingFingerprint(resyncTenant, "family-x-sol"); after != before || before == "" {
		t.Fatalf("tenant price %q -> %q", before, after)
	}
}

func TestAutofillFillsATenantsUnpricedSeedRow(t *testing.T) {
	initPricingResyncTestDB(t)
	useOpenRouterListing(t, listing("openai/seeded-model", "0.000001", "0.000004"))
	if err := UpsertModelConfigForTenant(resyncTenant, ModelConfigRow{ModelID: "seeded-model", Enabled: true, PricingMode: "token", Source: "seed", Description: "Kept"}); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	logTenantRequest(t, resyncTenant, "seeded-model")

	runModelCatalogAutofill(context.Background(), time.Now())

	row, _ := modelConfigStoreForTenant(resyncTenant).GetModelConfig("seeded-model")
	if row.InputPricePerMillion != 1 || row.OutputPricePerMillion != 4 || row.Description != "Kept" || row.ContextLength != 400_000 {
		t.Fatalf("tenant seed row = %+v", row)
	}
	if _, ok := modelConfigStoreForTenant(systemTenantID).GetModelConfig("seeded-model"); ok {
		t.Fatal("filled the system catalog instead of the tenant's own row")
	}
}

func TestAutofillIgnoresModelsThatNeverAnswered(t *testing.T) {
	initPricingResyncTestDB(t)
	useOpenRouterListing(t, listing("openai/never-served", "0.000002", "0.00001"))
	InsertRequestLog(RequestLogEntry{
		TrustedTenantID: resyncTenant, APIKey: "sk-autofill", APIKeyID: "key-autofill", Model: "never-served", Source: "codex",
		Failed: true, Timestamp: time.Now().UTC().Add(-time.Minute),
	})

	runModelCatalogAutofill(context.Background(), time.Now())

	if _, ok := modelConfigStoreForTenant(systemTenantID).GetModelConfig("never-served"); ok {
		t.Fatal("a model no upstream answered got a catalog row")
	}
}

func TestAutofillSkipsListingsThatDisagreeOnPrice(t *testing.T) {
	initPricingResyncTestDB(t)
	useOpenRouterListing(t, listing("openai/dup-model", "0.000002", "0.00001"), listing("azure/dup-model", "0.000003", "0.00001"))
	logTenantRequest(t, resyncTenant, "dup-model")

	runModelCatalogAutofill(context.Background(), time.Now())

	if _, ok := modelConfigStoreForTenant(systemTenantID).GetModelConfig("dup-model"); ok {
		t.Fatal("priced from one of two disagreeing listings")
	}
}

func TestOpenRouterSyncDoesNotRewriteRowsThatDidNotChange(t *testing.T) {
	initPricingResyncTestDB(t)
	models := []OpenRouterRemoteModel{
		listing("openai/sync-a", "0.000002", "0.00001"),
		listing("anthropic/claude-sync-b", "0.000003", "0.000015"),
	}
	ctx := context.Background()
	if _, err := SyncOpenRouterModelListForTenant(ctx, systemTenantID, models); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	const stamp = "2020-01-01T00:00:00Z"
	if _, err := getDB().Exec(`UPDATE model_configs SET updated_at = ? WHERE source = ?`, stamp, openRouterModelSource); err != nil {
		t.Fatalf("mark rows: %v", err)
	}
	if _, err := SyncOpenRouterModelListForTenant(ctx, systemTenantID, models); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	var rewritten int
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM model_configs WHERE source = ? AND updated_at <> ?`, openRouterModelSource, stamp).Scan(&rewritten); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rewritten != 0 {
		t.Fatalf("%d unchanged rows rewritten", rewritten)
	}
	// A real change is still written.
	models[0].Pricing.Prompt = "0.000004"
	if _, err := SyncOpenRouterModelListForTenant(ctx, systemTenantID, models); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if row, _ := modelConfigStoreForTenant(systemTenantID).GetModelConfig("sync-a"); row.InputPricePerMillion != 4 {
		t.Fatalf("changed price not written: %+v", row)
	}
}

func TestStartupMergeKeepsTheSystemPriceAgainstATenantsLegacyRow(t *testing.T) {
	CloseDB()
	path := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(path, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(CloseDB)
	priceModel(t, systemTenantID, "shared-model", 2, 10, 0.1)
	// A business tenant's own legacy price for the same model.
	if _, err := getDB().Exec(`INSERT INTO model_pricing (tenant_id, model_id, input_price_per_million, output_price_per_million, cached_price_per_million, updated_at)
		VALUES (?, 'shared-model', 9, 90, 0.9, ?)`, otherTenant, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("tenant legacy row: %v", err)
	}
	// Restart: the startup merge runs again.
	CloseDB()
	if err := InitDB(path, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB again: %v", err)
	}
	row, _ := modelConfigStoreForTenant(systemTenantID).GetModelConfig("shared-model")
	if row.InputPricePerMillion != 2 || row.OutputPricePerMillion != 10 {
		t.Fatalf("system price overwritten by a tenant's legacy row: %+v", row)
	}
}
