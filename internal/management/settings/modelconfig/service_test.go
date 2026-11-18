package modelconfig

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func initModelConfigServiceTestDB(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func TestUpsertConfigPreservesModalitiesWhenOmitted(t *testing.T) {
	initModelConfigServiceTestDB(t)

	inputModalities := []string{"text", "image"}
	outputModalities := []string{"text"}
	if _, err := UpsertConfig(UpsertConfigInput{
		ModelID:          "custom-image",
		Scope:            "active",
		OwnedBy:          "acme-ai",
		Description:      "Custom image model",
		Enabled:          true,
		InputModalities:  &inputModalities,
		OutputModalities: &outputModalities,
		PricingMode:      "call",
		PricePerCall:     0.12,
	}); err != nil {
		t.Fatalf("UpsertConfig() create error = %v", err)
	}

	saved, err := UpsertConfig(UpsertConfigInput{
		ModelID:      "custom-image",
		Scope:        "active",
		OwnedBy:      "acme-ai",
		Description:  "Updated image model",
		Enabled:      true,
		PricingMode:  "call",
		PricePerCall: 0.24,
		OriginalID:   "custom-image",
	})
	if err != nil {
		t.Fatalf("UpsertConfig() update error = %v", err)
	}

	if len(saved.InputModalities) != 2 || saved.InputModalities[1] != "image" {
		t.Fatalf("saved.InputModalities = %#v, want preserved text/image", saved.InputModalities)
	}
	if len(saved.OutputModalities) != 1 || saved.OutputModalities[0] != "text" {
		t.Fatalf("saved.OutputModalities = %#v, want preserved text", saved.OutputModalities)
	}
	if saved.PricePerCall != 0.24 {
		t.Fatalf("saved.PricePerCall = %v, want 0.24", saved.PricePerCall)
	}
}

func TestListConfigsFiltersByScope(t *testing.T) {
	initModelConfigServiceTestDB(t)

	if _, err := UpsertConfig(UpsertConfigInput{
		ModelID:               "custom-active",
		Scope:                 "active",
		OwnedBy:               "acme-ai",
		Description:           "Custom active model",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  1,
		OutputPricePerMillion: 2,
	}); err != nil {
		t.Fatalf("UpsertConfig(active) error = %v", err)
	}
	if _, err := UpsertConfig(UpsertConfigInput{
		ModelID:               "custom-library",
		Scope:                 "library",
		OwnedBy:               "acme-ai",
		Description:           "Custom library model",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  3,
		OutputPricePerMillion: 4,
	}); err != nil {
		t.Fatalf("UpsertConfig(library) error = %v", err)
	}
	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:               "openai/gpt-5.3-codex",
		OwnedBy:               "openai",
		Description:           "OpenRouter synced model",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  1.75,
		OutputPricePerMillion: 14,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("usage.UpsertModelConfig(openrouter) error = %v", err)
	}

	toSources := func(rows []usage.ModelConfigRow) map[string]string {
		t.Helper()
		sources := make(map[string]string, len(rows))
		for _, row := range rows {
			sources[row.ModelID] = row.Source
		}
		return sources
	}

	activeSources := toSources(ListConfigs("active"))
	if activeSources["custom-active"] != "user" {
		t.Fatalf("active custom-active source = %q, want user", activeSources["custom-active"])
	}
	if _, ok := activeSources["custom-library"]; ok {
		t.Fatal("did not expect custom-library in active scope")
	}
	if _, ok := activeSources["openai/gpt-5.3-codex"]; ok {
		t.Fatal("did not expect openrouter model in active scope")
	}

	librarySources := toSources(ListConfigs("library"))
	if librarySources["custom-library"] != "seed" {
		t.Fatalf("library custom-library source = %q, want seed", librarySources["custom-library"])
	}
	if librarySources["openai/gpt-5.3-codex"] != "openrouter" {
		t.Fatalf("library openrouter source = %q, want openrouter", librarySources["openai/gpt-5.3-codex"])
	}
	if _, ok := librarySources["custom-active"]; ok {
		t.Fatal("did not expect custom-active in library scope")
	}

	allSources := toSources(ListConfigs("all"))
	if allSources["custom-active"] != "user" || allSources["custom-library"] != "seed" {
		t.Fatalf("all scope sources = %#v, want custom-active=user custom-library=seed", allSources)
	}
	if allSources["openai/gpt-5.3-codex"] != "openrouter" {
		t.Fatalf("all scope openrouter source = %q, want openrouter", allSources["openai/gpt-5.3-codex"])
	}
}

func TestListOwnerPresetsWithCounts(t *testing.T) {
	initModelConfigServiceTestDB(t)

	for _, row := range []usage.ModelConfigRow{
		{
			ModelID:     "gpt-5.1",
			OwnedBy:     "owner-one",
			Description: "Owner one model",
			Enabled:     true,
			PricingMode: "token",
			Source:      "user",
		},
		{
			ModelID:     "custom-a",
			OwnedBy:     "acme-ai",
			Description: "Acme A",
			Enabled:     true,
			PricingMode: "token",
			Source:      "user",
		},
		{
			ModelID:     "custom-b",
			OwnedBy:     "acme-ai",
			Description: "Acme B",
			Enabled:     true,
			PricingMode: "token",
			Source:      "user",
		},
	} {
		if err := usage.UpsertModelConfig(row); err != nil {
			t.Fatalf("usage.UpsertModelConfig(%s) error = %v", row.ModelID, err)
		}
	}
	if err := ReplaceOwnerPresets([]usage.ModelOwnerPresetRow{
		{Value: "owner-one", Label: "Owner One", Enabled: true},
		{Value: "acme-ai", Label: "Acme AI", Enabled: true},
	}); err != nil {
		t.Fatalf("ReplaceOwnerPresets() error = %v", err)
	}

	items := ListOwnerPresetsWithCounts()
	counts := make(map[string]int, len(items))
	for _, item := range items {
		counts[item.Value] = item.ModelCount
	}

	if counts["owner-one"] != 1 {
		t.Fatalf("counts[owner-one] = %d, want 1", counts["owner-one"])
	}
	if counts["acme-ai"] != 2 {
		t.Fatalf("counts[acme-ai] = %d, want 2", counts["acme-ai"])
	}
}
