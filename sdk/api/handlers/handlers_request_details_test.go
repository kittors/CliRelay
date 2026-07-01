package handlers

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestGetRequestDetails_PreservesSuffix(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()

	modelRegistry.RegisterClient("test-request-details-gemini", "gemini", []*registry.ModelInfo{
		{ID: "gemini-2.5-pro", Created: now + 30},
		{ID: "gemini-2.5-flash", Created: now + 25},
	})
	modelRegistry.RegisterClient("test-request-details-openai", "openai", []*registry.ModelInfo{
		{ID: "gpt-5.2", Created: now + 20},
	})
	modelRegistry.RegisterClient("test-request-details-claude", "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-5", Created: now + 5},
	})
	modelRegistry.RegisterClient("test-request-details-opencode-go", "opencode-go", []*registry.ModelInfo{
		{ID: "deepseek-v4-flash", Created: now + 10},
	})
	modelRegistry.RegisterClient("test-request-details-custom", "openai", []*registry.ModelInfo{
		{ID: "custom-model[beta]", Created: now + 15},
	})

	// Ensure cleanup of all test registrations.
	clientIDs := []string{
		"test-request-details-gemini",
		"test-request-details-openai",
		"test-request-details-claude",
		"test-request-details-opencode-go",
	}
	for _, clientID := range clientIDs {
		id := clientID
		t.Cleanup(func() {
			modelRegistry.UnregisterClient(id)
		})
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	tests := []struct {
		name          string
		inputModel    string
		wantProviders []string
		wantModel     string
		wantErr       bool
	}{
		{
			name:          "numeric suffix preserved",
			inputModel:    "gemini-2.5-pro(8192)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-pro(8192)",
			wantErr:       false,
		},
		{
			name:          "level suffix preserved",
			inputModel:    "gpt-5.2(high)",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5.2(high)",
			wantErr:       false,
		},
		{
			name:          "no suffix unchanged",
			inputModel:    "claude-sonnet-4-5",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5",
			wantErr:       false,
		},
		{
			name:          "unknown model with suffix",
			inputModel:    "unknown-model(8192)",
			wantProviders: nil,
			wantModel:     "",
			wantErr:       true,
		},
		{
			name:          "auto suffix resolved",
			inputModel:    "auto(high)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-pro(high)",
			wantErr:       false,
		},
		{
			name:          "special suffix none preserved",
			inputModel:    "gemini-2.5-flash(none)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-flash(none)",
			wantErr:       false,
		},
		{
			name:          "bracket suffix 1M stripped for provider lookup",
			inputModel:    "deepseek-v4-flash[1M]",
			wantProviders: []string{"opencode-go"},
			wantModel:     "deepseek-v4-flash",
			wantErr:       false,
		},
		{
			name:          "bracket suffix 128K stripped",
			inputModel:    "deepseek-v4-flash[128K]",
			wantProviders: []string{"opencode-go"},
			wantModel:     "deepseek-v4-flash",
			wantErr:       false,
		},
		{
			name:          "bracket suffix unknown model",
			inputModel:    "unknown-model[1M]",
			wantProviders: nil,
			wantModel:     "",
			wantErr:       true,
		},
		{
			name:          "bracket suffix preserved round bracket thinking suffix",
			inputModel:    "deepseek-v4-flash[1M](8192)",
			wantProviders: []string{"opencode-go"},
			wantModel:     "deepseek-v4-flash(8192)",
			wantErr:       false,
		},
		{
			name:          "custom model with bracket suffix preserved",
			inputModel:    "custom-model[beta]",
			wantProviders: []string{"openai"},
			wantModel:     "custom-model[beta]",
			wantErr:       false,
		},
		{
			name:          "special suffix auto preserved",
			inputModel:    "claude-sonnet-4-5(auto)",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5(auto)",
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers, model, errMsg := handler.getRequestDetails(context.Background(), tt.inputModel)
			if (errMsg != nil) != tt.wantErr {
				t.Fatalf("getRequestDetails() error = %v, wantErr %v", errMsg, tt.wantErr)
			}
			if errMsg != nil {
				return
			}
			if !reflect.DeepEqual(providers, tt.wantProviders) {
				t.Fatalf("getRequestDetails() providers = %v, want %v", providers, tt.wantProviders)
			}
			if model != tt.wantModel {
				t.Fatalf("getRequestDetails() model = %v, want %v", model, tt.wantModel)
			}
		})
	}
}

