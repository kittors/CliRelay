package authfiles

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	metadataKeyCodexImageGenerationBridge = "codex_image_generation_bridge"
	// Per-account image model for the injected tool. Absent means the build
	// default, which is what every account used before the field existed.
	metadataKeyCodexImageGenerationModel = "codex_image_generation_model"
)

// CodexImageGenerationBridgePayload returns the management-facing payload for
// per-account Codex /responses image_generation tool injection.
//
// The selectable models are reported alongside the setting rather than hardcoded
// in the panel: Codex serves more than one gpt-image release, they are added to
// the catalog over time, and a list maintained in the browser would go stale the
// first time one shipped.
func CodexImageGenerationBridgePayload(auth *coreauth.Auth) map[string]any {
	if !isCodexOAuthAdmissionAuth(auth) {
		return nil
	}
	// Absent metadata means enabled, matching the executor default. Reporting false here
	// would show the panel toggle as off while the bridge is actually injecting the tool.
	enabled := true
	if raw, ok := auth.Metadata[metadataKeyCodexImageGenerationBridge]; ok {
		if value, isBool := raw.(bool); isBool {
			enabled = value
		}
	}

	available := registry.ListImageGenerationModelsForProvider(registry.ImageProviderCodex)
	models := make([]map[string]any, 0, len(available))
	for _, model := range available {
		models = append(models, map[string]any{
			"id":            model.ID,
			"display_name":  model.DisplayName,
			"description":   model.Description,
			"supports_edit": model.SupportsEdit,
		})
	}

	return map[string]any{
		"enabled": enabled,
		// An empty model means "the build default"; the panel shows that as the
		// default option rather than pre-selecting a specific release, so an
		// account keeps following the default when it changes.
		"model":            CodexImageGenerationModel(auth),
		"available_models": models,
	}
}

// CodexImageGenerationModel reports the image model an account pins for the
// injected tool, or "" when it follows the build default.
func CodexImageGenerationModel(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata[metadataKeyCodexImageGenerationModel].(string)
	return strings.TrimSpace(value)
}

func ensureCodexImageGenerationBridgeEditable(auth *coreauth.Auth) error {
	if !isCodexOAuthAdmissionAuth(auth) {
		return fmt.Errorf("codex image generation bridge is only supported for Codex OAuth auth files")
	}
	return nil
}

// normalizeCodexImageGenerationModel validates a requested model against the
// Codex image catalog.
//
// Validation happens here rather than at request time because a typo would
// otherwise surface as an upstream 400 on the account's next image turn, long
// after the operator left this form, and the setting would look accepted.
func normalizeCodexImageGenerationModel(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		// Clearing the field returns the account to the build default.
		return "", nil
	}
	available := registry.ListImageGenerationModelsForProvider(registry.ImageProviderCodex)
	for _, model := range available {
		if strings.EqualFold(model.ID, trimmed) {
			return model.ID, nil
		}
	}
	names := make([]string, 0, len(available))
	for _, model := range available {
		names = append(names, model.ID)
	}
	return "", fmt.Errorf("codex image generation model %q is not available (supported: %s)",
		trimmed, strings.Join(names, ", "))
}
