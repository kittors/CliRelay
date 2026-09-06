package registry

// Image-generation model definitions.
//
// These live apart from the general static catalog for two reasons: that file is
// already over the structure gate's line budget and may only shrink, and image
// models carry billing and capability rules that the classifier in
// image_generation_models.go reads. Keeping the definitions next to the rules that
// interpret them means adding a model is one file, not two.

// getOpenAIImageModelDefinitions returns OpenAI image-generation models.
func getOpenAIImageModelDefinitions() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:                  "gpt-image-2",
			Object:              "model",
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-image-2",
			DisplayName:         "GPT Image 2",
			Description:         "Text-to-image generation model.",
			SupportedParameters: []string{"prompt", "size", "n", "response_format"},
		},
	}
}

// minimaxImageSupportedParameters lists what the MiniMax image endpoint accepts.
//
// It is declared once and shared by every MiniMax image model so the catalog and
// the request allowlist in the runtime executor cannot drift apart.
var minimaxImageSupportedParameters = []string{
	"prompt",
	"aspect_ratio",
	"width",
	"height",
	"response_format",
	"seed",
	"n",
	"prompt_optimizer",
}

// getMiniMaxImageModelDefinitions returns MiniMax image-generation models.
//
// Pricing is left at zero deliberately: the published reference documents the
// request and response shape but no per-image rate, and an invented number would
// surface as real spend in usage reporting.
func getMiniMaxImageModelDefinitions() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:                  "image-01",
			Object:              "model",
			OwnedBy:             "minimax",
			Type:                "minimax",
			Version:             "image-01",
			DisplayName:         "MiniMax Image 01",
			Name:                "image-01",
			Description:         "MiniMax text-to-image generation, billed per invocation.",
			SupportedParameters: minimaxImageSupportedParameters,
		},
		{
			ID:                  "image-01-live",
			Object:              "model",
			OwnedBy:             "minimax",
			Type:                "minimax",
			Version:             "image-01-live",
			DisplayName:         "MiniMax Image 01 Live",
			Name:                "image-01-live",
			Description:         "MiniMax text-to-image generation tuned for illustrative styles.",
			SupportedParameters: minimaxImageSupportedParameters,
		},
	}
}

// GetMiniMaxModels returns the static MiniMax catalog.
//
// MiniMax currently contributes image models only, so this is the image set. It
// exists as its own accessor because the channel dispatcher and the image
// classifier both resolve providers through GetXxxModels, and routing a model the
// catalog cannot report leaves it selectable but unreachable.
func GetMiniMaxModels() []*ModelInfo {
	return getMiniMaxImageModelDefinitions()
}

// getXAIImageModelDefinitions returns Grok Imagine image-generation models.
//
// Media requests reach the official API host even for subscription credentials,
// because the CLI gateway rejects the payload sizes these carry; see
// xaiMediaBaseURL in the runtime executor.
func getXAIImageModelDefinitions() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:          "grok-imagine-image",
			Object:      "model",
			OwnedBy:     "xai",
			Type:        "xai",
			Version:     "grok-imagine-image",
			DisplayName: "Grok Imagine",
			Name:        "grok-imagine-image",
			Description: "Grok Imagine text-to-image generation with reference image editing.",
			// Media requests reach the official API host even for subscription
			// credentials; see xaiMediaBaseURL in the runtime executor.
			SupportedParameters: []string{"prompt", "n", "response_format", "image", "mask"},
		},
		{
			ID:                  "grok-imagine-image-quality",
			Object:              "model",
			OwnedBy:             "xai",
			Type:                "xai",
			Version:             "grok-imagine-image-quality",
			DisplayName:         "Grok Imagine Quality",
			Name:                "grok-imagine-image-quality",
			Description:         "Higher fidelity Grok Imagine image generation.",
			SupportedParameters: []string{"prompt", "n", "response_format", "image", "mask"},
		},
	}
}
