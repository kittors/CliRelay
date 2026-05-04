package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func initManagementModelsTestDB(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func performModelsRequest(method string, path string, body []byte, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if body == nil {
		c.Request = httptest.NewRequest(method, path, nil)
	} else {
		c.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
	}
	handler(c)
	return rec
}

func TestModelConfigHandlersCreateListAndDelete(t *testing.T) {
	initManagementModelsTestDB(t)
	h := NewHandler(&config.Config{}, "", nil)

	createBody := []byte(`{
		"id": "custom-image",
		"owned_by": "acme-ai",
		"description": "Custom image model",
		"enabled": true,
		"pricing": {
			"mode": "call",
			"price_per_call": 0.12
		}
	}`)
	createRec := performModelsRequest(http.MethodPost, "/model-configs", createBody, h.PostModelConfig)
	if createRec.Code != http.StatusOK {
		t.Fatalf("PostModelConfig status = %d body = %s", createRec.Code, createRec.Body.String())
	}

	listRec := performModelsRequest(http.MethodGet, "/model-configs", nil, h.GetModelConfigs)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GetModelConfigs status = %d body = %s", listRec.Code, listRec.Body.String())
	}
	var listPayload struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
			Pricing struct {
				Mode         string  `json:"mode"`
				PricePerCall float64 `json:"price_per_call"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	found := false
	for _, item := range listPayload.Data {
		if item.ID == "custom-image" {
			found = true
			if item.OwnedBy != "acme-ai" || item.Pricing.Mode != "call" || item.Pricing.PricePerCall != 0.12 {
				t.Fatalf("unexpected custom-image payload: %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("expected custom-image in list response")
	}

	deleteRec := performModelsRequest(http.MethodDelete, "/model-configs/custom-image", nil, func(c *gin.Context) {
		c.Params = gin.Params{{Key: "id", Value: "custom-image"}}
		h.DeleteModelConfig(c)
	})
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("DeleteModelConfig status = %d body = %s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, ok := usage.GetModelConfig("custom-image"); ok {
		t.Fatal("expected custom-image to be deleted")
	}
}

func TestModelConfigHandlersScopeFiltering(t *testing.T) {
	initManagementModelsTestDB(t)
	h := NewHandler(&config.Config{}, "", nil)

	createBody := []byte(`{
		"id": "custom-active",
		"owned_by": "acme-ai",
		"description": "Custom active model",
		"enabled": true,
		"pricing": {
			"mode": "token",
			"input_price_per_million": 1,
			"output_price_per_million": 2
		}
	}`)
	createRec := performModelsRequest(http.MethodPost, "/model-configs", createBody, h.PostModelConfig)
	if createRec.Code != http.StatusOK {
		t.Fatalf("PostModelConfig status = %d body = %s", createRec.Code, createRec.Body.String())
	}

	createLibraryBody := []byte(`{
		"id": "custom-library",
		"owned_by": "acme-ai",
		"description": "Custom library model",
		"enabled": true,
		"pricing": {
			"mode": "token",
			"input_price_per_million": 3,
			"output_price_per_million": 4
		}
	}`)
	createLibraryRec := performModelsRequest(http.MethodPost, "/model-configs?scope=library", createLibraryBody, h.PostModelConfig)
	if createLibraryRec.Code != http.StatusOK {
		t.Fatalf("PostModelConfig library status = %d body = %s", createLibraryRec.Code, createLibraryRec.Body.String())
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
		t.Fatalf("UpsertModelConfig openrouter model: %v", err)
	}

	decodeSources := func(rec *httptest.ResponseRecorder) map[string]string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("list status = %d body = %s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Data []struct {
				ID     string `json:"id"`
				Source string `json:"source"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal list response: %v", err)
		}
		sources := make(map[string]string)
		for _, item := range payload.Data {
			sources[item.ID] = item.Source
		}
		return sources
	}

	activeSources := decodeSources(performModelsRequest(http.MethodGet, "/model-configs", nil, h.GetModelConfigs))
	if activeSources["custom-active"] != "user" {
		t.Fatal("expected custom-active in default active scope")
	}
	if _, ok := activeSources["gpt-image-2"]; ok {
		t.Fatal("did not expect seed-only gpt-image-2 in default active scope")
	}
	if _, ok := activeSources["custom-library"]; ok {
		t.Fatal("did not expect custom-library in default active scope")
	}
	if _, ok := activeSources["openai/gpt-5.3-codex"]; ok {
		t.Fatal("did not expect openrouter-synced model in default active scope")
	}

	librarySources := decodeSources(performModelsRequest(http.MethodGet, "/model-configs?scope=library", nil, h.GetModelConfigs))
	if _, ok := librarySources["gpt-image-2"]; !ok {
		t.Fatal("expected gpt-image-2 in library scope")
	}
	if _, ok := librarySources["custom-active"]; ok {
		t.Fatal("did not expect user custom-active in library scope")
	}
	if librarySources["custom-library"] != "seed" {
		t.Fatalf("custom-library source = %q, want seed", librarySources["custom-library"])
	}
	if librarySources["openai/gpt-5.3-codex"] != "openrouter" {
		t.Fatalf("openrouter model source = %q, want openrouter", librarySources["openai/gpt-5.3-codex"])
	}

	allSources := decodeSources(performModelsRequest(http.MethodGet, "/model-configs?scope=all", nil, h.GetModelConfigs))
	if _, ok := allSources["gpt-image-2"]; !ok {
		t.Fatal("expected all scope to include gpt-image-2")
	}
	if allSources["custom-active"] != "user" || allSources["custom-library"] != "seed" {
		t.Fatalf("expected all scope to include user and seed models, got custom-active=%q custom-library=%q", allSources["custom-active"], allSources["custom-library"])
	}
	if allSources["openai/gpt-5.3-codex"] != "openrouter" {
		t.Fatalf("expected all scope to include openrouter model, got %q", allSources["openai/gpt-5.3-codex"])
	}
}

