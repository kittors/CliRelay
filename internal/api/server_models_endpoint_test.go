package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestUnifiedModelsHandlerScopesBusinessTenantToPortalPlazaAndAllowedModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		businessTenant = "cccccccc-dddd-eeee-ffff-000000000001"
		businessAuthID = "models-business-tenant-codex-auth"
		visibleModelA  = "gpt-business-visible-a"
		visibleModelB  = "gpt-business-visible-b"
		extraModel     = "gpt-business-static-extra"
		discoveryOnly  = "gpt-business-discovery-only"
	)

	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(businessAuthID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(businessAuthID) })
	modelRegistry.RegisterClient(businessAuthID, "codex", []*registry.ModelInfo{
		{ID: visibleModelA, Object: "model", OwnedBy: "openai"},
		{ID: visibleModelB, Object: "model", OwnedBy: "openai"},
		{ID: extraModel, Object: "model", OwnedBy: "openai"},
	})
	// Management plaza replaces the Codex static registry catalog with this
	// tenant-scoped live discovery set. discoveryOnly is intentionally absent
	// from the runtime registry, while extraModel is intentionally absent here.
	managementauthfiles.StoreDiscoveryCacheForTest(businessTenant, "codex", []*registry.ModelInfo{
		{ID: visibleModelA, Object: "model", OwnedBy: "openai"},
		{ID: visibleModelB, Object: "model", OwnedBy: "openai"},
		{ID: discoveryOnly, Object: "model", OwnedBy: "openai"},
	})

	authManager := coreauth.NewManager(nil, nil, nil)
	authManager.SetConfigForTenant(businessTenant, &config.Config{})
	if _, err := authManager.Register(context.Background(), &coreauth.Auth{
		ID: businessAuthID, TenantID: businessTenant, Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register business auth: %v", err)
	}

	cfg := &config.Config{}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager)
	server := &Server{handlers: base, cfg: cfg}
	openaiHandler := openai.NewOpenAIAPIHandler(base)
	claudeHandler := claude.NewClaudeCodeAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		c.Set("tenantID", businessTenant)
		if c.Query("restricted") == "1" {
			// Include extraModel to prove allowed-models cannot re-introduce an ID
			// removed by the plaza truth source.
			c.Set("accessMetadata", map[string]string{"allowed-models": visibleModelA + "," + extraModel})
		}
		server.unifiedModelsHandler(openaiHandler, claudeHandler)(c)
	})

	responseIDs := func(target, userAgent string) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body=%s", target, rec.Code, rec.Body.String())
		}
		var response struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("unmarshal GET %s response: %v", target, err)
		}
		return modelIDs(response.Data)
	}

	ids := responseIDs("/v1/models", "")
	if !sameStringSet(ids, []string{visibleModelA, visibleModelB}) {
		t.Fatalf("business tenant OpenAI models = %#v, want configured∩root-path registry models", ids)
	}

	ids = responseIDs("/v1/models", "claude-cli/1.0")
	if !sameStringSet(ids, []string{visibleModelA, visibleModelB}) {
		t.Fatalf("business tenant Claude models = %#v, want configured∩root-path registry models", ids)
	}

	ids = responseIDs("/v1/models?restricted=1", "")
	if !sameStringSet(ids, []string{visibleModelA}) {
		t.Fatalf("restricted business tenant models = %#v, want only %q", ids, visibleModelA)
	}
}

