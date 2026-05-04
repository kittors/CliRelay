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
