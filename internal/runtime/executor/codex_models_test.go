package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

func resetCodexModelsCacheForTest() {
	codexModelsCache.mu.Lock()
	codexModelsCache.models = nil
	codexModelsCache.mu.Unlock()
}

func TestParseCodexModelsFromDataAndSlug(t *testing.T) {
	body := []byte(`{
		"data": [
			{"id": "gpt-5.1-codex", "display_name": "GPT-5.1 Codex"},
			{"slug": "o3-pro", "title": "o3 Pro"},
			{"id": "gpt-5.1-codex"}
		]
	}`)
	models, ok := parseCodexModels(body, 1700000000)
	if !ok {
		t.Fatal("expected parse success")
	}
	if len(models) != 2 {
		t.Fatalf("len(models)=%d, want 2", len(models))
	}
	if models[0].ID != "gpt-5.1-codex" || models[0].Type != "codex" {
		t.Fatalf("models[0]=%+v", models[0])
	}
	if models[1].ID != "o3-pro" {
		t.Fatalf("models[1].ID=%q", models[1].ID)
	}
}

func TestParseCodexModelsLooseNested(t *testing.T) {
	body := []byte(`{
		"payload": {
			"models": [
				{"slug": "gpt-nested-1", "display_name": "Nested"},
				{"id": "not-a-model", "models": []}
			]
		}
	}`)
	models, ok := parseCodexModels(body, 1700000000)
	if !ok {
		t.Fatal("expected loose parse success")
	}
	found := false
	for _, m := range models {
		if m != nil && m.ID == "gpt-nested-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing nested model, got %v", models)
	}
}

func TestBuildCodexModelsURL(t *testing.T) {
	url, isManifest := buildCodexModelsURL("", false, "")
	if !isManifest || !strings.Contains(url, "chatgpt.com/backend-api/codex/models") {
		t.Fatalf("default oauth url=%q isManifest=%v", url, isManifest)
	}
	if !strings.Contains(url, "client_version=") {
		t.Fatalf("missing client_version in %q", url)
	}

	url, isManifest = buildCodexModelsURL("https://api.openai.com", true, "")
	if isManifest || url != "https://api.openai.com/v1/models" {
		t.Fatalf("api key openai url=%q isManifest=%v", url, isManifest)
	}

	url, isManifest = buildCodexModelsURL("https://gateway.example/v1", true, "")
	if isManifest || url != "https://gateway.example/v1/models" {
		t.Fatalf("compat url=%q isManifest=%v", url, isManifest)
	}

	url, isManifest = buildCodexModelsURL("https://chatgpt.com/backend-api", false, "0.180.0")
	if !isManifest || !strings.Contains(url, "/codex/models") {
		t.Fatalf("chatgpt backend url=%q isManifest=%v", url, isManifest)
	}
}

func TestFetchCodexModelsAPIKeyCatalog(t *testing.T) {
	resetCodexModelsCacheForTest()
	t.Cleanup(resetCodexModelsCacheForTest)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("Authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"codex-live-1","owned_by":"openai"}]}`))
	}))
	defer srv.Close()

	models := FetchCodexModels(context.Background(), &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": srv.URL,
		},
	}, nil)
	// Image models are merged into every Codex credential's set (see
	// withCodexImageModels), so assert on membership rather than length.
	if !codexModelSetContains(models, "codex-live-1") {
		t.Fatalf("live model missing: %v", codexModelIDs(models))
	}
	if !codexModelSetContains(models, "gpt-image-2") {
		t.Fatalf("image models must be routable for this credential: %v", codexModelIDs(models))
	}
}

