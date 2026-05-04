package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func resetAntigravityPrimaryModelsCacheForTest() {
	antigravityPrimaryModelsCache.mu.Lock()
	antigravityPrimaryModelsCache.models = nil
	antigravityPrimaryModelsCache.mu.Unlock()
}

func TestStoreAntigravityPrimaryModels_EmptyDoesNotOverwrite(t *testing.T) {
	resetAntigravityPrimaryModelsCacheForTest()
	t.Cleanup(resetAntigravityPrimaryModelsCacheForTest)

	seed := []*registry.ModelInfo{
		{ID: "claude-sonnet-4-5"},
		{ID: "gemini-2.5-pro"},
	}
	if updated := storeAntigravityPrimaryModels(seed); !updated {
		t.Fatal("expected non-empty model list to update primary cache")
	}

	if updated := storeAntigravityPrimaryModels(nil); updated {
		t.Fatal("expected nil model list not to overwrite primary cache")
	}
	if updated := storeAntigravityPrimaryModels([]*registry.ModelInfo{}); updated {
		t.Fatal("expected empty model list not to overwrite primary cache")
	}

	got := loadAntigravityPrimaryModels()
	if len(got) != 2 {
		t.Fatalf("expected cached model count 2, got %d", len(got))
	}
	if got[0].ID != "claude-sonnet-4-5" || got[1].ID != "gemini-2.5-pro" {
		t.Fatalf("unexpected cached model ids: %q, %q", got[0].ID, got[1].ID)
	}
}

func TestLoadAntigravityPrimaryModels_ReturnsClone(t *testing.T) {
	resetAntigravityPrimaryModelsCacheForTest()
	t.Cleanup(resetAntigravityPrimaryModelsCacheForTest)

	if updated := storeAntigravityPrimaryModels([]*registry.ModelInfo{{
		ID:                         "gpt-5",
		DisplayName:                "GPT-5",
		SupportedGenerationMethods: []string{"generateContent"},
		SupportedParameters:        []string{"temperature"},
		Thinking: &registry.ThinkingSupport{
			Levels: []string{"high"},
		},
	}}); !updated {
		t.Fatal("expected model cache update")
	}

	got := loadAntigravityPrimaryModels()
	if len(got) != 1 {
		t.Fatalf("expected one cached model, got %d", len(got))
	}
	got[0].ID = "mutated-id"
	if len(got[0].SupportedGenerationMethods) > 0 {
		got[0].SupportedGenerationMethods[0] = "mutated-method"
	}
	if len(got[0].SupportedParameters) > 0 {
		got[0].SupportedParameters[0] = "mutated-parameter"
	}
	if got[0].Thinking != nil && len(got[0].Thinking.Levels) > 0 {
		got[0].Thinking.Levels[0] = "mutated-level"
	}

	again := loadAntigravityPrimaryModels()
	if len(again) != 1 {
		t.Fatalf("expected one cached model after mutation, got %d", len(again))
	}
	if again[0].ID != "gpt-5" {
		t.Fatalf("expected cached model id to remain %q, got %q", "gpt-5", again[0].ID)
	}
	if len(again[0].SupportedGenerationMethods) == 0 || again[0].SupportedGenerationMethods[0] != "generateContent" {
		t.Fatalf("expected cached generation methods to be unmutated, got %v", again[0].SupportedGenerationMethods)
	}
	if len(again[0].SupportedParameters) == 0 || again[0].SupportedParameters[0] != "temperature" {
		t.Fatalf("expected cached supported parameters to be unmutated, got %v", again[0].SupportedParameters)
	}
	if again[0].Thinking == nil || len(again[0].Thinking.Levels) == 0 || again[0].Thinking.Levels[0] != "high" {
		t.Fatalf("expected cached model thinking levels to be unmutated, got %v", again[0].Thinking)
	}
}

func TestFetchAntigravityModels_UsesProjectAndParsesCurrentCatalog(t *testing.T) {
	resetAntigravityPrimaryModelsCacheForTest()
	t.Cleanup(resetAntigravityPrimaryModelsCacheForTest)

	var gotPayload struct {
		Project string `json:"project"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != antigravityModelsPath {
			t.Fatalf("request path = %q, want %q", r.URL.Path, antigravityModelsPath)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode request payload: %v", err)
		}
		if gotPayload.Project != "bamboo-precept-lgxtn" {
			t.Fatalf("project payload = %q, want bamboo-precept-lgxtn", gotPayload.Project)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": {
				"gemini-3.1-pro-high": {
					"displayName": "Gemini 3.1 Pro (High)",
					"supportsThinking": true,
					"maxTokens": 1048576,
					"maxOutputTokens": 65535,
					"quotaInfo": {"remainingFraction": 1, "resetTime": "2026-05-09T15:50:29Z"}
				},
				"gemini-3-flash-agent": {
					"displayName": "Gemini 3 Flash",
					"maxTokens": 1048576,
					"maxOutputTokens": 65536
				},
				"tab_jump_flash_lite_preview": {
					"maxTokens": 16384,
					"quotaInfo": {"remainingFraction": 1},
					"isInternal": true
				},
				"chat_23310": {
					"maxTokens": 32768,
					"isInternal": true
				}
			},
			"agentModelSorts": [
				{"displayName": "Recommended", "groups": [{"modelIds": ["gemini-3.1-pro-high", "gemini-3-flash-agent"]}]}
			],
			"tabModelIds": ["chat_23310"]
		}`))
	}))
	defer srv.Close()

	auth := &cliproxyauth.Auth{
		ID:       "ag-current-catalog",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": srv.URL,
		},
		Metadata: map[string]any{
			"access_token": "access-token",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
			"project_id":   "bamboo-precept-lgxtn",
		},
	}

	models := FetchAntigravityModels(context.Background(), auth, nil)
	ids := make(map[string]*registry.ModelInfo, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = model
		}
	}

	if _, ok := ids["gemini-3.1-pro-high"]; !ok {
		t.Fatalf("expected gemini-3.1-pro-high in fetched models, got %#v", ids)
	}
	if ids["gemini-3.1-pro-high"].ContextLength != 1048576 {
		t.Fatalf("context length = %d, want 1048576", ids["gemini-3.1-pro-high"].ContextLength)
	}
	if ids["gemini-3.1-pro-high"].MaxCompletionTokens != 65535 {
		t.Fatalf("max completion tokens = %d, want 65535", ids["gemini-3.1-pro-high"].MaxCompletionTokens)
	}
	if _, ok := ids["gemini-3-flash-agent"]; !ok {
		t.Fatalf("expected gemini-3-flash-agent in fetched models, got %#v", ids)
	}
	if _, ok := ids["tab_jump_flash_lite_preview"]; ok {
		t.Fatalf("internal tab jump model should not be registered")
	}
	if _, ok := ids["chat_23310"]; ok {
		t.Fatalf("internal chat model should not be registered")
	}
}
