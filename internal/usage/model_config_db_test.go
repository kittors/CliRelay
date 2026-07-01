package usage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func initModelConfigTestDB(t *testing.T) {
	t.Helper()
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(CloseDB)
}

func TestInitDBSeedsDefaultModelConfigs(t *testing.T) {
	initModelConfigTestDB(t)

	models := ListModelConfigs()
	if len(models) == 0 {
		t.Fatal("expected seeded model configs")
	}

	imageModel, ok := GetModelConfig("gpt-image-2")
	if !ok {
		t.Fatal("expected gpt-image-2 to be seeded")
	}
	if imageModel.PricingMode != "call" {
		t.Fatalf("expected gpt-image-2 pricing mode call, got %q", imageModel.PricingMode)
	}
	if imageModel.PricePerCall <= 0 {
		t.Fatalf("expected gpt-image-2 default per-call price, got %v", imageModel.PricePerCall)
	}
	if len(imageModel.OutputModalities) != 1 || imageModel.OutputModalities[0] != "image" {
		t.Fatalf("expected gpt-image-2 image output modality, got %+v", imageModel.OutputModalities)
	}

	owners := ListModelOwnerPresets()
	if len(owners) == 0 {
		t.Fatal("expected seeded owner presets")
	}
	if _, ok := GetModelOwnerPreset("openai"); !ok {
		t.Fatal("expected openai owner preset")
	}

	opencodeModel, ok := GetModelConfig("qwen3.5-plus")
	if !ok {
		t.Fatal("expected opencode-go qwen3.5-plus to be seeded")
	}
	if opencodeModel.OwnedBy != "opencode" || opencodeModel.Source != "seed" {
		t.Fatalf("unexpected opencode-go seed model config: %+v", opencodeModel)
	}

	clineModel, ok := GetModelConfig("cline-pass/deepseek-v4-flash")
	if !ok {
		t.Fatal("expected cline-pass/deepseek-v4-flash to be seeded")
	}
	if clineModel.OwnedBy != "cline" || clineModel.Source != "seed" {
		t.Fatalf("unexpected cline seed model config: %+v", clineModel)
	}
}

func TestInitDBRepairsCorruptedSeedImageModelConfig(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	CloseDB()

	seedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	seedDB.SetMaxOpenConns(1)
	if _, err := seedDB.Exec(
		`UPDATE model_configs
		 SET pricing_mode = 'token',
		     input_price_per_million = 5,
		     output_price_per_million = 20,
		     cached_price_per_million = 1,
		     cache_read_price_per_million = 2,
		     cache_write_price_per_million = 3,
		     price_per_call = 0,
		     input_modalities = '["text","image"]',
		     output_modalities = '["text"]'
		 WHERE model_id = 'gpt-image-2'`,
	); err != nil {
		t.Fatalf("corrupt gpt-image-2 seed row: %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(CloseDB)

	imageModel, ok := GetModelConfig("gpt-image-2")
	if !ok {
		t.Fatal("expected gpt-image-2 to remain configured")
	}
	if imageModel.PricingMode != "call" || imageModel.PricePerCall != 0.04 {
		t.Fatalf("expected gpt-image-2 call pricing to be repaired, got %+v", imageModel)
	}
	if imageModel.InputPricePerMillion != 0 || imageModel.OutputPricePerMillion != 0 || imageModel.CachedPricePerMillion != 0 {
		t.Fatalf("expected token pricing to be cleared, got %+v", imageModel)
	}
	if imageModel.CacheReadPricePerMillion != 0 || imageModel.CacheWritePricePerMillion != 0 {
		t.Fatalf("expected cache token pricing to be cleared, got %+v", imageModel)
	}
	if len(imageModel.InputModalities) != 1 || imageModel.InputModalities[0] != "text" {
		t.Fatalf("expected text input modality, got %+v", imageModel.InputModalities)
	}
	if len(imageModel.OutputModalities) != 1 || imageModel.OutputModalities[0] != "image" {
		t.Fatalf("expected image-only output modality, got %+v", imageModel.OutputModalities)
	}
}

func TestUpsertModelConfigAndPerCallCost(t *testing.T) {
	initModelConfigTestDB(t)

	err := UpsertModelConfig(ModelConfigRow{
		ModelID:          "custom-image",
		OwnedBy:          "acme-ai",
		Description:      "Custom image model",
		Enabled:          true,
		InputModalities:  []string{"Text", "image", "text"},
		OutputModalities: []string{"Image"},
		PricingMode:      "call",
		PricePerCall:     0.12,
	})
	if err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	model, ok := GetModelConfig("custom-image")
	if !ok {
		t.Fatal("expected custom model config")
	}
	if model.OwnedBy != "acme-ai" || model.PricePerCall != 0.12 {
		t.Fatalf("unexpected model config: %+v", model)
	}
	if len(model.InputModalities) != 2 || model.InputModalities[0] != "text" || model.InputModalities[1] != "image" {
		t.Fatalf("unexpected normalized input modalities: %+v", model.InputModalities)
	}
	if len(model.OutputModalities) != 1 || model.OutputModalities[0] != "image" {
		t.Fatalf("unexpected normalized output modalities: %+v", model.OutputModalities)
	}

	cost := CalculateCost("custom-image", 123, 456, 0)
	if cost != 0.12 {
		t.Fatalf("expected per-call cost 0.12, got %v", cost)
	}
}

func TestDeleteModelConfigRemovesConfigAndPricing(t *testing.T) {
	initModelConfigTestDB(t)

	if err := UpsertModelConfig(ModelConfigRow{
		ModelID:               "temporary-model",
		OwnedBy:               "openai",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  1,
		OutputPricePerMillion: 2,
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	if err := DeleteModelConfig("temporary-model"); err != nil {
		t.Fatalf("DeleteModelConfig() error = %v", err)
	}
	if _, ok := GetModelConfig("temporary-model"); ok {
		t.Fatal("expected model config to be deleted")
	}
	if cost := CalculateCost("temporary-model", 1_000_000, 1_000_000, 0); cost != 0 {
		t.Fatalf("expected deleted model cost 0, got %v", cost)
	}
}

func TestInitDBMergesLegacyPricingWithoutDeadlock(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")

	seedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	seedDB.SetMaxOpenConns(1)
	if _, err := seedDB.Exec(createPricingTableSQL); err != nil {
		t.Fatalf("create legacy pricing table: %v", err)
	}
	if _, err := seedDB.Exec(
		`INSERT INTO model_pricing (model_id, input_price_per_million, output_price_per_million, cached_price_per_million, updated_at)
		 VALUES ('legacy-model', 1.25, 2.5, 0.5, ?)`,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert legacy pricing: %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("InitDB() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("InitDB timed out while merging legacy model pricing")
	}
	t.Cleanup(CloseDB)

	model, ok := GetModelConfig("legacy-model")
	if !ok {
		t.Fatal("expected legacy pricing row to be merged into model_configs")
	}
	if model.PricingMode != "token" || model.InputPricePerMillion != 1.25 || model.OutputPricePerMillion != 2.5 {
		t.Fatalf("unexpected merged model config: %+v", model)
	}
}