// An empty catalog for a business tenant means an empty listing. The portal
// filter must key on "a catalog was computed" (non-nil), not on "the catalog has
// entries": a len>0 check silently skipped the filter for an empty catalog and
// let the tenant see every registry model instead of none.
func TestUnifiedModelsHandlerEmptyTenantCatalogYieldsEmptyListing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		businessTenant = "cccccccc-dddd-eeee-ffff-000000000002"
		businessAuthID = "models-business-tenant-empty-catalog-auth"
		registryModel  = "gpt-business-registry-only"
		catalogOnly    = "gpt-business-catalog-only"
	)

	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(businessAuthID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(businessAuthID) })
	modelRegistry.RegisterClient(businessAuthID, "codex", []*registry.ModelInfo{
		{ID: registryModel, Object: "model", OwnedBy: "openai"},
	})
	// The tenant catalog and the runtime registry do not intersect, so the
	// portal-visible set is empty.
	managementauthfiles.StoreDiscoveryCacheForTest(businessTenant, "codex", []*registry.ModelInfo{
		{ID: catalogOnly, Object: "model", OwnedBy: "openai"},
	})

	authManager := coreauth.NewManager(nil, nil, nil)
	authManager.SetConfigForTenant(businessTenant, &config.Config{})
	if _, err := authManager.Register(context.Background(), &coreauth.Auth{
		ID: businessAuthID, TenantID: businessTenant, Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register business auth: %v", err)
	}

	cfg := &config.Config{}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager)
	server := &Server{handlers: base, cfg: cfg}
	openaiHandler := openai.NewOpenAIAPIHandler(base)
	claudeHandler := claude.NewClaudeCodeAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		c.Set("tenantID", businessTenant)
		server.unifiedModelsHandler(openaiHandler, claudeHandler)(c)
	})

	for _, target := range []string{"/v1/models", "/v1/models?client_version=pi"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body=%s", target, rec.Code, rec.Body.String())
		}
		var response struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("unmarshal GET %s response: %v", target, err)
		}
		if ids := modelIDs(response.Data); len(ids) != 0 {
			t.Fatalf("GET %s for a tenant with an empty catalog = %#v, want no models", target, ids)
		}
	}
}

func TestUnifiedModelsHandlerKeepsSystemTenantRegistryBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		systemAuthID = "models-system-tenant-codex-auth"
		visibleModel = "gpt-system-live"
		staticOnly   = "gpt-system-static-only"
	)

	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(systemAuthID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(systemAuthID) })
	modelRegistry.RegisterClient(systemAuthID, "codex", []*registry.ModelInfo{
		{ID: visibleModel, Object: "model", OwnedBy: "openai"},
		{ID: staticOnly, Object: "model", OwnedBy: "openai"},
	})
	managementauthfiles.StoreDiscoveryCacheForTest(identity.SystemTenantID, "codex", []*registry.ModelInfo{
		{ID: visibleModel, Object: "model", OwnedBy: "openai"},
	})

	authManager := coreauth.NewManager(nil, nil, nil)
	authManager.SetConfigForTenant(identity.SystemTenantID, &config.Config{})
	if _, err := authManager.Register(context.Background(), &coreauth.Auth{
		ID: systemAuthID, TenantID: identity.SystemTenantID, Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register system auth: %v", err)
	}

	cfg := &config.Config{}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager)
	server := &Server{handlers: base, cfg: cfg}
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		c.Set("tenantID", identity.SystemTenantID)
		server.unifiedModelsHandler(
			openai.NewOpenAIAPIHandler(base),
			claude.NewClaudeCodeAPIHandler(base),
		)(c)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	ids := modelIDs(response.Data)
	if !containsString(ids, visibleModel) || !containsString(ids, staticOnly) {
		t.Fatalf("system tenant models = %#v, want both registry models", ids)
	}
}

