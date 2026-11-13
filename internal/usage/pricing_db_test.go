package usage

import (
	"math"
	"testing"
	"time"
)

func TestCalculateCostDiscountsCachedInputSubset(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "cache-aware-model",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  10,
		OutputPricePerMillion: 20,
		CachedPricePerMillion: 1,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	cost := CalculateCost("cache-aware-model", 1000, 500, 800)
	want := (float64(200)*10 + float64(500)*20 + float64(800)*1) / 1_000_000
	if cost != want {
		t.Fatalf("cost = %.10f, want %.10f", cost, want)
	}
}

func TestCalculateCostKeepsSeparateCacheTokensFromInput(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "separate-cache-model",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  3,
		OutputPricePerMillion: 15,
		CachedPricePerMillion: 0.3,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	cost := CalculateCost("separate-cache-model", 21, 393, 188086)
	want := (float64(21)*3 + float64(393)*15 + float64(188086)*0.3) / 1_000_000
	if cost != want {
		t.Fatalf("cost = %.10f, want %.10f", cost, want)
	}
}

func TestCalculateCostFallsBackToInputPriceWhenCachedPriceMissing(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "missing-cache-price-model",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  10,
		OutputPricePerMillion: 20,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	cost := CalculateCost("missing-cache-price-model", 1000, 500, 800)
	want := (float64(1000)*10 + float64(500)*20) / 1_000_000
	if cost != want {
		t.Fatalf("cost = %.10f, want %.10f", cost, want)
	}
}

func TestCalculateCostV2CacheReadIncludedInInput(t *testing.T) {
	// OpenAI-compatible: cache_read_tokens is subset of input_tokens,
	// billable input should exclude cache_read_tokens.
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:                  "openai-cache-model",
		Enabled:                  true,
		PricingMode:              "token",
		InputPricePerMillion:     10,
		OutputPricePerMillion:    20,
		CachedPricePerMillion:    1.5,
		CacheReadPricePerMillion: 0.5,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	tokens := TokenStats{
		InputTokens:              1000,
		OutputTokens:             500,
		CacheReadTokens:          800,
		CachedTokens:             800,
		CacheReadIncludedInInput: true,
	}

	cost := CalculateCostV2("openai-cache-model", tokens)
	// Input billable: 1000 - 800 = 200, at 10/M = 0.002
	// Output: 500 at 20/M = 0.01
	// Cache read: 800 at 0.5/M = 0.0004
	want := (float64(200)*10 + float64(500)*20 + float64(800)*0.5) / 1_000_000
	if cost != want {
		t.Fatalf("cost = %.10f, want %.10f", cost, want)
	}
}

func TestCalculateCostV2CacheReadSeparate(t *testing.T) {
	// Claude/Gemini style: cache_read_tokens is NOT included in input_tokens,
	// so input tokens are billed at full amount.
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:                  "claude-cache-model",
		Enabled:                  true,
		PricingMode:              "token",
		InputPricePerMillion:     10,
		OutputPricePerMillion:    20,
		CacheReadPricePerMillion: 0.3,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	tokens := TokenStats{
		InputTokens:              1000,
		OutputTokens:             500,
		CacheReadTokens:          800,
		CachedTokens:             800,
		CacheReadIncludedInInput: false,
	}

	cost := CalculateCostV2("claude-cache-model", tokens)
	// Input billable: 1000 at 10/M = 0.01
	// Output: 500 at 20/M = 0.01
	// Cache read: 800 at 0.3/M = 0.00024
	want := (float64(1000)*10 + float64(500)*20 + float64(800)*0.3) / 1_000_000
	if cost != want {
		t.Fatalf("cost = %.10f, want %.10f", cost, want)
	}
}

func TestCalculateCostV2CacheWriteOnly(t *testing.T) {
	// Cache write/creation only (e.g., first request with a new cache context).
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:                   "creation-model",
		Enabled:                   true,
		PricingMode:               "token",
		InputPricePerMillion:      10,
		OutputPricePerMillion:     20,
		CacheWritePricePerMillion: 5,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	tokens := TokenStats{
		InputTokens:      1000,
		OutputTokens:     500,
		CacheWriteTokens: 800,
		CachedTokens:     800,
	}

	cost := CalculateCostV2("creation-model", tokens)
	// Input billable: 1000 at 10/M = 0.01
	// Output: 500 at 20/M = 0.01
	// Cache write: 800 at 5/M = 0.004
	want := (float64(1000)*10 + float64(500)*20 + float64(800)*5) / 1_000_000
	if cost != want {
		t.Fatalf("cost = %.10f, want %.10f", cost, want)
	}
}

