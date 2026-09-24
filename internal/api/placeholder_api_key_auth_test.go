package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type clientRouteProbe struct {
	name  string
	build func(key string) *http.Request
}

// One probe per client route family and credential carrier that shares the
// access manager, so the example keys are refused wherever a key is accepted.
var placeholderKeyProbes = []clientRouteProbe{
	{"v1 bearer", func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		return req
	}},
	{"v1 x-api-key", func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("X-Api-Key", key)
		return req
	}},
	{"v1beta x-goog-api-key", func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
		req.Header.Set("X-Goog-Api-Key", key)
		return req
	}},
	{"v1beta query key", func(key string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/v1beta/models?key="+key, nil)
	}},
	{"codex direct bearer", func(key string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/backend-api/codex/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		return req
	}},
}

func assertClientRoutesRejectKey(t *testing.T, server *Server, key string) {
	t.Helper()
	for _, probe := range placeholderKeyProbes {
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, probe.build(key))
		if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "Invalid API key") {
			t.Fatalf("%s with %s: status = %d body=%s, want 401 Invalid API key", probe.name, key, rr.Code, rr.Body.String())
		}
	}
}

func assertClientRouteAcceptsKey(t *testing.T, server *Server, key string) {
	t.Helper()
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, placeholderKeyProbes[0].build(key))
	if rr.Code != http.StatusOK {
		t.Fatalf("real key %s: status = %d body=%s, want 200", key, rr.Code, rr.Body.String())
	}
}

func TestClientRoutesRejectPlaceholderAPIKeysFromConfig(t *testing.T) {
	server := newTestServerWithConfig(t, func(cfg *proxyconfig.Config) {
		cfg.SDKConfig.APIKeys = []string{"your-api-key-1", "your-api-key-2", "your-api-key-3", "test-key"}
	})

	for _, key := range []string{"your-api-key-1", "your-api-key-2", "your-api-key-3"} {
		assertClientRoutesRejectKey(t, server, key)
	}
	assertClientRouteAcceptsKey(t, server, "test-key")
}

// Deployments that already imported the example keys hold them as ordinary
// database rows; upgrading must cut those rows off as well.
func TestClientRoutesRejectPlaceholderAPIKeysStoredInDatabase(t *testing.T) {
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), proxyconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("usage.InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)
	for _, row := range []usage.APIKeyRow{
		{Key: "your-api-key-1", Name: "api-key-1"},
		{Key: "your-api-key-2", Name: "api-key-2"},
		{Key: "sk-stored-real", Name: "real"},
	} {
		if err := usage.UpsertAPIKey(row); err != nil {
			t.Fatalf("UpsertAPIKey(%s): %v", row.Name, err)
		}
	}
	server := newTestServerWithConfig(t, func(cfg *proxyconfig.Config) {
		cfg.SDKConfig.APIKeys = nil
	})

	for _, key := range []string{"your-api-key-1", "your-api-key-2"} {
		assertClientRoutesRejectKey(t, server, key)
	}
	assertClientRouteAcceptsKey(t, server, "sk-stored-real")
}
