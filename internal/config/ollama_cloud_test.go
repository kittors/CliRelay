package config

import "testing"

func TestSanitizeOllamaCloudKeysClearsModelsWhenAllAccessDisabled(t *testing.T) {
	cfg := &Config{OllamaCloudKey: []OllamaCloudKey{{
		APIKey:         " sk-ollama ",
		BaseURL:        "https://ollama.com/",
		Models:         []OllamaCloudModel{{Name: "gpt-oss:120b"}},
		ExcludedModels: []string{"gpt-oss:20b", "*"},
	}}}

	cfg.SanitizeOllamaCloudKeys()

	if len(cfg.OllamaCloudKey) != 1 {
		t.Fatalf("keys len = %d", len(cfg.OllamaCloudKey))
	}
	got := cfg.OllamaCloudKey[0]
	if len(got.Models) != 0 {
		t.Fatalf("models = %#v, want empty when all model access is disabled", got.Models)
	}
	if len(got.ExcludedModels) != 1 || got.ExcludedModels[0] != "*" {
		t.Fatalf("excluded models = %#v, want disable-all marker only", got.ExcludedModels)
	}
}
