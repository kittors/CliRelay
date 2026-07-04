package providers

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const clinePassModelPrefix = "cline-pass/"

func validateOpenCodeGoKeyModels(entry config.OpenCodeGoKey) error {
	for _, model := range entry.Models {
		if isClinePassModelID(model.Name) {
			return providerModelOwnershipError("opencode-go", "models", model.Name, "must not use cline-pass model IDs")
		}
	}
	for _, model := range entry.ExcludedModels {
		if model == "*" {
			continue
		}
		if isClinePassModelID(model) {
			return providerModelOwnershipError("opencode-go", "excluded-models", model, "must not use cline-pass model IDs")
		}
	}
	if isClinePassModelID(entry.VisionFallbackModel) {
		return providerModelOwnershipError("opencode-go", "vision-fallback-model", entry.VisionFallbackModel, "must not use cline-pass model IDs")
	}
	return nil
}

func validateClineKeyModels(entry config.ClineKey) error {
	for _, model := range entry.Models {
		if !isClinePassModelID(model.Name) {
			return providerModelOwnershipError("cline", "models", model.Name, "must use cline-pass model IDs")
		}
	}
	for _, model := range entry.ExcludedModels {
		if model == "*" {
			continue
		}
		if !isClinePassModelID(model) {
			return providerModelOwnershipError("cline", "excluded-models", model, "must use cline-pass model IDs")
		}
	}
	if entry.VisionFallbackModel != "" && !isClinePassModelID(entry.VisionFallbackModel) {
		return providerModelOwnershipError("cline", "vision-fallback-model", entry.VisionFallbackModel, "must use cline-pass model IDs")
	}
	return nil
}

func isClinePassModelID(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), clinePassModelPrefix)
}

func providerModelOwnershipError(provider, field, model, rule string) error {
	return fmt.Errorf("%s %s contains invalid model %q: %s", provider, field, strings.TrimSpace(model), rule)
}
