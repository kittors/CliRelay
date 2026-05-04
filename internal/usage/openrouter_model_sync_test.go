package usage

import (
	"context"
	"testing"
)

func TestSyncOpenRouterModelsAddsNewModelsWithLocalModelIDPricingAndOwner(t *testing.T) {
	initModelConfigTestDB(t)

	result, err := SyncOpenRouterModelList(context.Background(), []OpenRouterRemoteModel{
		{
			ID:          "openai/gpt-openrouter-test",
			Name:        "OpenAI: GPT OpenRouter Test",
			Description: "Agentic test model",
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
