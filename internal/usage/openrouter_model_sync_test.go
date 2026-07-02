package usage

import (
	"context"
	"strings"
	"testing"
)

func TestSyncOpenRouterModelsAddsNewModelsWithLocalModelIDPricingAndOwner(t *testing.T) {
	initModelConfigTestDB(t)

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "openai/gpt-openrouter-test",
			Name:        "OpenAI: GPT OpenRouter Test",
			Description: "Agentic test model",
			Architecture: OpenRouterRemoteArchitecture{
				Modality:         "text+image->text",
				InputModalities:  []string{"text", "image"},
				OutputModalities: []string{"text"},
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.00000175",
				Completion:     "0.000014",
				InputCacheRead: "0.000000175",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	model, ok := GetModelConfig("gpt-openrouter-test")
	if !ok {
		t.Fatal("expected gpt-openrouter-test to be imported")
	}
	if _, ok := GetModelConfig("openai/gpt-openrouter-test"); ok {
		t.Fatal("did not expect OpenRouter provider prefix to be stored in model id")
	}
	if model.OwnedBy != "openai" || model.Source != "openrouter" || model.Description != "Agentic test model" {
		t.Fatalf("unexpected imported model metadata: %+v", model)
	}
	if model.InputPricePerMillion != 1.75 || model.OutputPricePerMillion != 14 || model.CachedPricePerMillion != 0.175 {
		t.Fatalf("unexpected imported model pricing: %+v", model)
	}
	if strings.Join(model.InputModalities, ",") != "text,image" || strings.Join(model.OutputModalities, ",") != "text" {
		t.Fatalf("unexpected imported model modalities: %+v -> %+v", model.InputModalities, model.OutputModalities)
	}
	if _, ok := GetModelOwnerPreset("openai"); !ok {
		t.Fatal("expected openai owner preset to exist")
	}
}

func TestSyncOpenRouterModelsUpdatesExistingUserModelPricingOnly(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "gpt-openrouter-test",
		OwnedBy:               "custom-owner",
		Description:           "Local override",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  9,
		OutputPricePerMillion: 18,
		Source:                "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "openai/gpt-openrouter-test",
			Description: "Remote description",
			Architecture: OpenRouterRemoteArchitecture{
				Modality: "text+image->text",
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.00000175",
				Completion:     "0.000014",
				InputCacheRead: "0.000000175",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 0 || result.Updated != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	model, ok := GetModelConfig("gpt-openrouter-test")
	if !ok {
		t.Fatal("expected existing model config")
	}
	if model.OwnedBy != "custom-owner" || model.Description != "Local override" || model.Source != "user" {
		t.Fatalf("existing user metadata should not be overwritten: %+v", model)
	}
	if model.InputPricePerMillion != 1.75 || model.OutputPricePerMillion != 14 || model.CachedPricePerMillion != 0.175 {
		t.Fatalf("existing user model pricing should be synced: %+v", model)
	}
	if strings.Join(model.InputModalities, ",") != "text,image" || strings.Join(model.OutputModalities, ",") != "text" {
		t.Fatalf("existing user model modalities should be synced: %+v -> %+v", model.InputModalities, model.OutputModalities)
	}
}

func TestSyncOpenRouterModelsPreservesImageGenerationCallPricingAndOutputModality(t *testing.T) {
	initModelConfigTestDB(t)

	before, ok := GetModelConfig("gpt-image-2")
	if !ok {
		t.Fatal("expected gpt-image-2 seed config")
	}
	if before.PricingMode != "call" || before.PricePerCall <= 0 {
		t.Fatalf("expected seeded gpt-image-2 call pricing, got %+v", before)
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "openai/gpt-image-2",
			Description: "Remote image model description",
			Architecture: OpenRouterRemoteArchitecture{
				Modality:         "text->text",
				InputModalities:  []string{"text"},
				OutputModalities: []string{"text"},
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000005",
				Completion: "0.000020",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 0 || result.Updated != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	after, ok := GetModelConfig("gpt-image-2")
	if !ok {
		t.Fatal("expected gpt-image-2 to remain configured")
	}
	if after.PricingMode != "call" || after.PricePerCall != before.PricePerCall {
		t.Fatalf("image generation pricing should remain per-call, before=%+v after=%+v", before, after)
	}
	if after.InputPricePerMillion != 0 || after.OutputPricePerMillion != 0 || after.CachedPricePerMillion != 0 {
		t.Fatalf("image generation model should not be converted to token pricing: %+v", after)
	}
	if !containsModality(after.OutputModalities, "image") {
		t.Fatalf("image generation model should keep image output modality: %+v", after.OutputModalities)
	}
	if containsModality(after.OutputModalities, "text") {
		t.Fatalf("image generation model should not inherit misleading text output modality: %+v", after.OutputModalities)
	}
	if cost := CalculateCost("gpt-image-2", 1_000_000, 1_000_000, 0); cost != before.PricePerCall {
		t.Fatalf("expected CalculateCost to use per-call pricing %v, got %v", before.PricePerCall, cost)
	}
}

func TestSyncOpenRouterModelsBackfillsImageGenerationDefaultCallPrice(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "gpt-image-2",
		OwnedBy:               "codex",
		Description:           "Previously corrupted image model",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  5,
		OutputPricePerMillion: 20,
		PricePerCall:          0,
		Source:                "seed",
		InputModalities:       []string{"text"},
		OutputModalities:      []string{"text"},
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	_, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "openai/gpt-image-2",
			Description: "Remote image model description",
			Architecture: OpenRouterRemoteArchitecture{
				InputModalities:  []string{"text"},
				OutputModalities: []string{"text"},
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000005",
				Completion: "0.000020",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}

	model, ok := GetModelConfig("gpt-image-2")
	if !ok {
		t.Fatal("expected gpt-image-2 config")
	}
	if model.PricingMode != "call" || model.PricePerCall != 0.04 {
		t.Fatalf("expected gpt-image-2 default call pricing to be backfilled, got %+v", model)
	}
	if model.InputPricePerMillion != 0 || model.OutputPricePerMillion != 0 {
		t.Fatalf("expected token pricing to be cleared, got %+v", model)
	}
	if !containsModality(model.OutputModalities, "image") || containsModality(model.OutputModalities, "text") {
		t.Fatalf("expected image-only output modality, got %+v", model.OutputModalities)
	}
}

func TestSyncOpenRouterModelsUpdatesExistingClinePassWrapperFromCanonicalModel(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:     "cline-pass/deepseek-v4-flash",
		OwnedBy:     "cline",
		Description: "",
		Enabled:     true,
		PricingMode: "token",
		Source:      "seed",
		InputModalities: []string{
			"text",
		},
		OutputModalities: []string{
			"text",
		},
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "deepseek/deepseek-v4-flash",
			Description: "DeepSeek V4 Flash from OpenRouter",
			Architecture: OpenRouterRemoteArchitecture{
				Modality:         "text+image->text",
				InputModalities:  []string{"text", "image"},
				OutputModalities: []string{"text"},
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.0000003",
				Completion:     "0.0000012",
				InputCacheRead: "0.00000003",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	model, ok := GetModelConfig("cline-pass/deepseek-v4-flash")
	if !ok {
		t.Fatal("expected existing cline-pass wrapper to remain configured")
	}
	if model.OwnedBy != "cline" || model.Source != "seed" {
		t.Fatalf("cline wrapper identity should be preserved: %+v", model)
	}
	if model.Description != "DeepSeek V4 Flash from OpenRouter" {
		t.Fatalf("cline wrapper should inherit remote description, got %q", model.Description)
	}
	if model.InputPricePerMillion != 0.3 || model.OutputPricePerMillion != 1.2 || model.CachedPricePerMillion != 0.03 {
		t.Fatalf("cline wrapper should inherit canonical pricing: %+v", model)
	}
	if strings.Join(model.InputModalities, ",") != "text,image" || strings.Join(model.OutputModalities, ",") != "text" {
		t.Fatalf("cline wrapper should inherit canonical modalities: %+v -> %+v", model.InputModalities, model.OutputModalities)
	}
}

func TestSyncOpenRouterModelsUpdatesExistingOpenRouterDescription(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "gpt-openrouter-test",
		OwnedBy:               "openai",
		Description:           "Old OpenRouter description",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  9,
		OutputPricePerMillion: 18,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "openai/gpt-openrouter-test",
			Description: "Fresh remote description",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.00000175",
				Completion: "0.000014",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 0 || result.Updated != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	model, ok := GetModelConfig("gpt-openrouter-test")
	if !ok {
		t.Fatal("expected existing OpenRouter model config")
	}
	if model.Description != "Fresh remote description" {
		t.Fatalf("existing OpenRouter description should be refreshed, got %q", model.Description)
	}
}

func TestSyncOpenRouterModelsFillsEmptyUserDescription(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "gpt-openrouter-test",
		OwnedBy:               "custom-owner",
		Description:           "",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  9,
		OutputPricePerMillion: 18,
		Source:                "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "openai/gpt-openrouter-test",
			Description: "Remote description",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.00000175",
				Completion: "0.000014",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 0 || result.Updated != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	model, ok := GetModelConfig("gpt-openrouter-test")
	if !ok {
		t.Fatal("expected existing model config")
	}
	if model.Description != "Remote description" || model.Source != "user" {
		t.Fatalf("empty user description should be filled without changing source: %+v", model)
	}
}

func TestSyncOpenRouterModelsStripsProviderPrefixAndTildeFromImportedModelID(t *testing.T) {
	initModelConfigTestDB(t)

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "~moonshotai/kimi-latest",
			Description: "Moonshot latest alias",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.0000007448",
				Completion: "0.000004655",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 1 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	model, ok := GetModelConfig("kimi-latest")
	if !ok {
		t.Fatal("expected OpenRouter alias model to be imported with a local model id")
	}
	if model.ModelID != "kimi-latest" {
		t.Fatalf("model id should strip OpenRouter provider prefix, got %q", model.ModelID)
	}
	if _, ok := GetModelConfig("~moonshotai/kimi-latest"); ok {
		t.Fatal("did not expect OpenRouter alias marker to be stored in model id")
	}
	if model.OwnedBy != "moonshotai" {
		t.Fatalf("owner should not keep OpenRouter alias marker, got %q", model.OwnedBy)
	}
}

func TestSyncOpenRouterModelsNormalizesAnthropicVersionDots(t *testing.T) {
	initModelConfigTestDB(t)

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "anthropic/claude-sonnet-4.6",
			Description: "Claude Sonnet 4.6",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000003",
				Completion: "0.000015",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 0 || result.Updated != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	model, ok := GetModelConfig("claude-sonnet-4-6")
	if !ok {
		t.Fatal("expected anthropic model to use the local Claude id")
	}
	if model.OwnedBy != "anthropic" || model.InputPricePerMillion != 3 || model.OutputPricePerMillion != 15 {
		t.Fatalf("unexpected normalized anthropic model: %+v", model)
	}
	if _, ok := GetModelConfig("claude-sonnet-4.6"); ok {
		t.Fatal("did not expect dotted Anthropic version id to be stored")
	}
}

func TestSyncOpenRouterModelsUsesAnthropicDateSuffixBaseModelID(t *testing.T) {
	initModelConfigTestDB(t)

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "anthropic/claude-3-5-haiku-20241022",
			Description: "Fast Claude Haiku model from OpenRouter",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.0000008",
				Completion:     "0.000004",
				InputCacheRead: "0.00000008",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 1 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	baseModel, ok := GetModelConfig("claude-3-5-haiku")
	if !ok {
		t.Fatal("expected Anthropic dated OpenRouter id to sync into the base Claude id")
	}
	if baseModel.OwnedBy != "anthropic" || baseModel.Description != "Fast Claude Haiku model from OpenRouter" {
		t.Fatalf("unexpected base Claude metadata: %+v", baseModel)
	}
	if baseModel.InputPricePerMillion != 0.8 || baseModel.OutputPricePerMillion != 4 || baseModel.CachedPricePerMillion != 0.08 {
		t.Fatalf("unexpected base Claude pricing: %+v", baseModel)
	}

	datedModel, ok := GetModelConfig("claude-3-5-haiku-20241022")
	if !ok {
		t.Fatal("expected seeded dated Claude id to remain available")
	}
	if datedModel.InputPricePerMillion != 0.8 || datedModel.OutputPricePerMillion != 4 || datedModel.CachedPricePerMillion != 0.08 {
		t.Fatalf("dated Claude alias should reuse base pricing: %+v", datedModel)
	}
	if datedModel.Description != "Fast Claude Haiku model from OpenRouter" {
		t.Fatalf("dated Claude alias should reuse base description, got %q", datedModel.Description)
	}
}

func TestSyncOpenRouterModelsUpdatesAnthropicDatedAliasFromBaseRemoteID(t *testing.T) {
	initModelConfigTestDB(t)

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "anthropic/claude-3.5-haiku",
			Description: "Fast Claude Haiku model from OpenRouter",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.0000008",
				Completion:     "0.000004",
				InputCacheRead: "0.00000008",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 1 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	baseModel, ok := GetModelConfig("claude-3-5-haiku")
	if !ok {
		t.Fatal("expected base Claude model to be imported")
	}
	if baseModel.InputPricePerMillion != 0.8 || baseModel.OutputPricePerMillion != 4 || baseModel.CachedPricePerMillion != 0.08 {
		t.Fatalf("unexpected base Claude pricing: %+v", baseModel)
	}

	datedModel, ok := GetModelConfig("claude-3-5-haiku-20241022")
	if !ok {
		t.Fatal("expected seeded dated Claude alias to remain available")
	}
	if datedModel.InputPricePerMillion != 0.8 || datedModel.OutputPricePerMillion != 4 || datedModel.CachedPricePerMillion != 0.08 {
		t.Fatalf("dated Claude alias should reuse base remote pricing: %+v", datedModel)
	}
	if datedModel.Description != "Fast Claude Haiku model from OpenRouter" {
		t.Fatalf("dated Claude alias should reuse base remote description, got %q", datedModel.Description)
	}
}

func TestSyncOpenRouterModelsPreservesExistingOpenRouterDatedAliasFromBaseRemoteID(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "claude-3-5-haiku-20241022",
		OwnedBy:               "anthropic",
		Description:           "Old OpenRouter dated alias",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0,
		OutputPricePerMillion: 0,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "anthropic/claude-3.5-haiku",
			Description: "Fast Claude Haiku model from OpenRouter",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.0000008",
				Completion:     "0.000004",
				InputCacheRead: "0.00000008",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 1 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	datedModel, ok := GetModelConfig("claude-3-5-haiku-20241022")
	if !ok {
		t.Fatal("expected existing OpenRouter dated alias to remain available")
	}
	if datedModel.Source != "openrouter" || datedModel.Description != "Fast Claude Haiku model from OpenRouter" {
		t.Fatalf("dated OpenRouter alias metadata should be refreshed: %+v", datedModel)
	}
	if datedModel.InputPricePerMillion != 0.8 || datedModel.OutputPricePerMillion != 4 || datedModel.CachedPricePerMillion != 0.08 {
		t.Fatalf("dated OpenRouter alias should reuse base remote pricing: %+v", datedModel)
	}
}

func TestSyncOpenRouterModelsMigratesExistingOpenRouterPrefixedRows(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "openai/gpt-openrouter-legacy",
		OwnedBy:               "openai",
		Description:           "Existing prefixed import",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  9,
		OutputPricePerMillion: 18,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "openai/gpt-openrouter-legacy",
			Description: "Remote description",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000002",
				Completion: "0.000008",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 0 || result.Updated != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	model, ok := GetModelConfig("gpt-openrouter-legacy")
	if !ok {
		t.Fatal("expected existing OpenRouter row to be migrated to local model id")
	}
	if _, ok := GetModelConfig("openai/gpt-openrouter-legacy"); ok {
		t.Fatal("did not expect old prefixed OpenRouter row to remain")
	}
	if model.Description != "Remote description" || model.Source != "openrouter" || model.OwnedBy != "openai" {
		t.Fatalf("existing OpenRouter metadata should be refreshed during migration: %+v", model)
	}
	if model.InputPricePerMillion != 2 || model.OutputPricePerMillion != 8 {
		t.Fatalf("existing OpenRouter pricing should be synced: %+v", model)
	}
}

func TestOpenRouterPricePerMillionRoundsFloatArtifacts(t *testing.T) {
	if got := openRouterPricePerMillion("0.0000002"); got != 0.2 {
		t.Fatalf("expected clean per-million price, got %.17g", got)
	}
}

func TestOpenRouterStripDateSuffix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"qwen3.5-plus-20260420", "qwen3.5-plus"},
		{"claude-3-5-haiku-20241022", "claude-3-5-haiku"},
		{"deepseek-r1-20250101", "deepseek-r1"},
		{"qwen3.5-plus-02-15", "qwen3.5-plus"},
		{"model-name-12-31", "model-name"},
		{"model-2026-04-20", "model"},
		{"test-2025-01-01", "test"},
		{"qwen3.5-plus", "qwen3.5-plus"},
		{"qwen3.5-plus-thinking", "qwen3.5-plus-thinking"},
		{"qwen3.5-max", "qwen3.5-max"},
		{"gpt-4-turbo", "gpt-4-turbo"},
		{"gpt-4o", "gpt-4o"},
		{"deepseek-r1", "deepseek-r1"},
		{"", ""},
		{"model-v2", "model-v2"},
		{"claude-3-5-haiku", "claude-3-5-haiku"},
	}
	for _, tt := range tests {
		got := openRouterStripDateSuffix(tt.input)
		if got != tt.expected {
			t.Errorf("openRouterStripDateSuffix(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// seedBaseModelForTest creates a base model with zero pricing so variant-merging
// tests can verify that the base is updated correctly.
func seedBaseModelForTest(t *testing.T, modelID, owner string) {
	t.Helper()
	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               modelID,
		OwnedBy:               owner,
		Description:           "",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0,
		OutputPricePerMillion: 0,
		Source:                "seed",
	}); err != nil {
		t.Fatalf("UpsertModelConfig(%s) error = %v", modelID, err)
	}
}

func TestSyncOpenRouterModelsQwen8DigitDateSuffixUpdatesBaseModel(t *testing.T) {
	initModelConfigTestDB(t)
	seedBaseModelForTest(t, "qwen3.5-plus", "qwen")

	baseModel, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus to exist after seeding")
	}
	if baseModel.InputPricePerMillion != 0 || baseModel.OutputPricePerMillion != 0 {
		t.Fatalf("expected seeded qwen3.5-plus to have zero pricing, got %+v", baseModel)
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "qwen/qwen3.5-plus-20260420",
			Name:        "Qwen 3.5 Plus (2026-04-20)",
			Description: "Qwen 3.5 Plus with 8-digit date suffix",
			Architecture: OpenRouterRemoteArchitecture{
				Modality:         "text+image->text",
				InputModalities:  []string{"text", "image"},
				OutputModalities: []string{"text"},
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.00000175",
				Completion:     "0.000014",
				InputCacheRead: "0.000000175",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 1 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	if _, ok := GetModelConfig("qwen3.5-plus-20260420"); !ok {
		t.Fatal("expected variant qwen3.5-plus-20260420 to exist as a separate row")
	}

	updated, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus to still exist")
	}
	if updated.InputPricePerMillion != 1.75 || updated.OutputPricePerMillion != 14 || updated.CachedPricePerMillion != 0.175 {
		t.Fatalf("expected qwen3.5-plus pricing to be synced from variant, got %+v", updated)
	}
	if strings.Join(updated.InputModalities, ",") != "text,image" || strings.Join(updated.OutputModalities, ",") != "text" {
		t.Fatalf("expected qwen3.5-plus modalities to be synced from variant, got input=%v output=%v",
			updated.InputModalities, updated.OutputModalities)
	}
	if updated.Description != "Qwen 3.5 Plus with 8-digit date suffix" {
		t.Fatalf("expected qwen3.5-plus description to be synced from variant, got %q", updated.Description)
	}
	if updated.OwnedBy != "qwen" {
		t.Fatalf("expected qwen3.5-plus owner to be qwen, got %q", updated.OwnedBy)
	}

	// Variant ID should have non-zero billing via CalculateCost
	if cost := CalculateCost("qwen3.5-plus-20260420", 1_000_000, 0, 0); cost != 1.75 {
		t.Fatalf("expected CalculateCost(variant, 1M input) = 1.75, got %v", cost)
	}
	if cost := CalculateCost("qwen3.5-plus-20260420", 0, 1_000_000, 0); cost != 14 {
		t.Fatalf("expected CalculateCost(variant, 1M output) = 14, got %v", cost)
	}
	if cost := CalculateCost("qwen3.5-plus-20260420", 0, 0, 1_000_000); cost != 0.175 {
		t.Fatalf("expected CalculateCost(variant, 1M cached) = 0.175, got %v", cost)
	}
	// Base model billing should also work after merge
	if cost := CalculateCost("qwen3.5-plus", 1_000_000, 0, 0); cost != 1.75 {
		t.Fatalf("expected CalculateCost(base, 1M input) = 1.75, got %v", cost)
	}
}

func TestSyncOpenRouterModelsCreatesKnownStaticBaseModelWhenMissing(t *testing.T) {
	initModelConfigTestDB(t)

	if _, ok := GetModelConfig("qwen3.5-plus"); !ok {
		t.Fatal("expected qwen3.5-plus to be seeded before missing-base simulation")
	}
	if err := DeleteModelConfig("qwen3.5-plus"); err != nil {
		t.Fatalf("DeleteModelConfig(qwen3.5-plus) error = %v", err)
	}
	if _, ok := GetModelConfig("qwen3.5-plus"); ok {
		t.Fatal("expected qwen3.5-plus to be absent before OpenRouter sync")
	}

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "qwen/qwen3.5-plus-20260420",
			Name:        "Qwen 3.5 Plus (2026-04-20)",
			Description: "Qwen 3.5 Plus OpenRouter variant",
			Architecture: OpenRouterRemoteArchitecture{
				Modality: "text+image->text",
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.0000003",
				Completion:     "0.0000018",
				InputCacheRead: "0",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	variant, ok := GetModelConfig("qwen3.5-plus-20260420")
	if !ok {
		t.Fatal("expected qwen3.5-plus-20260420 variant row to exist")
	}
	if variant.InputPricePerMillion != 0.3 || variant.OutputPricePerMillion != 1.8 {
		t.Fatalf("unexpected variant pricing: %+v", variant)
	}

	base, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus base row to be recreated from static model metadata")
	}
	if base.Source != "seed" || base.OwnedBy != "opencode" {
		t.Fatalf("expected recreated base to keep static seed metadata, got %+v", base)
	}
	if base.InputPricePerMillion != 0.3 || base.OutputPricePerMillion != 1.8 {
		t.Fatalf("expected recreated base pricing from variant, got %+v", base)
	}
	if strings.Join(base.InputModalities, ",") != "text,image" || strings.Join(base.OutputModalities, ",") != "text" {
		t.Fatalf("expected recreated base modalities from variant, got input=%v output=%v",
			base.InputModalities, base.OutputModalities)
	}
	if cost := CalculateCost("qwen3.5-plus", 1_000_000, 1_000_000, 0); cost != 2.1 {
		t.Fatalf("expected CalculateCost(recreated base) = 2.1, got %v", cost)
	}
}

func TestSyncOpenRouterModelsQwenSegmentedDateSuffixUpdatesBaseModel(t *testing.T) {
	initModelConfigTestDB(t)
	seedBaseModelForTest(t, "qwen3.5-plus", "qwen")

	_, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "qwen/qwen3.5-plus-02-15",
			Name:        "Qwen 3.5 Plus (Feb 15)",
			Description: "Qwen 3.5 Plus with MM-DD suffix",
			Architecture: OpenRouterRemoteArchitecture{
				Modality: "text->text",
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000002",
				Completion: "0.000015",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}

	updated, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus to exist")
	}
	if updated.InputPricePerMillion != 2 || updated.OutputPricePerMillion != 15 {
		t.Fatalf("expected qwen3.5-plus pricing from MM-DD variant, got %+v", updated)
	}
}

func TestSyncOpenRouterModelsVariantGroupHighestPricing(t *testing.T) {
	initModelConfigTestDB(t)
	seedBaseModelForTest(t, "qwen3.5-plus", "qwen")

	// Both variants are stored with their full IDs. The merge pass groups
	// them by canonical base ID and aggregates the highest prices.
	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "qwen/qwen3.5-plus-20260420",
			Name:        "High price variant",
			Description: "Qwen 3.5 Plus high price",
			Architecture: OpenRouterRemoteArchitecture{
				Modality: "text+image->text",
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000003",
				Completion: "0.000020",
			},
		},
		{
			ID:          "qwen/qwen3.5-plus-02-15",
			Name:        "Low price variant",
			Description: "Qwen 3.5 Plus low price",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000001",
				Completion: "0.000008",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 2 || result.Added != 2 {
		t.Fatalf("expected 2 seen and 2 added (one per variant), got %+v", result)
	}

	updated, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus to exist")
	}
	if updated.InputPricePerMillion != 3 {
		t.Fatalf("expected highest input price (3), got %v", updated.InputPricePerMillion)
	}
	if updated.OutputPricePerMillion != 20 {
		t.Fatalf("expected highest output price (20), got %v", updated.OutputPricePerMillion)
	}
}

func TestSyncOpenRouterModelsVariantGroupTwoVariantsSecondHasHigherCachedPrice(t *testing.T) {
	initModelConfigTestDB(t)
	seedBaseModelForTest(t, "qwen3.5-plus", "qwen")

	_, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "qwen/qwen3.5-plus-20260420",
			Description: "Qwen 3.5 Plus v1",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.000003",
				Completion:     "0.000020",
				InputCacheRead: "0.0000001",
			},
		},
		{
			ID:          "qwen/qwen3.5-plus-02-15",
			Description: "Qwen 3.5 Plus v2",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.000001",
				Completion:     "0.000008",
				InputCacheRead: "0.0000005",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}

	updated, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus to exist")
	}
	if updated.InputPricePerMillion != 3 {
		t.Fatalf("expected input price 3 (from variant 1), got %v", updated.InputPricePerMillion)
	}
	if updated.OutputPricePerMillion != 20 {
		t.Fatalf("expected output price 20 (from variant 1), got %v", updated.OutputPricePerMillion)
	}
	if updated.CachedPricePerMillion != 0.5 {
		t.Fatalf("expected cached price 0.5 (from variant 2), got %v", updated.CachedPricePerMillion)
	}
}