func TestCalculateCostV2CacheReadAndWriteBothPresent(t *testing.T) {
	// Claude scenario: both cache read and cache creation in the same response.
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:                   "claude-both-cache-model",
		Enabled:                   true,
		PricingMode:               "token",
		InputPricePerMillion:      10,
		OutputPricePerMillion:     20,
		CachedPricePerMillion:     0.3,
		CacheReadPricePerMillion:  0.3,
		CacheWritePricePerMillion: 8,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	tokens := TokenStats{
		InputTokens:      1000,
		OutputTokens:     500,
		CacheReadTokens:  700,
		CacheWriteTokens: 300,
		CachedTokens:     700,
	}

	cost := CalculateCostV2("claude-both-cache-model", tokens)
	// Match the order of operations used in calculateTokenCostV2 to avoid float rounding diffs.
	want := float64(1000)/1_000_000*10 + float64(500)/1_000_000*20 +
		float64(700)/1_000_000*0.3 + float64(300)/1_000_000*8
	if cost != want {
		t.Fatalf("cost = %.20f, want %.20f", cost, want)
	}
}

func TestCalculateCostV2FallsBackToCachedPrice(t *testing.T) {
	// When cache_read_price_per_million is 0 but cached_price_per_million is set,
	// use the legacy cached_price_per_million as fallback.
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "fallback-model",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  10,
		OutputPricePerMillion: 20,
		CachedPricePerMillion: 2,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	tokens := TokenStats{
		InputTokens:              1000,
		OutputTokens:             500,
		CacheReadTokens:          800,
		CachedTokens:             800,
		CacheReadIncludedInInput: true,
	}

	cost := CalculateCostV2("fallback-model", tokens)
	// Match order of operations used in calculateTokenCostV2.
	want := float64(200)/1_000_000*10 + float64(500)/1_000_000*20 + float64(800)/1_000_000*2
	if cost != want {
		t.Fatalf("cost = %.20f, want %.20f", cost, want)
	}
}

func TestCalculateCostV2FallbackLegacyWithModelPricingTable(t *testing.T) {
	// Test that legacy model_pricing table entries work with CalculateCostV2.
	initModelConfigTestDB(t)

	if err := UpsertModelPricingV2("legacy-table-model", 10, 20, 2, 0, 0); err != nil {
		t.Fatalf("UpsertModelPricingV2() error = %v", err)
	}

	tokens := TokenStats{
		InputTokens:              1000,
		OutputTokens:             500,
		CacheReadTokens:          800,
		CachedTokens:             800,
		CacheReadIncludedInInput: true,
	}

	cost := CalculateCostV2("legacy-table-model", tokens)
	// Should use CalculateCostV2's legacy fallback path (since cache read/write prices are 0)
	// which calls calculateTokenCost with the old heuristic.
	want := CalculateCost("legacy-table-model", 1000, 500, 800)
	if cost != want {
		t.Fatalf("cost = %.10f, want %.10f (legacy CalculateCost = %.10f)", cost, want, want)
	}
}

func TestCalculateCostFallsBackToProviderlessModelPricing(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:                  "deepseek-v4-flash",
		Enabled:                  true,
		PricingMode:              "token",
		InputPricePerMillion:     0.3,
		OutputPricePerMillion:    1.2,
		CacheReadPricePerMillion: 0.03,
		Source:                   "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	for _, modelID := range []string{
		"ollama/deepseek-v4-flash",
		"foo-deepseek-v4-flash",
	} {
		legacyCost := CalculateCost(modelID, 1_000_000, 1_000_000, 0)
		if legacyCost != 1.5 {
			t.Fatalf("CalculateCost(%q) = %v, want 1.5", modelID, legacyCost)
		}

		v2Cost := CalculateCostV2(modelID, TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000})
		if v2Cost != 1.5 {
			t.Fatalf("CalculateCostV2(%q) = %v, want 1.5", modelID, v2Cost)
		}
	}

	if err := UpsertModelConfigForTenant(businessTenantID, ModelConfigRow{
		ModelID:               "deepseek-v4-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  3,
		OutputPricePerMillion: 12,
		Source:                "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfigForTenant() error = %v", err)
	}
	tenantCost := CalculateCostV2ForTenant(businessTenantID, "ollama/deepseek-v4-flash", TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if tenantCost != 15 {
		t.Fatalf("tenant providerless override cost = %v, want 15", tenantCost)
	}
}

