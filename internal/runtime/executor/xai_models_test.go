package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

func resetXAIModelsCacheForTest() {
	xaiModelsCache.mu.Lock()
	xaiModelsCache.models = nil
	xaiModelsCache.mu.Unlock()
}

func TestFetchXAIModelsParsesLiveCatalogAndAliases(t *testing.T) {
	resetXAIModelsCacheForTest()
	t.Cleanup(resetXAIModelsCacheForTest)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != xaiModelsPath {
			t.Fatalf("request path = %q, want %q", r.URL.Path, xaiModelsPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xai-token" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "grok-build-0.1",
					"aliases": ["grok-code-fast-1", "grok-code-fast"],
					"object": "model",
					"created": 1776297600,
					"owned_by": "xai",
					"context_length": 256000,
					"max_completion_tokens": 128000
				},
				{
					"id": "grok-4.5",
					"aliases": ["grok-build-latest"],
					"object": "model",
					"created": 1782691200,
					"owned_by": "xai",
					"context_length": 500000
				}
			]
		}`))
	}))
	defer srv.Close()

	models := FetchXAIModels(context.Background(), &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"api_key":  "xai-token",
			"base_url": srv.URL,
		},
	}, nil)

	byID := make(map[string]*sdkmodelcatalog.ModelInfo, len(models))
	for _, model := range models {
		if model != nil {
			byID[model.ID] = model
		}
	}
	if byID["grok-build-0.1"] == nil {
		t.Fatal("missing upstream grok-build-0.1 model")
	}
	if got := byID["grok-build-0.1"].ContextLength; got != 256000 {
		t.Fatalf("grok-build-0.1 context length = %d, want 256000", got)
	}
	if got := byID["grok-code-fast-1"].UpstreamModelID; got != "grok-build-0.1" {
		t.Fatalf("grok-code-fast-1 upstream = %q, want grok-build-0.1", got)
	}
	if got := byID["grok-build-latest"].UpstreamModelID; got != "grok-4.5" {
		t.Fatalf("grok-build-latest upstream = %q, want grok-4.5", got)
	}
	if got := byID["grok-build"].UpstreamModelID; got != "grok-build-0.1" {
		t.Fatalf("grok-build compatibility upstream = %q, want grok-build-0.1", got)
	}
}

func TestFetchXAIModelsFallsBackToCachedCatalog(t *testing.T) {
	resetXAIModelsCacheForTest()
	t.Cleanup(resetXAIModelsCacheForTest)

	if ok := storeXAIModels([]*sdkmodelcatalog.ModelInfo{{ID: "cached-xai-model", OwnedBy: "xai", Type: "xai"}}); !ok {
		t.Fatal("expected cache seed to store")
	}

	models := FetchXAIModels(context.Background(), &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"api_key":  "xai-token",
			"base_url": "http://127.0.0.1:1",
		},
	}, nil)
	if len(models) != 1 || models[0].ID != "cached-xai-model" {
		t.Fatalf("fallback models = %+v, want cached-xai-model", models)
	}
}