func TestSyncOpenRouterModelsPreservesNonDateSuffix(t *testing.T) {
	initModelConfigTestDB(t)
	seedBaseModelForTest(t, "qwen3.5-plus", "qwen")

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "qwen/qwen3.5-plus-thinking",
			Description: "Qwen 3.5 Plus Thinking",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000002",
				Completion: "0.000010",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	if result.Seen != 1 || result.Added != 1 {
		t.Fatalf("expected qwen3.5-plus-thinking to be added as new model, got %+v", result)
	}
	if _, ok := GetModelConfig("qwen3.5-plus-thinking"); !ok {
		t.Fatal("expected qwen3.5-plus-thinking to exist")
	}
	base, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus to exist")
	}
	if base.InputPricePerMillion != 0 || base.OutputPricePerMillion != 0 {
		t.Fatalf("expected qwen3.5-plus pricing to remain zero (not affected by -thinking variant), got %+v", base)
	}
}

func TestSyncOpenRouterModelsExactBaseMatchBeforeVariant(t *testing.T) {
	initModelConfigTestDB(t)

	// The exact-match Qwen model creates qwen3.5-plus first, then the variant
	// overwrites. The merge pass restores the higher prices from the exact match.
	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "chat/qwen3.5-plus",
			Name:        "Qwen 3.5 Plus (exact)",
			Description: "Exact match description",
			Architecture: OpenRouterRemoteArchitecture{
				Modality: "text+image->text",
			},
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000003",
				Completion: "0.000020",
			},
		},
		{
			ID:          "qwen/qwen3.5-plus-20260420",
			Name:        "Qwen 3.5 Plus (dated)",
			Description: "Lower price dated variant",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000001",
				Completion: "0.000008",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}
	// qwen3.5-plus is a seeded OpenCode Go model, so the exact match updates
	// the base row and the dated variant is added as a separate OpenRouter row.
	if result.Seen != 2 || result.Added != 1 || result.Updated != 1 {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	updated, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus to exist")
	}
	if updated.InputPricePerMillion != 3 || updated.OutputPricePerMillion != 20 {
		t.Fatalf("expected highest pricing across both variants, got input=%v output=%v (expected 3, 20)",
			updated.InputPricePerMillion, updated.OutputPricePerMillion)
	}
}

