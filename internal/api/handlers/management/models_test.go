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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
		"input_modalities": ["text", "image"],
		"output_modalities": ["text"],
		"pricing": {
			"mode": "call",
			"price_per_call": 0.12
		}
	}`)
	createRec := performModelsRequest(http.MethodPost, "/model-configs", createBody, h.Models().PostModelConfig)
	if createRec.Code != http.StatusOK {
		t.Fatalf("PostModelConfig status = %d body = %s", createRec.Code, createRec.Body.String())
	}

	listRec := performModelsRequest(http.MethodGet, "/model-configs", nil, h.Models().GetModelConfigs)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GetModelConfigs status = %d body = %s", listRec.Code, listRec.Body.String())
	}
	var listPayload struct {
		Data []struct {
			ID               string   `json:"id"`
			OwnedBy          string   `json:"owned_by"`
			InputModalities  []string `json:"input_modalities"`
			OutputModalities []string `json:"output_modalities"`
			SupportsVision   bool     `json:"supports_vision"`
			Pricing          struct {
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
			if !item.SupportsVision || len(item.InputModalities) != 2 || item.InputModalities[1] != "image" || len(item.OutputModalities) != 1 || item.OutputModalities[0] != "text" {
				t.Fatalf("unexpected custom-image capabilities: %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("expected custom-image in list response")
	}

	deleteRec := performModelsRequest(http.MethodDelete, "/model-configs/custom-image", nil, func(c *gin.Context) {
		c.Params = gin.Params{{Key: "id", Value: "custom-image"}}
		h.Models().DeleteModelConfig(c)
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
	createRec := performModelsRequest(http.MethodPost, "/model-configs", createBody, h.Models().PostModelConfig)
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
	createLibraryRec := performModelsRequest(http.MethodPost, "/model-configs?scope=library", createLibraryBody, h.Models().PostModelConfig)
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

	activeSources := decodeSources(performModelsRequest(http.MethodGet, "/model-configs", nil, h.Models().GetModelConfigs))
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

	librarySources := decodeSources(performModelsRequest(http.MethodGet, "/model-configs?scope=library", nil, h.Models().GetModelConfigs))
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

	allSources := decodeSources(performModelsRequest(http.MethodGet, "/model-configs?scope=all", nil, h.Models().GetModelConfigs))
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

func TestScopedModelsIncludeOwnerMappedConfiguredModels(t *testing.T) {
	initManagementModelsTestDB(t)
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name: "kimi+deepseek v4 flash",
					Match: config.ChannelGroupMatch{
						Channels: []string{"kimi-B", "opencode go"},
					},
				},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kimi-b",
		Provider: "kimi",
		Label:    "kimi-B",
	}); err != nil {
		t.Fatalf("Register kimi auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "opencode-go",
		Provider: "opencode-go",
		Label:    "opencode go",
	}); err != nil {
		t.Fatalf("Register opencode auth: %v", err)
	}
	if err := usage.UpsertAuthGroupOwnerMapping(usage.AuthGroupOwnerMappingRow{
		AuthGroup: "kimi",
		Owner:     "kimi-code",
	}); err != nil {
		t.Fatalf("UpsertAuthGroupOwnerMapping: %v", err)
	}
	for _, row := range []usage.ModelConfigRow{
		{ModelID: "kimi-k2.7", OwnedBy: "kimi-code", Description: "Kimi K2.7", Enabled: true, Source: "seed"},
		{ModelID: "kimi-k2.7-code", OwnedBy: "kimi-code", Description: "Kimi K2.7 Code", Enabled: true, Source: "seed"},
		{ModelID: "qwen3.5-plus", OwnedBy: "opencode", Description: "Qwen 3.5 Plus", Enabled: true, Source: "opencode-go"},
		{ModelID: "kimi-disabled", OwnedBy: "kimi-code", Description: "Disabled Kimi", Enabled: false, Source: "seed"},
		{ModelID: "other-owner", OwnedBy: "other", Description: "Other owner", Enabled: true, Source: "seed"},
	} {
		if err := usage.UpsertModelConfig(row); err != nil {
			t.Fatalf("UpsertModelConfig(%s): %v", row.ModelID, err)
		}
	}
	h := NewHandler(cfg, "", manager)

	decodeIDs := func(rec *httptest.ResponseRecorder) map[string]struct{} {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		ids := make(map[string]struct{}, len(payload.Data))
		for _, item := range payload.Data {
			ids[item.ID] = struct{}{}
		}
		return ids
	}

	models := decodeIDs(performModelsRequest(
		http.MethodGet,
		"/models?allowed_channel_groups=kimi%2Bdeepseek+v4+flash",
		nil,
		h.Models().GetModels,
	))
	for _, id := range []string{"kimi-k2.7", "kimi-k2.7-code", "qwen3.5-plus"} {
		if _, ok := models[id]; !ok {
			t.Fatalf("/models missing owner-mapped configured model %q; ids=%v", id, models)
		}
	}
	for _, id := range []string{"kimi-disabled", "other-owner"} {
		if _, ok := models[id]; ok {
			t.Fatalf("/models unexpectedly included %q; ids=%v", id, models)
		}
	}

	availability := decodeIDs(performModelsRequest(
		http.MethodGet,
		"/models/configured-availability?allowed_channel_groups=kimi%2Bdeepseek+v4+flash",
		nil,
		h.Models().GetConfiguredModelAvailability,
	))
	for _, id := range []string{"kimi-k2.7", "kimi-k2.7-code", "qwen3.5-plus"} {
		if _, ok := availability[id]; !ok {
			t.Fatalf("/models/configured-availability missing owner-mapped configured model %q; ids=%v", id, availability)
		}
	}

	channelScopedModels := decodeIDs(performModelsRequest(
		http.MethodGet,
		"/models?allowed_channels=kimi-B,opencode+go",
		nil,
		h.Models().GetModels,
	))
	for _, id := range []string{"kimi-k2.7", "kimi-k2.7-code", "qwen3.5-plus"} {
		if _, ok := channelScopedModels[id]; !ok {
			t.Fatalf("/models allowed_channels missing configured model %q; ids=%v", id, channelScopedModels)
		}
	}
}

func TestScopedModelsHonorChannelGroupAllowedModelsForConfiguredRows(t *testing.T) {
	initManagementModelsTestDB(t)
	cfg := &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:          "kimi-limited",
					AllowedModels: []string{"kimi-k2.7-code"},
					Match: config.ChannelGroupMatch{
						Channels: []string{"kimi-B"},
					},
				},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "kimi-b-limited",
		Provider: "kimi",
		Label:    "kimi-B",
	}); err != nil {
		t.Fatalf("Register kimi auth: %v", err)
	}
	if err := usage.UpsertAuthGroupOwnerMapping(usage.AuthGroupOwnerMappingRow{
		AuthGroup: "kimi",
		Owner:     "kimi-code",
	}); err != nil {
		t.Fatalf("UpsertAuthGroupOwnerMapping: %v", err)
	}
	for _, row := range []usage.ModelConfigRow{
		{ModelID: "kimi-k2.7", OwnedBy: "kimi-code", Description: "Kimi K2.7", Enabled: true, Source: "seed"},
		{ModelID: "kimi-k2.7-code", OwnedBy: "kimi-code", Description: "Kimi K2.7 Code", Enabled: true, Source: "seed"},
	} {
		if err := usage.UpsertModelConfig(row); err != nil {
			t.Fatalf("UpsertModelConfig(%s): %v", row.ModelID, err)
		}
	}
	h := NewHandler(cfg, "", manager)
	rec := performModelsRequest(
		http.MethodGet,
		"/models/configured-availability?allowed_channel_groups=kimi-limited",
		nil,
		h.Models().GetConfiguredModelAvailability,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		ActiveMetadata []struct {
			ID string `json:"id"`
		} `json:"active_metadata"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	ids := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		ids[item.ID] = struct{}{}
	}
	if _, ok := ids["kimi-k2.7-code"]; !ok {
		t.Fatalf("expected allowed configured model in response; ids=%v", ids)
	}
	if _, ok := ids["kimi-k2.7"]; ok {
		t.Fatalf("did not expect configured model outside group allowed-models; ids=%v", ids)
	}
	metadataIDs := make(map[string]struct{}, len(payload.ActiveMetadata))
	for _, item := range payload.ActiveMetadata {
		metadataIDs[item.ID] = struct{}{}
	}
	if _, ok := metadataIDs["kimi-k2.7-code"]; !ok {
		t.Fatalf("expected allowed configured model metadata; ids=%v", metadataIDs)
	}
	if _, ok := metadataIDs["kimi-k2.7"]; ok {
		t.Fatalf("did not expect metadata outside group allowed-models; ids=%v", metadataIDs)
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
	putRec := performModelsRequest(http.MethodPut, "/model-owner-presets", body, h.Models().PutModelOwnerPresets)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PutModelOwnerPresets status = %d body = %s", putRec.Code, putRec.Body.String())
	}

	getRec := performModelsRequest(http.MethodGet, "/model-owner-presets", nil, h.Models().GetModelOwnerPresets)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GetModelOwnerPresets status = %d body = %s", getRec.Code, getRec.Body.String())
	}
	if _, ok := usage.GetModelOwnerPreset("acme-ai"); !ok {
		t.Fatal("expected acme-ai owner preset")
	}
}

