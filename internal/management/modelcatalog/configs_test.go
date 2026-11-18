package modelcatalog

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestListModelConfigsUsesEffectivePricing(t *testing.T) {
	initModelCatalogTestDB(t)

	const (
		baseModelID    = "deepseek-v4-flash"
		runtimeModelID = "ollama/deepseek-v4-flash"
		variantModelID = "deepseek-v4-flash:free"
	)
	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:               baseModelID,
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0.098,
		OutputPricePerMillion: 0.196,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(base) error = %v", err)
	}
	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:     runtimeModelID,
		Description: "exact runtime row",
		Enabled:     false,
		PricingMode: "token",
		Source:      "seed",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(runtime) error = %v", err)
	}
	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:     variantModelID,
		Enabled:     true,
		PricingMode: "token",
		Source:      "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(variant) error = %v", err)
	}

	result := New(nil, nil).ListModelConfigs("library")
	data, ok := result["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data = %#v, want []map[string]any", result["data"])
	}
	found := make(map[string]bool, 2)
	for _, item := range data {
		modelID, _ := item["id"].(string)
		if modelID != runtimeModelID && modelID != variantModelID {
			continue
		}
		if modelID == runtimeModelID && (item["description"] != "exact runtime row" || item["enabled"] != false || item["source"] != "seed") {
			t.Fatalf("exact identity fields changed: %#v", item)
		}
		pricing, _ := item["pricing"].(map[string]any)
		if pricing["input_price_per_million"] != 0.098 || pricing["output_price_per_million"] != 0.196 {
			t.Fatalf("pricing for %q = %#v, want inherited base pricing", modelID, pricing)
		}
		found[modelID] = true
	}
	if !found[runtimeModelID] || !found[variantModelID] {
		t.Fatalf("library response missing effective pricing rows: found=%v", found)
	}
}