func TestGetRequestDetails_AllowedGroupResolvesPrefixedModelProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()
	modelRegistry.RegisterClient("test-request-details-pro", "openai", []*registry.ModelInfo{
		{ID: "pro/gpt-5", Created: now},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("test-request-details-pro")
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("accessMetadata", map[string]string{"allowed-channel-groups": "pro"})
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)

	providers, model, errMsg := handler.getRequestDetails(ctx, "gpt-5")
	if errMsg != nil {
		t.Fatalf("getRequestDetails() unexpected error = %v", errMsg)
	}
	if !reflect.DeepEqual(providers, []string{"openai"}) {
		t.Fatalf("providers = %v, want [openai]", providers)
	}
	if model != "gpt-5" {
		t.Fatalf("model = %q, want gpt-5", model)
	}
}

func TestGetRequestDetails_RouteGroupRejectsConflictingModelPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set(internalrouting.GinPathRouteContextKey, &internalrouting.PathRouteContext{
		RoutePath: "/pro",
		Group:     "pro",
		Fallback:  "none",
	})
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)

	_, _, errMsg := handler.getRequestDetails(ctx, "free/gpt-5")
	if errMsg == nil {
		t.Fatal("expected model_prefix_conflict error")
	}
	if errMsg.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", errMsg.StatusCode)
	}
}

func TestGetRequestDetails_CcSwitchMappedTargetModelAllowsProviderPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()
	modelRegistry.RegisterClient("test-request-details-ccswitch-target", "deepseek", []*registry.ModelInfo{
		{ID: "cline-pass/deepseek-v4-flash", Created: now},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("test-request-details-ccswitch-target")
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set(internalrouting.GinPathRouteContextKey, &internalrouting.PathRouteContext{
		RoutePath: "/group1/cs_test",
		Group:     "group1",
		Fallback:  "none",
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ClientType: "claude",
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{
					RequestModel: "claude-sonnet-5",
					TargetModel:  "cline-pass/deepseek-v4-flash",
				},
			},
		},
	})
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)

	providers, model, errMsg := handler.getRequestDetails(ctx, "cline-pass/deepseek-v4-flash")
	if errMsg != nil {
		t.Fatalf("getRequestDetails() unexpected error = %v", errMsg)
	}
	if !reflect.DeepEqual(providers, []string{"deepseek"}) {
		t.Fatalf("providers = %v, want [deepseek]", providers)
	}
	if model != "cline-pass/deepseek-v4-flash" {
		t.Fatalf("model = %q, want cline-pass/deepseek-v4-flash", model)
	}
}

func TestRequestExecutionMetadata_UsesPathRouteContextFromRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("POST", "/openai/plus/v1/responses", nil)
	route := &internalrouting.PathRouteContext{
		RoutePath: "/openai/plus",
		Group:     "pro",
		Fallback:  "none",
	}
	ctx := internalrouting.WithPathRouteContext(context.Background(), route)
	ctx = context.WithValue(ctx, util.ContextKeyGin, ginCtx)

	meta := requestExecutionMetadata(ctx)
	if got := meta["route_group"]; got != "pro" {
		t.Fatalf("route_group = %v, want %q", got, "pro")
	}
	if got := meta["route_fallback"]; got != "none" {
		t.Fatalf("route_fallback = %v, want %q", got, "none")
	}
}

func TestRequestExecutionMetadata_CapturesSessionStickyHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ginCtx.Request.Header.Set("Session-Id", "sess-1")

	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.SessionStickyMetadataKey]; got != "header:session-id:sess-1" {
		t.Fatalf("session sticky key = %v, want header:session-id:sess-1", got)
	}
}

func TestRequestExecutionMetadata_UsesGinRequestContextPathRouteAfterHandleContextReset(t *testing.T) {
	gin.SetMode(gin.TestMode)

	route := &internalrouting.PathRouteContext{
		RoutePath: "/openai/plus",
		Group:     "pro",
		Fallback:  "none",
	}
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req = req.WithContext(internalrouting.WithPathRouteContext(req.Context(), route))

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = req

	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
	meta := requestExecutionMetadata(ctx)
	if got := meta["route_group"]; got != "pro" {
		t.Fatalf("route_group = %v, want %q", got, "pro")
	}
	if got := meta["route_fallback"]; got != "none" {
		t.Fatalf("route_fallback = %v, want %q", got, "none")
	}
}
