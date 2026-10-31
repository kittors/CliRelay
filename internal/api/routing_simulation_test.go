package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const routingSimulationTestModel = "rsim-model"

// newRoutingSimulationTestServer serves routingSimulationTestModel from two
// credentials, one of them cooling down, behind the given keys and routing.
func newRoutingSimulationTestServer(t *testing.T, keys []proxyconfig.APIKeyEntry, routing proxyconfig.RoutingConfig) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	routing.IncludeDefaultGroup = true
	cfg := &proxyconfig.Config{
		SDKConfig: proxyconfig.SDKConfig{APIKeyEntries: keys},
		AuthDir:   t.TempDir(),
		Routing:   routing,
	}
	authManager := auth.NewManager(nil, nil, nil)
	authManager.RegisterExecutor(&staticResponseExecutor{})
	until := time.Now().Add(5 * time.Minute)
	for _, credential := range []*auth.Auth{
		{ID: "rsim-ready", Label: "Ready", Provider: "test-provider", Status: auth.StatusActive},
		{ID: "rsim-cooling", Label: "Cooling", Provider: "test-provider", Status: auth.StatusActive,
			ModelStates: map[string]*auth.ModelState{routingSimulationTestModel: {
				Status: auth.StatusError, Unavailable: true, NextRetryAfter: until,
				LastError: &auth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
				Quota:     auth.QuotaState{Exceeded: true, NextRecoverAt: until},
			}}},
	} {
		if _, err := authManager.Register(context.Background(), credential); err != nil {
			t.Fatalf("register %s: %v", credential.ID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: routingSimulationTestModel}})
		id := credential.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return NewServer(cfg, authManager, sdkaccess.NewManager(), filepath.Join(t.TempDir(), "config.yaml"), WithRequestLoggerFactory(nil))
}

func simulate(t *testing.T, server *Server, req managementHandlers.RoutingSimulationRequest) *managementHandlers.RoutingSimulationResult {
	t.Helper()
	if req.Endpoint == "" {
		req.Endpoint = managementHandlers.RoutingSimulationEndpointChat
	}
	result, err := server.simulateRouting(context.Background(), req)
	if err != nil {
		t.Fatalf("simulateRouting(%+v) error = %v", req, err)
	}
	return result
}

func stageOutcomes(result *managementHandlers.RoutingSimulationResult) []string {
	out := make([]string, 0, len(result.Stages))
	for _, stage := range result.Stages {
		out = append(out, stage.Name+"="+stage.Outcome)
	}
	return out
}

func TestRoutingSimulationRunsTheInferenceStages(t *testing.T) {
	server := newRoutingSimulationTestServer(t, []proxyconfig.APIKeyEntry{{Key: "rsim-key"}}, proxyconfig.RoutingConfig{})
	for _, endpoint := range []string{managementHandlers.RoutingSimulationEndpointChat, managementHandlers.RoutingSimulationEndpointMessages} {
		t.Run(endpoint, func(t *testing.T) {
			result := simulate(t, server, managementHandlers.RoutingSimulationRequest{APIKey: "rsim-key", Model: routingSimulationTestModel, Endpoint: endpoint})
			want := []string{"auth=passed", "route=passed", "channel-group=passed", "quota=skipped", "model-policy=passed",
				"model-gate=passed", "ccswitch-mapping=passed", "system-prompt=passed", "handler=passed"}
			if got := stageOutcomes(result); !slices.Equal(got, want) {
				t.Fatalf("stages = %v, want %v", got, want)
			}
			if result.Rejection != nil || result.Model != routingSimulationTestModel || result.Selection == nil {
				t.Fatalf("result = %+v, want a selection for %s and no rejection", result, routingSimulationTestModel)
			}
			attempts := result.Selection.Attempts
			if len(attempts) != 1 || attempts[0].AuthID != "rsim-ready" || attempts[0].UpstreamModel != routingSimulationTestModel {
				t.Fatalf("attempts = %+v, want only rsim-ready", attempts)
			}
			var cooling *auth.CredentialSimulation
			for i := range result.Selection.Groups[0].Credentials {
				if result.Selection.Groups[0].Credentials[i].AuthID == "rsim-cooling" {
					cooling = &result.Selection.Groups[0].Credentials[i]
				}
			}
			if cooling == nil || cooling.Blocked != auth.SimulatedBlockCooldown || cooling.BlockedUntil.IsZero() {
				t.Fatalf("cooling credential = %+v, want it listed as cooling down with an end time", cooling)
			}
		})
	}
}

