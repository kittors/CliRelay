package requestdispatch

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkrouting "github.com/router-for-me/CLIProxyAPI/v6/sdk/routing"
)

func TestResolveRequestDetails_RouteGroupRejectsConflictingModelPrefix(t *testing.T) {
	ctx := sdkrouting.WithPathRouteContext(context.Background(), &sdkrouting.PathRouteContext{
		RoutePath: "/pro",
		Group:     "pro",
		Fallback:  "none",
	})

	_, _, errMsg := ResolveRequestDetails(ctx, "free/gpt-5")
	if errMsg == nil {
		t.Fatal("expected model_prefix_conflict error")
	}
	if errMsg.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", errMsg.StatusCode)
	}
}

func TestResolveRequestDetails_CcSwitchMappedTargetModelAllowsProviderPrefix(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()
	modelRegistry.RegisterClient("test-requestdispatch-ccswitch-target", "deepseek", []*registry.ModelInfo{
		{ID: "cline-pass/deepseek-v4-flash", Created: now},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("test-requestdispatch-ccswitch-target")
	})

	ctx := sdkrouting.WithPathRouteContext(context.Background(), &sdkrouting.PathRouteContext{
		RoutePath: "/group1/cs_test",
		Group:     "group1",
		Fallback:  "none",
		CcSwitch: &sdkrouting.CcSwitchRouteContext{
			ClientType: "claude",
			ModelMappings: []sdkrouting.CcSwitchModelMapping{
				{
					RequestModel: "claude-sonnet-5",
					TargetModel:  "cline-pass/deepseek-v4-flash",
				},
			},
		},
	})

	providers, model, errMsg := ResolveRequestDetails(ctx, "cline-pass/deepseek-v4-flash")
	if errMsg != nil {
		t.Fatalf("ResolveRequestDetails() unexpected error = %v", errMsg)
	}
	if !reflect.DeepEqual(providers, []string{"deepseek"}) {
		t.Fatalf("providers = %v, want [deepseek]", providers)
	}
	if model != "cline-pass/deepseek-v4-flash" {
		t.Fatalf("model = %q, want cline-pass/deepseek-v4-flash", model)
	}
}
