package modelconfig

import "github.com/router-for-me/CLIProxyAPI/v6/internal/registry"

// applyImageGenerationSeedDefaults stamps the billing and modality shape an
// image-generation model needs onto a seed row.
//
// Image models are billed per invocation rather than per token, so a row seeded
// with the default token pricing would under-report spend. The classification is
// deliberately not spelled out here: it lives in internal/registry alongside the
// model definitions, so adding a provider does not mean hunting for every branch
// that special-cased a model ID.
func applyImageGenerationSeedDefaults(row *ModelConfigRow, modelID string) {
	if row == nil || !registry.IsImageGenerationModel(modelID) {
		return
	}
	row.InputModalities = registry.ImageGenerationInputModalities(modelID)
	row.OutputModalities = registry.ImageGenerationOutputModalities()
	row.PricingMode = "call"
	if price, description, ok := registry.ImageGenerationModelDefaults(modelID); ok {
		row.PricePerCall = price
		row.Description = description
	}
}