func TestUnifiedModelsHandlerPiCatalogHidesStaticOnlyModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		systemAuthID = "models-pi-catalog-codex-auth"
		liveModel    = "claude-live-discovery"
		staticOnly   = "claude-4-legacy-static"
	)

	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(systemAuthID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(systemAuthID) })
	modelRegistry.RegisterClient(systemAuthID, "codex", []*registry.ModelInfo{
		{ID: liveModel, Object: "model", OwnedBy: "anthropic"},
		{ID: staticOnly, Object: "model", OwnedBy: "anthropic"},
	})
	// Live discovery only knows the current model; staticOnly is the stale
	// registry row the management plaza already hides.
	managementauthfiles.StoreDiscoveryCacheForTest(identity.SystemTenantID, "codex", []*registry.ModelInfo{
		{ID: liveModel, Object: "model", OwnedBy: "anthropic"},
	})

	authManager := coreauth.NewManager(nil, nil, nil)
	authManager.SetConfigForTenant(identity.SystemTenantID, &config.Config{})
	if _, err := authManager.Register(context.Background(), &coreauth.Auth{
		ID: systemAuthID, TenantID: identity.SystemTenantID, Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register system auth: %v", err)
	}

	cfg := &config.Config{}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager)
	server := &Server{handlers: base, cfg: cfg}
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		c.Set("tenantID", identity.SystemTenantID)
		server.unifiedModelsHandler(
			openai.NewOpenAIAPIHandler(base),
			claude.NewClaudeCodeAPIHandler(base),
		)(c)
	})

	requestIDs := func(target string) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body=%s", target, rec.Code, rec.Body.String())
		}
		var response struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("unmarshal GET %s response: %v", target, err)
		}
		return modelIDs(response.Data)
	}

	// The pi provider builds its /model picker from this endpoint, so it must see
	// the curated catalog rather than the raw static registry.
	if ids := requestIDs("/v1/models?client_version=pi"); !sameStringSet(ids, []string{liveModel}) {
		t.Fatalf("pi catalog = %#v, want only %q", ids, liveModel)
	}

	// Every other caller keeps the previous system-tenant behaviour.
	if ids := requestIDs("/v1/models"); !containsString(ids, liveModel) || !containsString(ids, staticOnly) {
		t.Fatalf("system tenant models = %#v, want both registry models", ids)
	}
}

func TestPiCatalogRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		target string
		want   bool
	}{
		{"/v1/models?client_version=pi", true},
		{"/v1/models?client_version=PI", true},
		{"/v1/models?client_version=%20pi%20", true},
		{"/v1/models", false},
		{"/v1/models?client_version=0.180.0", false},
		{"/v1/models?client_version=pid", false},
	}
	for _, testCase := range cases {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, testCase.target, nil)
		if got := piCatalogRequest(ctx); got != testCase.want {
			t.Errorf("piCatalogRequest(%q) = %v, want %v", testCase.target, got, testCase.want)
		}
	}
	if piCatalogRequest(nil) {
		t.Error("piCatalogRequest(nil) = true, want false")
	}
}

// A preset's mappings rename models without narrowing the list: what the route
// serves is its channel group, already applied by the scope filter. Listing only
// the mapped models hid every model the upstream added after the preset was
// saved, the same models the request gate used to refuse on the preset route.
func TestFilterCodexModelsForCcSwitchRouteRenamesMappedModelsAndKeepsTheRest(t *testing.T) {
	models := []map[string]interface{}{
		{"id": "deepseek-chat", "object": "model", "owned_by": "deepseek"},
		{"id": "kimi-k2", "object": "model", "owned_by": "moonshot"},
		{"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
	}
	route := &internalrouting.PathRouteContext{
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ClientType:   "codex",
			DefaultModel: "deepseek-v4-flash",
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{RequestModel: "deepseek-v4-flash", TargetModel: "deepseek-chat"},
				{RequestModel: "deepseek-v4-pro", TargetModel: "deepseek-chat"},
			},
		},
	}

	filtered := filterModelsForCcSwitchRoute(models, route)
	got := modelIDs(filtered)
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro", "kimi-k2", "gpt-5.5"}
	if !sameStrings(got, want) {
		t.Fatalf("model ids = %#v, want %#v", got, want)
	}
	if filtered[0]["owned_by"] != "deepseek" {
		t.Fatalf("owned_by = %v, want deepseek", filtered[0]["owned_by"])
	}
}

// A request name that a mapping sends elsewhere reaches the mapping's target on
// this route, so the model that owns that name must not be listed under it too.
func TestFilterCodexModelsForCcSwitchRouteListsAShadowedNameOnce(t *testing.T) {
	models := []map[string]interface{}{
		{"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
		{"id": "deepseek-chat", "object": "model", "owned_by": "deepseek"},
	}
	route := &internalrouting.PathRouteContext{
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ClientType:    "codex",
			ModelMappings: []internalrouting.CcSwitchModelMapping{{RequestModel: "gpt-5.5", TargetModel: "deepseek-chat"}},
		},
	}

	filtered := filterModelsForCcSwitchRoute(models, route)
	if got := modelIDs(filtered); !sameStrings(got, []string{"gpt-5.5"}) {
		t.Fatalf("model ids = %#v, want only the mapped name", got)
	}
	if filtered[0]["owned_by"] != "deepseek" {
		t.Fatalf("owned_by = %v, want the mapping target's deepseek", filtered[0]["owned_by"])
	}
}

