package serviceapp

import (
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

type testModelRegistry struct {
	models       []*sdkmodelcatalog.ModelInfo
	unregistered bool
}

func (r *testModelRegistry) RegisterClient(_ string, _ string, models []*sdkmodelcatalog.ModelInfo) {
	r.models = models
	r.unregistered = false
}

func (r *testModelRegistry) UnregisterClient(_ string) {
	r.models = nil
	r.unregistered = true
}

func (r *testModelRegistry) SetModelQuotaExceeded(_, _ string)            {}
func (r *testModelRegistry) SuspendClientModel(_, _, _ string)            {}
func (r *testModelRegistry) ResumeClientModel(_, _ string)                {}
func (r *testModelRegistry) ClearModelQuotaExceeded(_, _ string)          {}
func (r *testModelRegistry) ClientSupportsModel(_, _ string) bool         { return false }
func (r *testModelRegistry) GetAvailableModels(_ string) []map[string]any { return nil }
func (r *testModelRegistry) GetAvailableModelsByProvider(_ string) []*sdkmodelcatalog.ModelInfo {
	return nil
}
func (r *testModelRegistry) GetModelProviders(_ string) []string { return nil }
func (r *testModelRegistry) GetFirstAvailableModel(_ string) (string, error) {
	return "", nil
}
func (r *testModelRegistry) GetModelsForClient(_ string) []*sdkmodelcatalog.ModelInfo {
	return r.models
}
func (r *testModelRegistry) SetHook(sdkmodelcatalog.RegistryHook) {}

func TestSyncDynamicConfigAuthModelsFiltersProviderDirtyModels(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		cfg        *config.Config
		attributes map[string]string
		want       string
		reject     string
	}{
		{
			name:     "opencode-go rejects cline pass rows",
			provider: "opencode-go",
			cfg: &config.Config{OpenCodeGoKey: []config.OpenCodeGoKey{{
				APIKey: "go-key",
				Models: []config.OpenCodeGoModel{
					{Name: "cline-pass/glm-5.2"},
					{Name: "qwen3.7-max"},
				},
			}}},
			attributes: map[string]string{"api_key": "go-key"},
			want:       "qwen3.7-max",
			reject:     "cline-pass/glm-5.2",
		},
		{
			name:     "cline keeps only cline pass rows",
			provider: "cline",
			cfg: &config.Config{ClineKey: []config.ClineKey{{
				APIKey:  "cline-key",
				BaseURL: config.DefaultClineBaseURL,
				Models: []config.ClineModel{
					{Name: "qwen3.7-max"},
					{Name: "cline-pass/mimo-v2.5-pro", Alias: "mimo-v2.5-pro"},
				},
			}}},
			attributes: map[string]string{"api_key": "cline-key", "base_url": config.DefaultClineBaseURL},
			want:       "mimo-v2.5-pro",
			reject:     "qwen3.7-max",
		},
		{
			name:     "ollama cloud rejects cline pass rows",
			provider: "ollama-cloud",
			cfg: &config.Config{OllamaCloudKey: []config.OllamaCloudKey{{
				APIKey:  "ollama-key",
				BaseURL: config.DefaultOllamaCloudBaseURL,
				Models: []config.OllamaCloudModel{
					{Name: "cline-pass/glm-5.2"},
					{Name: "gpt-oss:120b"},
				},
			}}},
			attributes: map[string]string{"api_key": "ollama-key", "base_url": config.DefaultOllamaCloudBaseURL},
			want:       "gpt-oss:120b",
			reject:     "cline-pass/glm-5.2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &testModelRegistry{}
			syncDynamicConfigAuthModels(reg, tt.cfg, &coreauth.Auth{
				ID:         tt.name,
				Provider:   tt.provider,
				Attributes: tt.attributes,
			})

			if !hasSDKModelID(reg.models, tt.want) {
				t.Fatalf("registered models = %+v, want %q", reg.models, tt.want)
			}
			if hasSDKModelID(reg.models, tt.reject) {
				t.Fatalf("registered dirty model %q in %+v", tt.reject, reg.models)
			}
		})
	}
}

func TestSyncDynamicConfigAuthModelsFallsBackWhenOnlyDirtyModelsRemain(t *testing.T) {
	reg := &testModelRegistry{}
	syncDynamicConfigAuthModels(reg, &config.Config{
		OpenCodeGoKey: []config.OpenCodeGoKey{{
			APIKey: "go-key",
			Models: []config.OpenCodeGoModel{{Name: "cline-pass/glm-5.2"}},
		}},
	}, &coreauth.Auth{
		ID:         "go-key",
		Provider:   "opencode-go",
		Attributes: map[string]string{"api_key": "go-key"},
	})

	if !hasSDKModelID(reg.models, "deepseek-v4-flash") {
		t.Fatalf("registered models = %+v, want OpenCode Go defaults", reg.models)
	}
	if hasSDKModelID(reg.models, "cline-pass/glm-5.2") {
		t.Fatalf("registered dirty fallback model in %+v", reg.models)
	}
}

func hasSDKModelID(models []*sdkmodelcatalog.ModelInfo, id string) bool {
	for _, model := range models {
		if model != nil && strings.EqualFold(strings.TrimSpace(model.ID), id) {
			return true
		}
	}
	return false
}