func TestCalculateCostFallsBackPastExactUnpricedConfig(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "deepseek-v4-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0.3,
		OutputPricePerMillion: 1.2,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(base) error = %v", err)
	}
	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:     "ollama/deepseek-v4-flash",
		Enabled:     true,
		PricingMode: "token",
		Source:      "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(exact) error = %v", err)
	}

	for name, cost := range map[string]float64{
		"legacy": CalculateCost("ollama/deepseek-v4-flash", 1_000_000, 1_000_000, 0),
		"v2":     CalculateCostV2("ollama/deepseek-v4-flash", TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000}),
	} {
		if cost != 1.5 {
			t.Fatalf("%s exact-unpriced fallback cost = %v, want 1.5", name, cost)
		}
	}
}

func TestCalculateCostFallsBackPastUnpricedVariant(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "deepseek-v4-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0.098,
		OutputPricePerMillion: 0.196,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(base) error = %v", err)
	}
	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:     "deepseek-v4-flash:free",
		Enabled:     true,
		PricingMode: "token",
		Source:      "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(variant) error = %v", err)
	}

	row, ok := resolveModelPricingRowForTenant(systemTenantID, "deepseek-v4-flash:free")
	if !ok || row.InputPricePerMillion != 0.098 || row.OutputPricePerMillion != 0.196 {
		t.Fatalf("resolved variant pricing = %+v ok=%v, want base pricing", row, ok)
	}
	wantCost := 0.098 + 0.196
	for name, cost := range map[string]float64{
		"legacy": CalculateCost("deepseek-v4-flash:free", 1_000_000, 1_000_000, 0),
		"v2":     CalculateCostV2("deepseek-v4-flash:free", TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000}),
	} {
		if math.Abs(cost-wantCost) > 1e-12 {
			t.Fatalf("%s variant fallback cost = %v, want %v", name, cost, wantCost)
		}
	}
}

func TestCalculateCostVariantPreservesExactCustomPricing(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "deepseek-v4-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0.098,
		OutputPricePerMillion: 0.196,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(base) error = %v", err)
	}
	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "deepseek-v4-flash:free",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  3,
		OutputPricePerMillion: 12,
		Source:                "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(variant) error = %v", err)
	}

	if cost := CalculateCostV2("deepseek-v4-flash:free", TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000}); cost != 15 {
		t.Fatalf("exact variant custom pricing cost = %v, want 15", cost)
	}
}

func TestCalculateCostInheritedPricingRespectsExactDisabled(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "deepseek-v4-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0.3,
		OutputPricePerMillion: 1.2,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(base) error = %v", err)
	}
	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:     "ollama/deepseek-v4-flash",
		Enabled:     false,
		PricingMode: "token",
		Source:      "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(exact) error = %v", err)
	}

	row, ok := resolveModelPricingRowForTenant(systemTenantID, "ollama/deepseek-v4-flash")
	if !ok || !hasEffectiveModelPricing(row) {
		t.Fatalf("resolved pricing = %+v ok=%v, want inherited base pricing", row, ok)
	}
	if row.Enabled {
		t.Fatalf("resolved pricing = %+v, want exact disabled state preserved", row)
	}
	if cost := CalculateCost("ollama/deepseek-v4-flash", 1_000_000, 1_000_000, 0); cost != 0 {
		t.Fatalf("disabled exact model cost = %v, want 0", cost)
	}
}