func TestCcSwitchRequestModelAllowedForTarget(t *testing.T) {
	route := &internalrouting.PathRouteContext{
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{RequestModel: "deepseek-v4-flash", TargetModel: "deepseek-chat"},
			},
		},
	}

	if !ccSwitchRequestModelAllowedForTarget("deepseek-chat", route, map[string]struct{}{"deepseek-v4-flash": {}}) {
		t.Fatal("request model alias was not allowed for target")
	}
	if ccSwitchRequestModelAllowedForTarget("kimi-k2", route, map[string]struct{}{"deepseek-v4-flash": {}}) {
		t.Fatal("unmapped target was allowed")
	}
}

func TestEnrichOpenAIModelsWithCatalogLeavesUnknownModels(t *testing.T) {
	models := []map[string]interface{}{
		{"id": "unknown-model-xyz", "object": "model"},
	}
	enrichOpenAIModelsWithCatalog("", models)
	if _, hasPricing := models[0]["pricing"]; hasPricing {
		t.Fatal("unexpected pricing for unknown model")
	}
	if _, hasDesc := models[0]["description"]; hasDesc {
		t.Fatal("unexpected description for unknown model")
	}
}

func TestEnrichOpenAIModelsWithCatalogInheritsPrefixedPricing(t *testing.T) {
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("usage.InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)

	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:               "deepseek-v4-flash",
		Enabled:               true,
		PricingMode:           "token",
		InputPricePerMillion:  0.3,
		OutputPricePerMillion: 1.2,
		Source:                "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	models := []map[string]interface{}{{
		"id":     "ollama/deepseek-v4-flash",
		"object": "model",
	}}
	enrichOpenAIModelsWithCatalog("", models)
	pricing, ok := models[0]["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("pricing = %#v, want inherited pricing map", models[0]["pricing"])
	}
	if pricing["input_price_per_million"] != 0.3 || pricing["output_price_per_million"] != 1.2 {
		t.Fatalf("pricing = %#v, want input=0.3 output=1.2", pricing)
	}
}

func modelIDs(models []map[string]interface{}) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id, ok := model["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, value := range a {
		if !containsString(b, value) {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFilterModelsForCcSwitchRouteListsRequestNamesForOpenAIProtocolApps(t *testing.T) {
	models := []map[string]interface{}{
		{"id": "deepseek-chat", "object": "model", "owned_by": "deepseek"},
		{"id": "kimi-k2", "object": "model", "owned_by": "moonshot"},
		{"id": "glm-5", "object": "model", "owned_by": "zhipu"},
	}
	mappings := []internalrouting.CcSwitchModelMapping{
		{RequestModel: "gpt-5.4", TargetModel: "deepseek-chat"},
		{RequestModel: "kimi-k2", TargetModel: "kimi-k2"},
	}
	for _, clientType := range []string{"opencode", "openclaw", "hermes"} {
		route := &internalrouting.PathRouteContext{
			CcSwitch: &internalrouting.CcSwitchRouteContext{ClientType: clientType, DefaultModel: "kimi-k2", ModelMappings: mappings},
		}
		got := modelIDs(filterModelsForCcSwitchRoute(models, route))
		if want := []string{"gpt-5.4", "kimi-k2", "glm-5"}; !sameStrings(got, want) {
			t.Fatalf("%s model ids = %#v, want %#v", clientType, got, want)
		}
	}

	// Gemini CLI never has its request rewritten, so it keeps seeing the real ids.
	route := &internalrouting.PathRouteContext{
		CcSwitch: &internalrouting.CcSwitchRouteContext{ClientType: "gemini", DefaultModel: "kimi-k2", ModelMappings: mappings},
	}
	got := modelIDs(filterModelsForCcSwitchRoute(models, route))
	if want := []string{"deepseek-chat", "kimi-k2", "glm-5"}; !sameStrings(got, want) {
		t.Fatalf("gemini model ids = %#v, want %#v", got, want)
	}
}