func TestRoutingSimulationStopsAtTheRejectingStage(t *testing.T) {
	server := newRoutingSimulationTestServer(t,
		[]proxyconfig.APIKeyEntry{
			{Key: "rsim-key"},
			{Key: "rsim-other-groups", AllowedChannelGroups: []string{"other"}},
			{Key: "rsim-other-models", AllowedModels: []string{"other-model"}},
		},
		proxyconfig.RoutingConfig{
			ChannelGroups: []proxyconfig.RoutingChannelGroup{
				{Name: "team", Match: proxyconfig.ChannelGroupMatch{Channels: []string{"Ready", "Cooling"}}},
				{Name: "other", Match: proxyconfig.ChannelGroupMatch{Channels: []string{"Ready"}}},
			},
			PathRoutes: []proxyconfig.RoutingPathRoute{{Path: "/team", Group: "team"}},
		})
	cases := []struct {
		name   string
		req    managementHandlers.RoutingSimulationRequest
		stage  string
		status int
		code   string
	}{
		{"unknown key", managementHandlers.RoutingSimulationRequest{APIKey: "rsim-missing", Model: routingSimulationTestModel},
			"auth", http.StatusUnauthorized, "authentication_failed"},
		{"unknown route path", managementHandlers.RoutingSimulationRequest{APIKey: "rsim-key", Model: routingSimulationTestModel, RoutePath: "/missing"},
			"route", http.StatusNotFound, "route_group_unavailable"},
		{"group the key may not use", managementHandlers.RoutingSimulationRequest{APIKey: "rsim-other-groups", Model: routingSimulationTestModel, RoutePath: "/team"},
			"channel-group", http.StatusForbidden, "channel_group_forbidden"},
		{"model the key may not use", managementHandlers.RoutingSimulationRequest{APIKey: "rsim-other-models", Model: routingSimulationTestModel},
			"model-gate", http.StatusForbidden, "model_not_allowed"},
		{"model nothing serves", managementHandlers.RoutingSimulationRequest{APIKey: "rsim-key", Model: "rsim-unknown-model"},
			"handler", http.StatusBadGateway, "internal_server_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := simulate(t, server, tc.req)
			reached := true
			for _, stage := range result.Stages {
				want := managementHandlers.RoutingStagePassed
				switch {
				case stage.Name == stageQuota:
					want = managementHandlers.RoutingStageSkipped
				case stage.Name == tc.stage:
					want, reached = managementHandlers.RoutingStageRejected, false
				case !reached:
					want = managementHandlers.RoutingStageNotRun
				}
				if stage.Outcome != want {
					t.Fatalf("stages = %v, want %s rejected and the ones after it not run", stageOutcomes(result), tc.stage)
				}
			}
			if result.Rejection == nil || result.Rejection.Status != tc.status || result.Rejection.ErrorCode != tc.code || result.Rejection.ErrorMessage == "" {
				t.Fatalf("rejection = %+v, want status %d code %q with a message", result.Rejection, tc.status, tc.code)
			}
			if result.Selection != nil {
				t.Fatalf("selection = %+v, want none after a rejection", result.Selection)
			}
		})
	}

	t.Run("route path selects its group", func(t *testing.T) {
		result := simulate(t, server, managementHandlers.RoutingSimulationRequest{APIKey: "rsim-key", Model: routingSimulationTestModel, RoutePath: "team"})
		if result.Route == nil || result.Route.Group != "team" || result.Route.RoutePath != "/team" {
			t.Fatalf("route = %+v, want /team routed to the team group", result.Route)
		}
		if result.Selection == nil || len(result.Selection.Groups) == 0 || result.Selection.Groups[0].Name != "team" {
			t.Fatalf("selection = %+v, want the team group tried first", result.Selection)
		}
	})
}

func TestRoutingSimulationAppliesTheModelPolicy(t *testing.T) {
	server := newRoutingSimulationTestServer(t, []proxyconfig.APIKeyEntry{{Key: "rsim-policy-key"}}, proxyconfig.RoutingConfig{})
	// Published after the server built its key table, which publishes too.
	accesspolicy.Publish(map[string]*accesspolicy.Policy{"rsim-policy-key": {
		Routes: []quota.ModelRoute{{ID: "alias-route", From: []string{"rsim-alias"}, To: routingSimulationTestModel}},
	}})
	t.Cleanup(func() { accesspolicy.Publish(nil) })

	result := simulate(t, server, managementHandlers.RoutingSimulationRequest{APIKey: "rsim-policy-key", Model: "rsim-alias"})
	if result.ModelPolicy == nil || result.ModelPolicy.RequestedModel != "rsim-alias" || result.ModelPolicy.FinalModel != routingSimulationTestModel || result.ModelPolicy.RouteID != "alias-route" {
		t.Fatalf("model policy = %+v, want rsim-alias routed to %s by alias-route", result.ModelPolicy, routingSimulationTestModel)
	}
	if result.Model != routingSimulationTestModel || result.Selection == nil || len(result.Selection.Attempts) != 1 {
		t.Fatalf("result = %+v, want credentials selected for %s", result, routingSimulationTestModel)
	}
}

// Quota admission is the one stage the simulation skips: a simulated request
// must not count against the key's rate limit.
func TestRoutingSimulationDoesNotCountAgainstTheKey(t *testing.T) {
	const key = "rsim-rpm-key"
	server := newRoutingSimulationTestServer(t, []proxyconfig.APIKeyEntry{{Key: key, RPMLimit: 5}}, proxyconfig.RoutingConfig{})
	for range 3 {
		simulate(t, server, managementHandlers.RoutingSimulationRequest{APIKey: key, Model: routingSimulationTestModel})
	}
	if got := keyRPM(key); got != 0 {
		t.Fatalf("RPM after simulations = %d, want 0", got)
	}

	// The same request sent for real is counted.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+routingSimulationTestModel+`","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	server.engine.ServeHTTP(httptest.NewRecorder(), req)
	if got := keyRPM(key); got != 1 {
		t.Fatalf("RPM after one real request = %d, want 1", got)
	}
}

func keyRPM(key string) int {
	snapshots, _ := middleware.GetConcurrencySnapshot()
	for _, snapshot := range snapshots {
		if snapshot.APIKey == key {
			return snapshot.RPM
		}
	}
	return 0
}