func TestAuthGroupModelOwnerMappingHandlersPatchAndList(t *testing.T) {
	initManagementModelsTestDB(t)
	h := NewHandler(&config.Config{}, "", nil)

	patchRec := performModelsRequest(
		http.MethodPatch,
		"/auth-group-model-owner-mappings",
		[]byte(`{"auth_group":"claude","owner":"anthropic"}`),
		h.Models().PatchAuthGroupModelOwnerMapping,
	)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PatchAuthGroupModelOwnerMapping status = %d body = %s", patchRec.Code, patchRec.Body.String())
	}

	listRec := performModelsRequest(
		http.MethodGet,
		"/auth-group-model-owner-mappings",
		nil,
		h.Models().GetAuthGroupModelOwnerMappings,
	)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GetAuthGroupModelOwnerMappings status = %d body = %s", listRec.Code, listRec.Body.String())
	}

	var listPayload struct {
		Items []struct {
			AuthGroup string `json:"auth_group"`
			Owner     string `json:"owner"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("unmarshal auth group owner mapping list: %v", err)
	}
	if len(listPayload.Items) != 1 || listPayload.Items[0].AuthGroup != "claude" || listPayload.Items[0].Owner != "anthropic" {
		t.Fatalf("unexpected auth group owner mapping list: %+v", listPayload.Items)
	}

	deleteRec := performModelsRequest(
		http.MethodPatch,
		"/auth-group-model-owner-mappings",
		[]byte(`{"auth_group":"claude","owner":""}`),
		h.Models().PatchAuthGroupModelOwnerMapping,
	)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("PatchAuthGroupModelOwnerMapping delete status = %d body = %s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, ok := usage.GetAuthGroupOwnerMapping("claude"); ok {
		t.Fatal("expected claude auth group owner mapping to be deleted")
	}
}

func TestGetModelPathAvailabilityIncludesRootAndConfiguredPath(t *testing.T) {
	modelID := "model-path-availability-test"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("model-path-availability-client", "openai", []*registry.ModelInfo{
		{ID: modelID, Object: "model", OwnedBy: "openai", Type: "openai"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("model-path-availability-client")
	})

	h := NewHandler(&config.Config{
		Routing: config.RoutingConfig{
			PathRoutes: []config.RoutingPathRoute{
				{Path: "/team-a", Group: "team-a"},
			},
		},
	}, "", nil)

	rec := performModelsRequest(http.MethodGet, "/model-path-availability", nil, h.Models().GetModelPathAvailability)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetModelPathAvailability status = %d body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Routes []struct {
			Path         string `json:"path"`
			System       bool   `json:"system"`
			Capabilities []struct {
				Method string `json:"method"`
				Path   string `json:"path"`
				Label  string `json:"label"`
			} `json:"capabilities"`
		} `json:"routes"`
		Data []struct {
			ID    string `json:"id"`
			Paths []struct {
				Scope  string `json:"scope"`
				Method string `json:"method"`
				Path   string `json:"path"`
				Label  string `json:"label"`
			} `json:"paths"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	hasRootRoute := false
	hasTeamRoute := false
	for _, route := range payload.Routes {
		if route.Path == "/" && route.System {
			hasRootRoute = true
		}
		if route.Path == "/team-a" && !route.System {
			hasTeamRoute = true
		}
	}
	if !hasRootRoute {
		t.Fatal("expected system root route")
	}
	if !hasTeamRoute {
		t.Fatal("expected configured /team-a route")
	}

	var gotModel *struct {
		ID    string `json:"id"`
		Paths []struct {
			Scope  string `json:"scope"`
			Method string `json:"method"`
			Path   string `json:"path"`
			Label  string `json:"label"`
		} `json:"paths"`
	}
	for i := range payload.Data {
		if payload.Data[i].ID == modelID {
			gotModel = &payload.Data[i]
			break
		}
	}
	if gotModel == nil {
		t.Fatalf("expected model %q in response", modelID)
	}

	hasRootModels := false
	hasTeamModels := false
	for _, path := range gotModel.Paths {
		if path.Scope == "root" && path.Method == http.MethodGet && path.Path == "/v1/models" {
			hasRootModels = true
		}
		if path.Scope == "group" && path.Method == http.MethodGet && path.Path == "/team-a/v1/models" {
			hasTeamModels = true
		}
	}
	if !hasRootModels {
		t.Fatalf("expected root /v1/models path for %q", modelID)
	}
	if !hasTeamModels {
		t.Fatalf("expected /team-a/v1/models path for %q", modelID)
	}
}

func TestGetModelPathAvailabilityFiltersConfiguredPathByRouteGroup(t *testing.T) {
	allowedModelID := "model-path-availability-team-allowed"
	blockedModelID := "model-path-availability-team-blocked"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("model-path-availability-team-auth", "openai", []*registry.ModelInfo{
		{ID: allowedModelID, Object: "model", OwnedBy: "openai", Type: "openai"},
		{ID: blockedModelID, Object: "model", OwnedBy: "openai", Type: "openai"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("model-path-availability-team-auth")
	})

	cfg := &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:          "team-a",
					AllowedModels: []string{allowedModelID},
				},
			},
			PathRoutes: []config.RoutingPathRoute{
				{Path: "/team-a", Group: "team-a"},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "model-path-availability-team-auth",
		Provider: "openai",
		Prefix:   "team-a",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandler(cfg, "", manager)
	rec := performModelsRequest(http.MethodGet, "/model-path-availability", nil, h.Models().GetModelPathAvailability)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetModelPathAvailability status = %d body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Data []struct {
			ID    string `json:"id"`
			Paths []struct {
				Method string `json:"method"`
				Path   string `json:"path"`
			} `json:"paths"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	hasTeamPath := func(modelID string) bool {
		t.Helper()
		for _, item := range payload.Data {
			if item.ID != modelID {
				continue
			}
			for _, path := range item.Paths {
				if path.Method == http.MethodGet && path.Path == "/team-a/v1/models" {
					return true
				}
			}
		}
		return false
	}

	if !hasTeamPath(allowedModelID) {
		t.Fatalf("expected %q to have /team-a/v1/models path", allowedModelID)
	}
	if hasTeamPath(blockedModelID) {
		t.Fatalf("did not expect %q to have /team-a/v1/models path", blockedModelID)
	}
}

func TestGetModelPathAvailabilityFiltersRootByDefaultAllowedModels(t *testing.T) {
	allowedModelID := "model-path-availability-default-allowed"
	blockedModelID := "model-path-availability-default-blocked"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("model-path-availability-default-auth", "openai", []*registry.ModelInfo{
		{ID: allowedModelID, Object: "model", OwnedBy: "openai", Type: "openai"},
		{ID: blockedModelID, Object: "model", OwnedBy: "openai", Type: "openai"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("model-path-availability-default-auth")
	})

	cfg := &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:          "default",
					AllowedModels: []string{allowedModelID},
				},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "model-path-availability-default-auth",
		Provider: "openai",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandler(cfg, "", manager)
	rec := performModelsRequest(http.MethodGet, "/model-path-availability", nil, h.Models().GetModelPathAvailability)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetModelPathAvailability status = %d body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Data []struct {
			ID    string `json:"id"`
			Paths []struct {
				Method string `json:"method"`
				Path   string `json:"path"`
			} `json:"paths"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	hasRootPath := func(modelID string) bool {
		t.Helper()
		for _, item := range payload.Data {
			if item.ID != modelID {
				continue
			}
			for _, path := range item.Paths {
				if path.Method == http.MethodGet && path.Path == "/v1/models" {
					return true
				}
			}
		}
		return false
	}

	if !hasRootPath(allowedModelID) {
		t.Fatalf("expected %q to have root /v1/models path", allowedModelID)
	}
	if hasRootPath(blockedModelID) {
		t.Fatalf("did not expect %q to have root /v1/models path", blockedModelID)
	}
}

func TestGetModelPathAvailabilityExcludesIsolatedGroupFromRoot(t *testing.T) {
	rootModelID := "model-path-availability-root-default"
	isolatedModelID := "model-path-availability-root-isolated"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("model-path-availability-root-default-auth", "openai", []*registry.ModelInfo{
		{ID: rootModelID, Object: "model", OwnedBy: "openai", Type: "openai"},
	})
	reg.RegisterClient("model-path-availability-root-isolated-auth", "openai", []*registry.ModelInfo{
		{ID: isolatedModelID, Object: "model", OwnedBy: "openai", Type: "openai"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("model-path-availability-root-default-auth")
		reg.UnregisterClient("model-path-availability-root-isolated-auth")
	})

	cfg := &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:               "kimicode",
					ExcludeFromDefault: true,
					Match: config.ChannelGroupMatch{
						Channels: []string{"Kimi Channel"},
					},
				},
			},
			PathRoutes: []config.RoutingPathRoute{
				{Path: "/kimicode", Group: "kimicode"},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	for _, auth := range []*coreauth.Auth{
		{
			ID:       "model-path-availability-root-default-auth",
			Label:    "Default Channel",
			Provider: "openai",
		},
		{
			ID:       "model-path-availability-root-isolated-auth",
			Label:    "Kimi Channel",
			Provider: "openai",
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}

	h := NewHandler(cfg, "", manager)
	rec := performModelsRequest(http.MethodGet, "/model-path-availability", nil, h.Models().GetModelPathAvailability)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetModelPathAvailability status = %d body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Data []struct {
			ID    string `json:"id"`
			Paths []struct {
				Method string `json:"method"`
				Path   string `json:"path"`
			} `json:"paths"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	hasPath := func(modelID, path string) bool {
		t.Helper()
		for _, item := range payload.Data {
			if item.ID != modelID {
				continue
			}
			for _, itemPath := range item.Paths {
				if itemPath.Method == http.MethodGet && itemPath.Path == path {
					return true
				}
			}
		}
		return false
	}

	if !hasPath(rootModelID, "/v1/models") {
		t.Fatalf("expected %q to have root /v1/models path", rootModelID)
	}
	if hasPath(isolatedModelID, "/v1/models") {
		t.Fatalf("did not expect %q to have root /v1/models path", isolatedModelID)
	}
	if !hasPath(isolatedModelID, "/kimicode/v1/models") {
		t.Fatalf("expected %q to have /kimicode/v1/models path", isolatedModelID)
	}
}

func TestGetModelPathAvailabilityIncludesCcSwitchRoutePaths(t *testing.T) {
	initManagementModelsTestDB(t)

	modelID := "model-path-availability-ccswitch"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("model-path-availability-ccswitch-auth", "openai", []*registry.ModelInfo{
		{ID: modelID, Object: "model", OwnedBy: "openai", Type: "openai"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("model-path-availability-ccswitch-auth")
	})

	if err := usage.ReplaceAllCcSwitchImportConfigs([]usage.CcSwitchImportConfigRow{
		{
			ID:                   "cfg-model-path-availability",
			ClientType:           "claude",
			ProviderName:         "Team Relay",
			DefaultModel:         modelID,
			AllowedChannelGroups: []string{"team-a"},
			RoutePath:            "/ccswitch/team-a",
			EndpointPath:         "/v1",
			UsageAutoInterval:    30,
		},
	}); err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs() error = %v", err)
	}

	cfg := &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "team-a"},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "model-path-availability-ccswitch-auth",
		Provider: "openai",
		Prefix:   "team-a",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	h := NewHandler(cfg, "", manager)
	rec := performModelsRequest(http.MethodGet, "/model-path-availability", nil, h.Models().GetModelPathAvailability)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetModelPathAvailability status = %d body = %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Routes []struct {
			Path  string `json:"path"`
			Group string `json:"group"`
		} `json:"routes"`
		Data []struct {
			ID    string `json:"id"`
			Paths []struct {
				Method string `json:"method"`
				Path   string `json:"path"`
			} `json:"paths"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	hasRoute := false
	for _, route := range payload.Routes {
		if route.Path == "/ccswitch/team-a" && route.Group == "team-a" {
			hasRoute = true
			break
		}
	}
	if !hasRoute {
		t.Fatal("expected ccswitch route /ccswitch/team-a in route list")
	}

	for _, item := range payload.Data {
		if item.ID != modelID {
			continue
		}
		for _, path := range item.Paths {
			if path.Method == http.MethodGet && path.Path == "/ccswitch/team-a/v1/models" {
				return
			}
		}
	}
	t.Fatalf("expected %q to include /ccswitch/team-a/v1/models path", modelID)
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
	putRec := performModelsRequest(http.MethodPut, "/model-openrouter-sync", putBody, h.Models().PutOpenRouterModelSync)
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

	runRec := performModelsRequest(http.MethodPost, "/model-openrouter-sync/run", nil, h.Models().PostOpenRouterModelSyncRun)
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

	getRec := performModelsRequest(http.MethodGet, "/model-openrouter-sync", nil, h.Models().GetOpenRouterModelSync)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GetOpenRouterModelSync status = %d body = %s", getRec.Code, getRec.Body.String())
	}
}