func TestFetchCodexModelsFallsBackToCache(t *testing.T) {
	resetCodexModelsCacheForTest()
	t.Cleanup(resetCodexModelsCacheForTest)

	if ok := storeCodexModels([]*sdkmodelcatalog.ModelInfo{{ID: "cached-codex", OwnedBy: "openai", Type: "codex"}}); !ok {
		t.Fatal("cache seed failed")
	}
	// This auth carries no token, so nothing here is routable — the cache is
	// returned unmerged. Merging image models onto a credential-less lookup would
	// manufacture a servable set out of nothing.
	models := FetchCodexModels(context.Background(), &cliproxyauth.Auth{Provider: "codex"}, nil)
	if len(models) != 1 || models[0].ID != "cached-codex" {
		t.Fatalf("fallback models=%v", codexModelIDs(models))
	}
}

// TestFetchCodexModelsMergesImageModelsForCredentialedFallback covers the other
// fallback shape: a real credential whose manifest fetch failed still has to be
// able to serve an image request, or the router reports auth_not_found.
func TestFetchCodexModelsMergesImageModelsForCredentialedFallback(t *testing.T) {
	resetCodexModelsCacheForTest()
	t.Cleanup(resetCodexModelsCacheForTest)

	if ok := storeCodexModels([]*sdkmodelcatalog.ModelInfo{{ID: "cached-codex", OwnedBy: "openai", Type: "codex"}}); !ok {
		t.Fatal("cache seed failed")
	}
	models := FetchCodexModels(context.Background(), &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test", "base_url": "http://127.0.0.1:1"},
	}, nil)
	for _, modelID := range []string{"cached-codex", "gpt-image-2", "gpt-image-2.5-flare"} {
		if !codexModelSetContains(models, modelID) {
			t.Fatalf("%s missing from a credentialed fallback set: %v", modelID, codexModelIDs(models))
		}
	}
}

func codexModelIDs(models []*sdkmodelcatalog.ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model != nil {
			ids = append(ids, model.ID)
		}
	}
	return ids
}

func codexModelSetContains(models []*sdkmodelcatalog.ModelInfo, modelID string) bool {
	for _, model := range models {
		if model != nil && model.ID == modelID {
			return true
		}
	}
	return false
}

func TestResolveCodexModelsClientVersionFloorsStaleDefault(t *testing.T) {
	got := resolveCodexModelsClientVersion(nil, nil)
	if got != defaultCodexModelsClientVer {
		t.Fatalf("nil cfg version=%q want %q", got, defaultCodexModelsClientVer)
	}
	got = resolveCodexModelsClientVersion(&config.Config{}, nil)
	if got != defaultCodexModelsClientVer {
		t.Fatalf("empty fp version=%q want %q", got, defaultCodexModelsClientVer)
	}
}

func TestResolveCodexModelsClientVersionUsesConfiguredWhenNewer(t *testing.T) {
	cfg := &config.Config{}
	cfg.IdentityFingerprint.Codex.Version = "0.201.0"
	got := resolveCodexModelsClientVersion(cfg, nil)
	if got != "0.201.0" {
		t.Fatalf("got %q", got)
	}
	cfg.IdentityFingerprint.Codex.Version = "0.118.0"
	got = resolveCodexModelsClientVersion(cfg, nil)
	if got != defaultCodexModelsClientVer {
		t.Fatalf("stale configured version should floor, got %q", got)
	}
}

func TestBuildCodexModelsURLEmbedsClientVersion(t *testing.T) {
	url, isManifest := buildCodexModelsURL("", false, "0.180.0")
	if !isManifest || !strings.Contains(url, "client_version=0.180.0") {
		t.Fatalf("url=%q", url)
	}
}

func TestParseCodexModelsFromSlugManifest(t *testing.T) {
	body := []byte(`{"models":[
		{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol"},
		{"slug":"gpt-5.5","display_name":"GPT-5.5"},
		{"slug":"gpt-5.4","display_name":"GPT-5.4"}
	]}`)
	models, ok := parseCodexModels(body, 1700000000)
	if !ok || len(models) != 3 {
		t.Fatalf("ok=%v len=%d", ok, len(models))
	}
	if models[0].ID != "gpt-5.6-sol" {
		t.Fatalf("models[0]=%+v", models[0])
	}
}
