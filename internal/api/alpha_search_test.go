package api

import (
	"context"
	"io"
	"path/filepath"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAlphaSearchRouteRequiresAuthentication(t *testing.T) {
	s := newRouteTestServer(t, nil)
	for _, path := range []string{"/v1/alpha/search", "/codex/v1/alpha/search"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-5.6-sol","commands":{"search_query":[{"q":"test"}]}}`))
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%q, want authenticated route (401)", w.Code, w.Body.String())
			}
		})
	}
}

func TestAlphaSearchRealRouterAndExecutor(t *testing.T) {
	var calls atomic.Int32
	const response = `{"id":"search-result","results":[{"url":"https://example.org"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/alpha/search" {
			t.Errorf("upstream path=%s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if gjson.GetBytes(body, "model").String() != "upstream-search-model" || gjson.GetBytes(body, "commands.search_query.0.q").String() != "test" || gjson.GetBytes(body, "prompt_cache_key").Exists() {
			t.Errorf("body=%s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	defer upstream.Close()
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}, AuthDir: t.TempDir()}
	cfg.CodexKey = []proxyconfig.CodexKey{{APIKey: "upstream-key", BaseURL: upstream.URL + "/v1", AlphaSearch: true, Models: []proxyconfig.CodexModel{{Name: "upstream-search-model", Alias: "search-alias"}}}}
	cfg.Routing.IncludeDefaultGroup = true
	cfg.Routing.ChannelGroups = []proxyconfig.RoutingChannelGroup{{Name: "search", Match: proxyconfig.ChannelGroupMatch{Channels: []string{"Search Channel"}}, AllowedModels: []string{"search-alias"}}}
	cfg.Routing.PathRoutes = []proxyconfig.RoutingPathRoute{{Path: "/team/search", Group: "search"}}
	cfg.SanitizeRouting()
	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	a := &auth.Auth{ID: "alpha-router-auth", Label: "Search Channel", Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "upstream-key", "base_url": upstream.URL + "/v1", "alpha_search": "true"}}
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "search-alias"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	server := NewServer(cfg, manager, sdkaccess.NewManager(), filepath.Join(t.TempDir(), "config.yaml"), WithRequestLoggerFactory(nil))
	for _, path := range []string{"/v1/alpha/search", "/search/v1/alpha/search", "/team/search/v1/alpha/search"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"search-alias","commands":{"search_query":[{"q":"test"}]},"prompt_cache_key":"remove"}`))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.String() != response {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	for _, body := range []string{`null`, `[]`, `{"commands":{}}`, `{"model":123}`, `{"model":"search-alias","stream":true}`} {
		req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/search/v1/alpha/search", strings.NewReader(`{"model":"forbidden-model","commands":{}}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Fatalf("model gate status=%d body=%s", rec.Code, rec.Body.String())
	}
	a.Attributes["alpha_search"] = "false"
	if _, err := manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"model":"search-alias","commands":{}}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "alpha_search_auth_unavailable") {
		t.Fatalf("capability status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 3 {
		t.Fatalf("rejected request reached upstream, calls=%d", calls.Load())
	}
}

func TestAlphaSearchDoesNotInjectSystemPrompt(t *testing.T) {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("accessMetadata", map[string]string{"system-prompt": "secret prompt"})
		c.Next()
	}, SystemPromptMiddleware())
	router.POST("/v1/alpha/search", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Error(err)
		}
		c.Data(200, "application/json", body)
	})
	const body = `{"model":"test","commands":{},"messages":[],"input":"opaque extension"}`
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(body)))
	if rec.Code != 200 || rec.Body.String() != body {
		t.Fatalf("search body changed: %s", rec.Body.String())
	}
}