func TestSyncOpenRouterModelsUserDescriptionNotOverwrittenByVariantMerge(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "qwen3.5-plus",
		OwnedBy:               "custom-owner",
		Description:           "User defined description",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  5,
		OutputPricePerMillion: 10,
		Source:                "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	_, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "qwen/qwen3.5-plus-20260420",
			Description: "OpenRouter variant description",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000002",
				Completion: "0.000015",
			},
		},
		{
			ID:          "qwen/qwen3.5-plus-02-15",
			Description: "Another variant description",
			Pricing: OpenRouterRemotePricing{
				Prompt:     "0.000003",
				Completion: "0.000020",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}

	model, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected qwen3.5-plus to exist")
	}
	if model.Description != "User defined description" {
		t.Fatalf("user description should NOT be overwritten by variant merge, got %q", model.Description)
	}
	if model.Source != "user" {
		t.Fatalf("user source should be preserved, got %q", model.Source)
	}
}

func TestSyncOpenRouterModelsAnthropicDottedExactFirstThenDatedVariant(t *testing.T) {
	initModelConfigTestDB(t)

	// Dotted exact base first (higher price), then dated variant (lower price).
	// The merge pass must keep the highest prices.
	_, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "anthropic/claude-3.5-haiku",
			Description: "Claude 3.5 Haiku (exact, high price)",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.000001",
				Completion:     "0.000005",
				InputCacheRead: "0.0000001",
			},
		},
		{
			ID:          "anthropic/claude-3-5-haiku-20241022",
			Description: "Claude 3.5 Haiku (dated, low price)",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.0000008",
				Completion:     "0.000004",
				InputCacheRead: "0.00000008",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}

	base, ok := GetModelConfig("claude-3-5-haiku")
	if !ok {
		t.Fatal("expected claude-3-5-haiku to exist")
	}
	// Should retain highest prices from exact match (not overwritten by dated variant)
	if base.InputPricePerMillion != 1 || base.OutputPricePerMillion != 5 || base.CachedPricePerMillion != 0.1 {
		t.Fatalf("expected highest prices across both variants, got input=%v output=%v cached=%v",
			base.InputPricePerMillion, base.OutputPricePerMillion, base.CachedPricePerMillion)
	}
}

