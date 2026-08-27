package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	kimiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

type kimiModelsRoundTripper struct {
	req    *http.Request
	body   string
	status int
}

func (k *kimiModelsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	k.req = req
	status := k.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(k.body)),
		Request:    req,
	}, nil
}

func resetKimiModelsCacheForTest() {
	kimiModelsCache.mu.Lock()
	kimiModelsCache.models = nil
	kimiModelsCache.mu.Unlock()
}

func kimiOAuthAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "kimi",
		Metadata: map[string]any{
			"access_token": "kimi-token",
			"device_id":    "device-abc",
		},
	}
}

func fetchKimiModelsWithBody(t *testing.T, body string) ([]*sdkmodelcatalog.ModelInfo, *kimiModelsRoundTripper) {
	t.Helper()
	rt := &kimiModelsRoundTripper{body: body}
	ctx := context.WithValue(context.Background(), util.ContextKeyRoundTripper, rt)
	return FetchKimiModels(ctx, kimiOAuthAuth(), nil), rt
}

func kimiModelsByID(models []*sdkmodelcatalog.ModelInfo) map[string]*sdkmodelcatalog.ModelInfo {
	byID := make(map[string]*sdkmodelcatalog.ModelInfo, len(models))
	for _, model := range models {
		if model != nil {
			byID[model.ID] = model
		}
	}
	return byID
}

func TestFetchKimiModelsNamespacesUpstreamIDs(t *testing.T) {
	resetKimiModelsCacheForTest()
	t.Cleanup(resetKimiModelsCacheForTest)

	models, rt := fetchKimiModelsWithBody(t, `{
		"object": "list",
		"data": [
			{"id": "k2.5", "object": "model", "created": 1769472000, "owned_by": "moonshot"},
			{"id": "kimi-k2", "object": "model", "owned_by": "moonshot"}
		]
	}`)

	if rt.req == nil {
		t.Fatal("expected models request to be issued")
	}
	wantURL := strings.TrimRight(kimiauth.KimiAPIBaseURL, "/") + kimiModelsPath
	if got := rt.req.URL.String(); got != wantURL {
		t.Fatalf("models URL = %q, want %q", got, wantURL)
	}
	if got := rt.req.Header.Get("Authorization"); got != "Bearer kimi-token" {
		t.Fatalf("Authorization = %q, want Bearer kimi-token", got)
	}
	// The gateway rejects requests that do not look like kimi-cli, so discovery
	// must carry the same identity headers as chat traffic.
	if got := rt.req.Header.Get("X-Msh-Platform"); got != "kimi_cli" {
		t.Fatalf("X-Msh-Platform = %q, want kimi_cli", got)
	}
	if got := rt.req.Header.Get("X-Msh-Device-Id"); got != "device-abc" {
		t.Fatalf("X-Msh-Device-Id = %q, want the account device id", got)
	}

	byID := kimiModelsByID(models)
	bare := byID["kimi-k2.5"]
	if bare == nil {
		t.Fatalf("models = %+v, want bare id namespaced to kimi-k2.5", models)
	}
	if bare.UpstreamModelID != "k2.5" {
		t.Fatalf("kimi-k2.5 upstream = %q, want k2.5", bare.UpstreamModelID)
	}
	if bare.Type != "kimi" {
		t.Fatalf("kimi-k2.5 type = %q, want kimi", bare.Type)
	}
	prefixed := byID["kimi-k2"]
	if prefixed == nil {
		t.Fatalf("models = %+v, want already-prefixed id kept as kimi-k2", models)
	}
	if prefixed.UpstreamModelID != "" {
		t.Fatalf("kimi-k2 upstream = %q, want empty when the id needs no rewrite", prefixed.UpstreamModelID)
	}
}

