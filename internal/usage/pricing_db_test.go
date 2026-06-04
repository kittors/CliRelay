package usage

import "testing"

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
		ModelID:                 "openai-cache-model",
		Enabled:                 true,
		PricingMode:             "token",
		InputPricePerMillion:    10,
		OutputPricePerMillion:   20,
		CachedPricePerMillion:   1.5,
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
		ModelID:                 "claude-cache-model",
		Enabled:                 true,
		PricingMode:             "token",
		InputPricePerMillion:    10,
		OutputPricePerMillion:   20,
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
		ModelID:                  "creation-model",
		Enabled:                  true,
		PricingMode:              "token",
		InputPricePerMillion:     10,
		OutputPricePerMillion:    20,
		CacheWritePricePerMillion: 5,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	tokens := TokenStats{
		InputTokens:     1000,
		OutputTokens:    500,
		CacheWriteTokens: 800,
		CachedTokens:    800,
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
		ModelID:                  "claude-both-cache-model",
		Enabled:                  true,
		PricingMode:              "token",
		InputPricePerMillion:     10,
		OutputPricePerMillion:    20,
		CachedPricePerMillion:    0.3,
		CacheReadPricePerMillion:  0.3,
		CacheWritePricePerMillion: 8,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	tokens := TokenStats{
		InputTokens:     1000,
		OutputTokens:    500,
		CacheReadTokens:  700,
		CacheWriteTokens: 300,
		CachedTokens:    700,
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