func TestSyncOpenRouterModelsAnthropicDatedVariantFirstThenDottedExact(t *testing.T) {
	initModelConfigTestDB(t)

	// Dated variant first (lower price), then dotted exact base (higher price).
	// The merge pass must keep the highest prices regardless of order.
	_, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "anthropic/claude-3-5-haiku-20241022",
			Description: "Claude 3.5 Haiku (dated, low price)",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.0000008",
				Completion:     "0.000004",
				InputCacheRead: "0.00000008",
			},
		},
		{
			ID:          "anthropic/claude-3.5-haiku",
			Description: "Claude 3.5 Haiku (exact, high price)",
			Pricing: OpenRouterRemotePricing{
				Prompt:         "0.000001",
				Completion:     "0.000005",
				InputCacheRead: "0.0000001",
			},
		},
	})
	if err != nil {
		t.Fatalf("SyncOpenRouterModelList() error = %v", err)
	}

	base, ok := GetModelConfig("claude-3-5-haiku")
	if !ok {
		t.Fatal("expected claude-3-5-haiku to exist")
	}
	// Should retain highest prices from exact match (not overwritten by dated variant)
	if base.InputPricePerMillion != 1 || base.OutputPricePerMillion != 5 || base.CachedPricePerMillion != 0.1 {
		t.Fatalf("expected highest prices across both variants regardless of order, got input=%v output=%v cached=%v",
			base.InputPricePerMillion, base.OutputPricePerMillion, base.CachedPricePerMillion)
	}
}

