package modelcatalog

import (
	"strings"

	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func pricingLookupMapsForTenant(tenantID string) (map[string]usage.ModelConfigRow, map[string]usage.ModelPricingRow) {
	configRows := modelconfigsettings.ListAllConfigsForTenant(tenantID)
	configByID := make(map[string]usage.ModelConfigRow, len(configRows))
	for _, row := range configRows {
		configByID[strings.ToLower(strings.TrimSpace(row.ModelID))] = row
	}
	pricingRows := usage.GetAllModelPricingForTenant(tenantID)
	pricingByID := make(map[string]usage.ModelPricingRow, len(pricingRows))
	for modelID, row := range pricingRows {
		pricingByID[strings.ToLower(strings.TrimSpace(modelID))] = row
	}
	return configByID, pricingByID
}

func modelPricingPayload(row usage.ModelConfigRow) map[string]any {
	return map[string]any{
		"mode":                          row.PricingMode,
		"input_price_per_million":       row.InputPricePerMillion,
		"output_price_per_million":      row.OutputPricePerMillion,
		"cached_price_per_million":      row.CachedPricePerMillion,
		"cache_read_price_per_million":  row.CacheReadPricePerMillion,
		"cache_write_price_per_million": row.CacheWritePricePerMillion,
		"price_per_call":                row.PricePerCall,
	}
}