func TestFetchKimiModelsEnrichesKnownModelsFromStaticCatalog(t *testing.T) {
	resetKimiModelsCacheForTest()
	t.Cleanup(resetKimiModelsCacheForTest)

	models, _ := fetchKimiModelsWithBody(t, `{"data":[{"id":"k2.6","object":"model"}]}`)

	byID := kimiModelsByID(models)
	model := byID["kimi-k2.6"]
	if model == nil {
		t.Fatalf("models = %+v, want kimi-k2.6", models)
	}
	// The listing carries ids only; reasoning support has to come from the catalog
	// or ApplyThinking silently drops the client's reasoning_effort.
	if model.Thinking == nil {
		t.Fatal("kimi-k2.6 thinking support = nil, want the catalog definition")
	}
	if model.DisplayName != "Kimi K2.6" {
		t.Fatalf("kimi-k2.6 display name = %q, want Kimi K2.6", model.DisplayName)
	}
	if model.ContextLength != 131072 {
		t.Fatalf("kimi-k2.6 context length = %d, want the catalog value", model.ContextLength)
	}
	if model.OwnedBy != "moonshot" {
		t.Fatalf("kimi-k2.6 owned_by = %q, want moonshot", model.OwnedBy)
	}
}

func TestFetchKimiModelsKeepsUnknownModelWithoutGuessedCapabilities(t *testing.T) {
	resetKimiModelsCacheForTest()
	t.Cleanup(resetKimiModelsCacheForTest)

	models, _ := fetchKimiModelsWithBody(t, `{"data":[{"id":"k9-preview","object":"model"}]}`)

	byID := kimiModelsByID(models)
	model := byID["kimi-k9-preview"]
	if model == nil {
		t.Fatalf("models = %+v, want an id the catalog has never seen to still be routable", models)
	}
	if model.Thinking != nil {
		t.Fatalf("kimi-k9-preview thinking = %+v, want nil rather than a guessed budget", model.Thinking)
	}
	if model.DisplayName != "Kimi K9 Preview" {
		t.Fatalf("kimi-k9-preview display name = %q, want a readable fallback", model.DisplayName)
	}
}

func TestFetchKimiModelsFallsBackToCachedCatalog(t *testing.T) {
	resetKimiModelsCacheForTest()
	t.Cleanup(resetKimiModelsCacheForTest)

	if ok := storeKimiModels([]*sdkmodelcatalog.ModelInfo{{ID: "kimi-cached", OwnedBy: "moonshot", Type: "kimi"}}); !ok {
		t.Fatal("expected cache seed to store")
	}

	rt := &kimiModelsRoundTripper{status: http.StatusInternalServerError, body: `{"error":"boom"}`}
	ctx := context.WithValue(context.Background(), util.ContextKeyRoundTripper, rt)
	models := FetchKimiModels(ctx, kimiOAuthAuth(), nil)

	if len(models) != 1 || models[0].ID != "kimi-cached" {
		t.Fatalf("fallback models = %+v, want the cached list retained", models)
	}
}

func TestFetchKimiModelsWithoutCredentialsDoesNotCallUpstream(t *testing.T) {
	resetKimiModelsCacheForTest()
	t.Cleanup(resetKimiModelsCacheForTest)

	rt := &kimiModelsRoundTripper{body: `{"data":[{"id":"k2.5"}]}`}
	ctx := context.WithValue(context.Background(), util.ContextKeyRoundTripper, rt)
	models := FetchKimiModels(ctx, &cliproxyauth.Auth{Provider: "kimi"}, nil)

	if rt.req != nil {
		t.Fatal("expected no upstream request without a token")
	}
	if len(models) != 0 {
		t.Fatalf("models = %+v, want empty so callers apply their own floor", models)
	}
}

func TestParseKimiModelsRejectsEmptyAndInvalidPayloads(t *testing.T) {
	if _, ok := parseKimiModels([]byte(`not json`), 1); ok {
		t.Fatal("invalid JSON reported as a usable model list")
	}
	if _, ok := parseKimiModels([]byte(`{"data":[]}`), 1); ok {
		t.Fatal("empty listing reported as a usable model list")
	}
	if _, ok := parseKimiModels([]byte(`{"data":[{"id":"   "}]}`), 1); ok {
		t.Fatal("blank id reported as a usable model list")
	}
}