func TestUnionModalities(t *testing.T) {
	tests := []struct {
		name     string
		a, b     []string
		expected []string
	}{
		{
			name:     "identical",
			a:        []string{"text", "image"},
			b:        []string{"text", "image"},
			expected: []string{"text", "image"},
		},
		{
			name:     "different content same length",
			a:        []string{"text", "image"},
			b:        []string{"text", "video"},
			expected: []string{"text", "image", "video"},
		},
		{
			name:     "a superset of b",
			a:        []string{"text", "image", "video"},
			b:        []string{"text"},
			expected: []string{"text", "image", "video"},
		},
		{
			name:     "b superset of a",
			a:        []string{"text"},
			b:        []string{"text", "image", "video"},
			expected: []string{"text", "image", "video"},
		},
		{
			name:     "empty a",
			a:        nil,
			b:        []string{"text"},
			expected: []string{"text"},
		},
		{
			name:     "empty b",
			a:        []string{"text"},
			b:        nil,
			expected: []string{"text"},
		},
		{
			name:     "both empty",
			a:        nil,
			b:        nil,
			expected: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unionModalities(tt.a, tt.b)
			if len(got) != len(tt.expected) {
				t.Fatalf("unionModalities(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.expected)
			}
			seen := make(map[string]struct{}, len(got))
			for _, m := range got {
				seen[m] = struct{}{}
			}
			for _, m := range tt.expected {
				if _, ok := seen[m]; !ok {
					t.Fatalf("unionModalities(%v, %v) = %v, missing expected %q", tt.a, tt.b, got, m)
				}
			}
		})
	}
}

func containsModality(modalities []string, expected string) bool {
	for _, modality := range modalities {
		if strings.EqualFold(strings.TrimSpace(modality), expected) {
			return true
		}
	}
	return false
}
