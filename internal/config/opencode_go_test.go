package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigReadsOpenCodeGoKeysWithoutBaseURL(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
opencode-go-api-key:
  - api-key: " go-key-1 "
    name: " primary "
    prefix: " team-a "
    proxy-url: " http://127.0.0.1:7890 "
    proxy-id: " hk "
    headers:
      X-Test: " yes "
    excluded-models:
      - " deepseek-v4-pro "
      - " * "
    models:
      - name: " qwen3.7-max "
      - name: " qwen3.7-max "
      - name: " kimi-k2.7-code "
    vision-fallback-model: " qwen3.5-plus "
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(cfg.OpenCodeGoKey) != 1 {
		t.Fatalf("OpenCodeGoKey length = %d, want 1", len(cfg.OpenCodeGoKey))
	}
	got := cfg.OpenCodeGoKey[0]
	if got.APIKey != "go-key-1" {
		t.Fatalf("api key = %q, want trimmed key", got.APIKey)
	}
	if got.Name != "primary" || got.Prefix != "team-a" || got.ProxyURL != "http://127.0.0.1:7890" || got.ProxyID != "hk" {
		t.Fatalf("entry was not normalized: %+v", got)
	}
	if got.Headers["X-Test"] != "yes" {
		t.Fatalf("headers = %#v, want normalized X-Test", got.Headers)
	}
	if len(got.ExcludedModels) != 1 || got.ExcludedModels[0] != "*" {
		t.Fatalf("excluded models = %#v", got.ExcludedModels)
	}
	if len(got.Models) != 2 || got.Models[0].Name != "qwen3.7-max" || got.Models[1].Name != "kimi-k2.7-code" {
		t.Fatalf("models = %#v, want normalized unique models", got.Models)
	}
	if got.VisionFallbackModel != "qwen3.5-plus" {
		t.Fatalf("vision fallback model = %q, want qwen3.5-plus", got.VisionFallbackModel)
	}
}

func TestSanitizeOpenCodeGoKeysDropsEmptyAndDeduplicates(t *testing.T) {
	cfg := &Config{
		OpenCodeGoKey: []OpenCodeGoKey{
			{APIKey: " "},
			{APIKey: "go-key", Prefix: " team "},
			{APIKey: "go-key", Prefix: "duplicate"},
			{APIKey: "go-key-2", Headers: map[string]string{" X-Trace ": " on "}, Models: []OpenCodeGoModel{{Name: " glm-5.2 "}, {Name: "GLM-5.2"}, {Name: " "}}, ExcludedModels: []string{"glm-5.2", "*"}, VisionFallbackModel: " qwen3.6-plus ", WorkspaceID: " wrk_123 ", AuthCookie: " auth-token "},
		},
	}

	cfg.SanitizeOpenCodeGoKeys()

	if len(cfg.OpenCodeGoKey) != 2 {
		t.Fatalf("OpenCodeGoKey length = %d, want 2", len(cfg.OpenCodeGoKey))
	}
	if cfg.OpenCodeGoKey[0].Prefix != "team" {
		t.Fatalf("prefix = %q, want team", cfg.OpenCodeGoKey[0].Prefix)
	}
	if cfg.OpenCodeGoKey[1].Headers["X-Trace"] != "on" {
		t.Fatalf("headers = %#v, want normalized header", cfg.OpenCodeGoKey[1].Headers)
	}
	if cfg.OpenCodeGoKey[1].VisionFallbackModel != "qwen3.6-plus" {
		t.Fatalf("vision fallback model = %q, want qwen3.6-plus", cfg.OpenCodeGoKey[1].VisionFallbackModel)
	}
	if len(cfg.OpenCodeGoKey[1].Models) != 1 || cfg.OpenCodeGoKey[1].Models[0].Name != "glm-5.2" {
		t.Fatalf("models = %#v, want normalized unique model", cfg.OpenCodeGoKey[1].Models)
	}
	if len(cfg.OpenCodeGoKey[1].ExcludedModels) != 1 || cfg.OpenCodeGoKey[1].ExcludedModels[0] != "*" {
		t.Fatalf("excluded models = %#v, want disable-all marker only", cfg.OpenCodeGoKey[1].ExcludedModels)
	}
	if cfg.OpenCodeGoKey[1].WorkspaceID != "wrk_123" || cfg.OpenCodeGoKey[1].AuthCookie != "auth-token" {
		t.Fatalf("usage fields not normalized: %+v", cfg.OpenCodeGoKey[1])
	}
}