func TestCalculateCostProviderlessFallbackPreservesExactCustomPricing(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfigForTenant(systemTenantID, ModelConfigRow{
		ModelID:               "deepseek-v4-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0.3,
		OutputPricePerMillion: 1.2,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfigForTenant(base) error = %v", err)
	}
	if err := UpsertModelConfigForTenant(businessTenantID, ModelConfigRow{
		ModelID:               "ollama/deepseek-v4-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  3,
		OutputPricePerMillion: 12,
		Source:                "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfigForTenant(exact) error = %v", err)
	}

	cost := CalculateCostV2ForTenant(businessTenantID, "ollama/deepseek-v4-flash", TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if cost != 15 {
		t.Fatalf("exact custom pricing cost = %v, want 15", cost)
	}
}

func TestCalculateCostProviderlessFallbackSupportsCallPricing(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:      "image-call-base",
		Enabled:      true,
		PricingMode:  "call",
		PricePerCall: 0.04,
		Source:       "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	for name, cost := range map[string]float64{
		"legacy": CalculateCost("ollama/image-call-base", 0, 0, 0),
		"v2":     CalculateCostV2("ollama/image-call-base", TokenStats{}),
	} {
		if cost != 0.04 {
			t.Fatalf("%s call cost = %v, want 0.04", name, cost)
		}
	}
}

func TestQueryTodayCostByKeyResetsDaily(t *testing.T) {
	initModelConfigTestDB(t)
	db := getDB()
	if db == nil {
		t.Fatal("expected test db")
	}

	today := CutoffStartUTC(1)
	yesterday := today.Add(-time.Second)
	for _, row := range []struct {
		ts   time.Time
		cost float64
	}{
		{ts: today.Add(time.Hour), cost: 1.25},
		{ts: today.Add(2 * time.Hour), cost: 2.75},
		{ts: yesterday, cost: 100},
	} {
		if _, err := db.Exec(
			`INSERT INTO request_logs
			 (timestamp, api_key, api_key_id, model, source, failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
			 VALUES (?, ?, ?, ?, ?, 0, 1, 0, 0, 0, 0, 0, ?)`,
			row.ts.Format(time.RFC3339), "sk-daily-spending", "key-daily-spending", "model", "test", row.cost,
		); err != nil {
			t.Fatalf("insert request log: %v", err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := projectUsageRollupTx(usageTestMetadataTx(tx), rollupEvent{
			TenantID: systemTenantID,
			APIKeyID: "key-daily-spending",
			Model:    "model",
			Source:   "test",
			Cost:     row.cost,
			At:       row.ts,
		}); err != nil {
			_ = tx.Rollback()
			t.Fatalf("project: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	_ = UpsertAPIKey(APIKeyRow{ID: "key-daily-spending", Key: "sk-daily-spending"})

	got, err := QueryTodayCostByKey("sk-daily-spending")
	if err != nil {
		t.Fatalf("QueryTodayCostByKey() error = %v", err)
	}
	if math.Abs(got-4) > 1e-12 {
		t.Fatalf("today cost = %v, want 4", got)
	}
}

func TestCalculateCostSuffixStrippedFallback(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "claude-opus-4-6",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  15,
		OutputPricePerMillion: 75,
		Source:                "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(base) error = %v", err)
	}

	// Models with variant/tier suffixes should fall back to the base via
	// last-dash stripping: "claude-opus-4-6-thinking" → "claude-opus-4-6".
	for _, modelID := range []string{
		"claude-opus-4-6-thinking",
		"claude-opus-4-6-agent",
		"claude-opus-4-6-high",
		"claude-opus-4-6-low",
		"claude-opus-4-6-medium",
		"claude-opus-4-6-tiered",
	} {
		wantCost := 15.0 + 75.0
		cost := CalculateCostV2(modelID, TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000})
		if math.Abs(cost-wantCost) > 1e-12 {
			t.Fatalf("CalculateCostV2(%q) = %v, want %v", modelID, cost, wantCost)
		}
	}
}

func TestCalculateCostSuffixStrippedFallbackWithProvider(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "gemini-2.5-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0.3,
		OutputPricePerMillion: 2.5,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(base) error = %v", err)
	}

	// Provider-prefixed model with suffix should still fall back via
	// providerless last-dash stripping: "ollama/gemini-2.5-flash-thinking"
	// → providerless "gemini-2.5-flash-thinking" → last-dash "gemini-2.5-flash".
	wantCost := 0.3 + 2.5
	cost := CalculateCostV2("ollama/gemini-2.5-flash-thinking", TokenStats{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if math.Abs(cost-wantCost) > 1e-12 {
		t.Fatalf("CalculateCostV2(ollama/gemini-2.5-flash-thinking) = %v, want %v", cost, wantCost)
	}
}
