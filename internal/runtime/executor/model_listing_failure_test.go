package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// The Discover* listings back model registration, which must tell a live answer
// from a failure: a failure keeps the credential's last known list, a live answer
// replaces it. The Fetch* variants keep answering failures with the process-wide
// cache, as the panels expect.

func TestDiscoverAntigravityModelsReportsFailureRatherThanTheCache(t *testing.T) {
	resetAntigravityPrimaryModelsCacheForTest()
	t.Cleanup(resetAntigravityPrimaryModelsCacheForTest)
	if !storeAntigravityPrimaryModels([]*sdkmodelcatalog.ModelInfo{{ID: "cached-antigravity-model"}}) {
		t.Fatal("expected cache seed to store")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	auth := &cliproxyauth.Auth{
		ID:         "ag-listing-fails",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": srv.URL},
		Metadata: map[string]any{
			"access_token": "access-token",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	models, err := DiscoverAntigravityModels(context.Background(), auth, nil)
	if err == nil || len(models) != 0 {
		t.Fatalf("Discover = %d models, err %v; want the failure reported", len(models), err)
	}
	if got := FetchAntigravityModels(context.Background(), auth, nil); len(got) != 1 || got[0].ID != "cached-antigravity-model" {
		t.Fatalf("Fetch = %+v, want the cached list", got)
	}
}

func TestDiscoverXAIModelsReportsFailureRatherThanTheCache(t *testing.T) {
	resetXAIModelsCacheForTest()
	t.Cleanup(resetXAIModelsCacheForTest)
	if !storeXAIModels([]*sdkmodelcatalog.ModelInfo{{ID: "cached-xai-model", OwnedBy: "xai", Type: "xai"}}) {
		t.Fatal("expected cache seed to store")
	}
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"api_key": "xai-token", "base_url": "http://127.0.0.1:1"},
	}

	models, err := DiscoverXAIModels(context.Background(), auth, nil)
	if err == nil || len(models) != 0 {
		t.Fatalf("Discover = %d models, err %v; want the failure reported", len(models), err)
	}
	if _, errNoToken := DiscoverXAIModels(context.Background(), &cliproxyauth.Auth{Provider: "xai"}, nil); errNoToken == nil {
		t.Fatal("a credential without a token must report a failure")
	}
}

func TestDiscoverKimiModelsReportsFailureRatherThanTheCache(t *testing.T) {
	resetKimiModelsCacheForTest()
	t.Cleanup(resetKimiModelsCacheForTest)
	if !storeKimiModels([]*sdkmodelcatalog.ModelInfo{{ID: "kimi-cached", OwnedBy: "moonshot", Type: "kimi"}}) {
		t.Fatal("expected cache seed to store")
	}
	rt := &kimiModelsRoundTripper{status: http.StatusInternalServerError, body: `{"error":"boom"}`}
	ctx := context.WithValue(context.Background(), util.ContextKeyRoundTripper, rt)

	models, err := DiscoverKimiModels(ctx, kimiOAuthAuth(), nil)
	if err == nil || len(models) != 0 {
		t.Fatalf("Discover = %d models, err %v; want the failure reported", len(models), err)
	}
	if _, errNoToken := DiscoverKimiModels(ctx, &cliproxyauth.Auth{Provider: "kimi"}, nil); errNoToken == nil {
		t.Fatal("a credential without a token must report a failure")
	}
}
