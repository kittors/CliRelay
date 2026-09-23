package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// codexManifest builds a body in the shape of the ChatGPT Codex /models response
// (codex-rs protocol ModelsResponse): entries keyed by slug, reasoning presets as
// {effort, description} objects that include client modes such as "ultra".
func codexManifest(entries ...string) string {
	return `{"models":[` + strings.Join(entries, ",") + `]}`
}

func codexManifestServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func codexOAuthTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"base_url": baseURL + "/backend-api/codex"},
		Metadata:   map[string]any{"access_token": "oauth-token", "account_id": "acct-1"},
	}
}

func codexModelByID(models []*sdkmodelcatalog.ModelInfo, id string) *sdkmodelcatalog.ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

func TestDiscoverCodexModelsDescribesModelsTheCatalogLacks(t *testing.T) {
	resetCodexModelsCacheForTest()
	t.Cleanup(resetCodexModelsCacheForTest)

	var catalog *sdkmodelcatalog.ModelInfo
	for _, model := range sdkmodelcatalog.StaticModelDefinitionsByChannel("codex") {
		if model != nil && model.Thinking != nil && len(model.Thinking.Levels) > 1 {
			catalog = model
			break
		}
	}
	if catalog == nil {
		t.Skip("the codex catalog has no model with several reasoning levels")
	}

	var request *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(codexManifest(
			`{"slug":"gpt-manifest-only","display_name":"GPT Manifest Only","description":"Described by the manifest.",
			  "supported_reasoning_levels":[{"effort":"low","description":"a"},{"effort":"medium"},{"effort":"xhigh"},
			    {"effort":"ultra","description":"client mode"},{"effort":"persistent"}],
			  "context_window":1050000,"visibility":"list","supported_in_api":true,"priority":1}`,
			`{"slug":"gpt-manifest-plain","display_name":"Plain","supported_reasoning_levels":[{"effort":"ultra"}]}`,
			`{"slug":"`+catalog.ID+`","display_name":"Renamed upstream","supported_reasoning_levels":[{"effort":"low"}]}`,
		)))
	}))
	t.Cleanup(srv.Close)

	models, err := DiscoverCodexModels(context.Background(), codexOAuthTestAuth(srv.URL), nil)
	if err != nil {
		t.Fatalf("DiscoverCodexModels: %v", err)
	}
	if request == nil || request.URL.Path != "/backend-api/codex/models" || request.URL.Query().Get("client_version") == "" {
		t.Fatalf("did not ask the manifest: %+v", request)
	}
	if got := request.Header.Get("Chatgpt-Account-Id"); got != "acct-1" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}

	described := codexModelByID(models, "gpt-manifest-only")
	if described == nil {
		t.Fatalf("manifest-only model missing: %v", codexModelIDs(models))
	}
	// Client modes are not wire efforts and must not become selectable levels.
	if described.Thinking == nil || !slices.Equal(described.Thinking.Levels, []string{"low", "medium", "xhigh"}) {
		t.Fatalf("reasoning levels = %+v, want the manifest's wire efforts only", described.Thinking)
	}
	if described.ContextLength != 1050000 || described.Description != "Described by the manifest." || described.UserDefined {
		t.Fatalf("manifest-only model = %+v", described)
	}

	// With nothing recognisable advertised, the upstream judges reasoning settings.
	plain := codexModelByID(models, "gpt-manifest-plain")
	if plain == nil || plain.Thinking != nil || !plain.UserDefined {
		t.Fatalf("model without wire efforts = %+v, want user-defined passthrough", plain)
	}

	known := codexModelByID(models, catalog.ID)
	if known == nil || known.Thinking == nil || !slices.Equal(known.Thinking.Levels, catalog.Thinking.Levels) {
		t.Fatalf("catalog model %s = %+v, want the catalog's levels %v", catalog.ID, known, catalog.Thinking.Levels)
	}

	// Discovery reports what the manifest says; synthesized image models belong to
	// registration, not to this list.
	for _, model := range models {
		if registry.IsImageGenerationModel(model.ID) {
			t.Fatalf("discovery invented image model %s", model.ID)
		}
	}
}

func TestDiscoverCodexModelsReportsFailureInsteadOfTheCache(t *testing.T) {
	resetCodexModelsCacheForTest()
	t.Cleanup(resetCodexModelsCacheForTest)
	if !storeCodexModels([]*sdkmodelcatalog.ModelInfo{{ID: "cached-codex", Type: "codex"}}) {
		t.Fatal("cache seed failed")
	}
	srv, _ := codexManifestServer(t, http.StatusServiceUnavailable, `{"error":"down"}`)
	auth := codexOAuthTestAuth(srv.URL)

	models, err := DiscoverCodexModels(context.Background(), auth, nil)
	if err == nil || len(models) != 0 {
		t.Fatalf("DiscoverCodexModels = %v, %v; want the failure, not the cached list", codexModelIDs(models), err)
	}
	// The panel path keeps answering from the cache; that is the whole difference.
	if !codexModelSetContains(FetchCodexModels(context.Background(), auth, nil), "cached-codex") {
		t.Fatal("FetchCodexModels stopped falling back to the cache")
	}
}

func TestDiscoverCodexModelsRefusesAPIKeyCredentials(t *testing.T) {
	srv, hits := codexManifestServer(t, http.StatusOK, codexManifest(`{"slug":"relay-model"}`))

	_, err := DiscoverCodexModels(context.Background(), &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test", "base_url": srv.URL + "/backend-api/codex"},
	}, nil)

	if !errors.Is(err, ErrCodexManifestNeedsOAuth) || hits.Load() != 0 {
		t.Fatalf("err = %v, upstream hits = %d; want refusal without a request", err, hits.Load())
	}
}

func TestDiscoverCodexModelsRejectsANonManifestBase(t *testing.T) {
	srv, _ := codexManifestServer(t, http.StatusOK, `{"data":[{"id":"relay-model"}]}`)

	_, err := DiscoverCodexModels(context.Background(), &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"base_url": srv.URL},
		Metadata:   map[string]any{"access_token": "oauth-token"},
	}, nil)

	if !errors.Is(err, errCodexNotManifest) {
		t.Fatalf("err = %v, want errCodexNotManifest", err)
	}
}

func TestDiscoverCodexModelsRejectsAnUnrecognisedShape(t *testing.T) {
	resetCodexModelsCacheForTest()
	t.Cleanup(resetCodexModelsCacheForTest)
	srv, _ := codexManifestServer(t, http.StatusOK, `{"catalog":{"entries":[{"id":"gpt-shape-changed"}]}}`)
	auth := codexOAuthTestAuth(srv.URL)

	if _, err := DiscoverCodexModels(context.Background(), auth, nil); !errors.Is(err, errCodexManifestUnrecognised) {
		t.Fatalf("err = %v, want errCodexManifestUnrecognised", err)
	}
	// Panels still tolerate the heuristic walk.
	if !codexModelSetContains(FetchCodexModels(context.Background(), auth, nil), "gpt-shape-changed") {
		t.Fatal("FetchCodexModels stopped tolerating an unknown shape")
	}
}

func TestCodexWireReasoningLevelsAcceptsBareStrings(t *testing.T) {
	got := codexWireReasoningLevels([]byte(`["high","HIGH","ultra",{"effort":"max"},42]`))
	if !slices.Equal(got, []string{"high", "max"}) {
		t.Fatalf("levels = %v", got)
	}
}