func TestModelOwnerPresetHandlersReplacePresets(t *testing.T) {
	initManagementModelsTestDB(t)
	h := NewHandler(&config.Config{}, "", nil)

	body := []byte(`{
		"items": [
			{"value": "openai", "label": "OpenAI", "description": "OpenAI models", "enabled": true},
			{"value": "acme-ai", "label": "Acme AI", "description": "Internal models", "enabled": true}
		]
	}`)
	putRec := performModelsRequest(http.MethodPut, "/model-owner-presets", body, h.PutModelOwnerPresets)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PutModelOwnerPresets status = %d body = %s", putRec.Code, putRec.Body.String())
	}

	getRec := performModelsRequest(http.MethodGet, "/model-owner-presets", nil, h.GetModelOwnerPresets)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GetModelOwnerPresets status = %d body = %s", getRec.Code, getRec.Body.String())
	}
	if _, ok := usage.GetModelOwnerPreset("acme-ai"); !ok {
		t.Fatal("expected acme-ai owner preset")
	}
}

func TestOpenRouterModelSyncHandlersConfigureAndRun(t *testing.T) {
	initManagementModelsTestDB(t)
	h := NewHandler(&config.Config{}, "", nil)
	restoreFetcher := usage.SetOpenRouterModelFetcherForTest(func(_ context.Context) ([]usage.OpenRouterRemoteModel, error) {
		return []usage.OpenRouterRemoteModel{
			{
				ID:          "openai/gpt-openrouter-handler-test",
				Name:        "OpenAI: GPT OpenRouter Handler Test",
				Description: "Agentic coding model",
				Pricing: usage.OpenRouterRemotePricing{
					Prompt:         "0.00000175",
					Completion:     "0.000014",
					InputCacheRead: "0.000000175",
				},
			},
		}, nil
	})
	defer restoreFetcher()

	putBody := []byte(`{"enabled": true, "interval_minutes": 120}`)
	putRec := performModelsRequest(http.MethodPut, "/model-openrouter-sync", putBody, h.PutOpenRouterModelSync)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PutOpenRouterModelSync status = %d body = %s", putRec.Code, putRec.Body.String())
	}
	var putPayload struct {
		Enabled         bool `json:"enabled"`
		IntervalMinutes int  `json:"interval_minutes"`
	}
	if err := json.Unmarshal(putRec.Body.Bytes(), &putPayload); err != nil {
		t.Fatalf("unmarshal put response: %v", err)
	}
	if !putPayload.Enabled || putPayload.IntervalMinutes != 120 {
		t.Fatalf("unexpected sync settings response: %+v", putPayload)
	}

	runRec := performModelsRequest(http.MethodPost, "/model-openrouter-sync/run", nil, h.PostOpenRouterModelSyncRun)
	if runRec.Code != http.StatusOK {
		t.Fatalf("PostOpenRouterModelSyncRun status = %d body = %s", runRec.Code, runRec.Body.String())
	}
	var runPayload struct {
		Result struct {
			Seen    int `json:"seen"`
			Added   int `json:"added"`
			Skipped int `json:"skipped"`
		} `json:"result"`
		State struct {
			LastAdded   int    `json:"last_added"`
			LastSkipped int    `json:"last_skipped"`
			LastError   string `json:"last_error"`
		} `json:"state"`
	}
	if err := json.Unmarshal(runRec.Body.Bytes(), &runPayload); err != nil {
		t.Fatalf("unmarshal run response: %v", err)
	}
	if runPayload.Result.Seen != 1 || runPayload.Result.Added != 1 || runPayload.Result.Skipped != 0 {
		t.Fatalf("unexpected sync run result: %+v", runPayload.Result)
	}
	if runPayload.State.LastAdded != 1 || runPayload.State.LastSkipped != 0 || runPayload.State.LastError != "" {
		t.Fatalf("unexpected sync run state: %+v", runPayload.State)
	}
	if _, ok := usage.GetModelConfig("gpt-openrouter-handler-test"); !ok {
		t.Fatal("expected gpt-openrouter-handler-test to be imported")
	}
	if _, ok := usage.GetModelConfig("openai/gpt-openrouter-handler-test"); ok {
		t.Fatal("did not expect OpenRouter provider prefix to be stored in model id")
	}

	getRec := performModelsRequest(http.MethodGet, "/model-openrouter-sync", nil, h.GetOpenRouterModelSync)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GetOpenRouterModelSync status = %d body = %s", getRec.Code, getRec.Body.String())
	}
}
