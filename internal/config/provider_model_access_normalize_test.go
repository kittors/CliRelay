package config

import "testing"

func TestSanitizeProviderModelAccessRemovesConfiguredModelExclusions(t *testing.T) {
	cfg := &Config{
		OpenCodeGoKey: []OpenCodeGoKey{{
			APIKey:         "go-key",
			Models:         []OpenCodeGoModel{{Name: "qwen3.7-max"}, {Name: "kimi-k2.6"}},
			ExcludedModels: []string{"qwen3.7-max", "kimi-k2.6"},
		}},
		ClineKey: []ClineKey{{
			APIKey:         "cline-key",
			Models:         []ClineModel{{Name: "cline-pass/qwen3.7-max"}, {Name: "cline-pass/kimi-k2.6"}},
			ExcludedModels: []string{"cline-pass/qwen3.7-max", "cline-pass/kimi-k2.6"},
		}},
		OllamaCloudKey: []OllamaCloudKey{{
			APIKey:         "ollama-key",
			Models:         []OllamaCloudModel{{Name: "gpt-oss:120b"}, {Name: "gpt-oss:20b"}},
			ExcludedModels: []string{"gpt-oss:120b", "gpt-oss:20b"},
		}},
	}

	cfg.SanitizeOpenCodeGoKeys()
	cfg.SanitizeClineKeys()
	cfg.SanitizeOllamaCloudKeys()

	if len(cfg.OpenCodeGoKey[0].ExcludedModels) != 0 {
		t.Fatalf("OpenCode Go excluded models = %#v, want empty", cfg.OpenCodeGoKey[0].ExcludedModels)
	}
	if len(cfg.ClineKey[0].ExcludedModels) != 0 {
		t.Fatalf("ClinePass excluded models = %#v, want empty", cfg.ClineKey[0].ExcludedModels)
	}
	if len(cfg.OllamaCloudKey[0].ExcludedModels) != 0 {
		t.Fatalf("Ollama Cloud excluded models = %#v, want empty", cfg.OllamaCloudKey[0].ExcludedModels)
	}
}

func TestSanitizeProviderModelAccessClearsModelsWhenAllAccessDisabled(t *testing.T) {
	cfg := &Config{
		OpenCodeGoKey: []OpenCodeGoKey{{
			APIKey:         "go-key",
			Models:         []OpenCodeGoModel{{Name: "qwen3.7-max"}},
			ExcludedModels: []string{"*"},
		}},
		ClineKey: []ClineKey{{
			APIKey:         "cline-key",
			Models:         []ClineModel{{Name: "cline-pass/qwen3.7-max"}},
			ExcludedModels: []string{"*"},
		}},
		OllamaCloudKey: []OllamaCloudKey{{
			APIKey:         "ollama-key",
			Models:         []OllamaCloudModel{{Name: "gpt-oss:120b"}},
			ExcludedModels: []string{"*"},
		}},
	}

	cfg.SanitizeOpenCodeGoKeys()
	cfg.SanitizeClineKeys()
	cfg.SanitizeOllamaCloudKeys()

	if len(cfg.OpenCodeGoKey[0].Models) != 0 {
		t.Fatalf("OpenCode Go models = %#v, want empty", cfg.OpenCodeGoKey[0].Models)
	}
	if len(cfg.ClineKey[0].Models) != 0 {
		t.Fatalf("ClinePass models = %#v, want empty", cfg.ClineKey[0].Models)
	}
	if len(cfg.OllamaCloudKey[0].Models) != 0 {
		t.Fatalf("Ollama Cloud models = %#v, want empty", cfg.OllamaCloudKey[0].Models)
	}
}
