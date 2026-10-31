package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const routingSimulationTestSecret = "sk-routing-simulation-plaintext"

func newRoutingSimulationTestDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	for tenantID, row := range map[string]usage.APIKeyRow{
		identity.SystemTenantID: {ID: "key-1", Key: routingSimulationTestSecret, Name: "Team key", Disabled: true},
		"tenant-b":              {ID: "key-b", Key: "sk-other-tenant", Name: "Other"},
	} {
		if err := usage.UpsertAPIKeyForTenant(tenantID, row); err != nil {
			t.Fatalf("upsert key %s: %v", row.ID, err)
		}
	}
}

func callRoutingSimulation(h *Handler, principal identity.Principal, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, principal)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	h.GetRoutingSimulation(c)
	return rec
}

func tenantPrincipal(tenantID string, permissions ...string) identity.Principal {
	granted := make(map[string]bool, len(permissions))
	for _, permission := range permissions {
		granted[permission] = true
	}
	return identity.Principal{Permissions: granted, EffectiveTenant: identity.Tenant{ID: tenantID}}
}

func TestGetRoutingSimulationRunsTheSimulatorForATenantKey(t *testing.T) {
	newRoutingSimulationTestDB(t)
	until := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)
	var got RoutingSimulationRequest
	h := &Handler{}
	h.SetRoutingSimulator(func(_ context.Context, req RoutingSimulationRequest) (*RoutingSimulationResult, error) {
		got = req
		return &RoutingSimulationResult{
			Stages:      []RoutingSimulationStage{{Name: "auth", Outcome: RoutingStagePassed}, {Name: "quota", Outcome: RoutingStageSkipped}},
			Route:       &diagnostics.RouteSnapshot{RoutePath: "/team", Group: "team", Fallback: "default"},
			ModelPolicy: &diagnostics.ModelPolicySnapshot{RequestedModel: "fast", FinalModel: "gpt-5", RouteID: "r1"},
			Model:       "gpt-5",
			Selection: &coreauth.SelectionSimulation{
				Model: "gpt-5", ModelKey: "gpt-5", Providers: []string{"codex"},
				Groups: []coreauth.RouteGroupSimulation{{Name: "team", Distribution: "weighted", Credentials: []coreauth.CredentialSimulation{
					{AuthID: "codex-a", Label: "A", Provider: "codex", Weight: 1, Attempt: 1},
					{AuthID: "codex-b", Label: "B", Provider: "codex", Weight: 1, Blocked: coreauth.SimulatedBlockCooldown, BlockedUntil: until, BlockCause: coreauth.CooldownReasonRateLimited, BlockHTTPStatus: 429},
				}}},
				Attempts:      []coreauth.SimulatedAttempt{{AuthID: "codex-a", Group: "team", UpstreamModel: "gpt-5"}},
				RetryWait:     1500 * time.Millisecond,
				RetryWaitKind: coreauth.SimulatedRetryWaitBusyWait,
			},
		}, nil
	})

	rec := callRoutingSimulation(h, tenantPrincipal(identity.SystemTenantID, "routing.read", "api_keys.read", "auth_files.read"),
		"/v0/management/routing-simulation?api-key-id=key-1&model=fast&route-path=/team&endpoint=messages")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	want := RoutingSimulationRequest{APIKey: routingSimulationTestSecret, Model: "fast", RoutePath: "/team", Endpoint: RoutingSimulationEndpointMessages}
	if got != want {
		t.Fatalf("simulator got %+v, want %+v", got, want)
	}
	body := rec.Body.String()
	if strings.Contains(body, routingSimulationTestSecret) {
		t.Fatalf("response carries the key's plaintext: %s", body)
	}
	var resp struct {
		APIKey struct {
			ID, Name string
			Disabled bool
		} `json:"api-key"`
		RequestedModel string `json:"requested-model"`
		Stages         []struct{ Name, Outcome string }
		Route          struct{ Group string }
		ModelPolicy    struct {
			FinalModel string `json:"final-model"`
		} `json:"model-policy"`
		Selection struct {
			Groups []struct {
				Credentials []struct {
					AuthID       string `json:"auth-id"`
					Blocked      string
					BlockedUntil string `json:"blocked-until"`
					BlockCause   string `json:"block-cause"`
					Attempt      int
				}
			}
			Attempts []struct {
				AuthID string `json:"auth-id"`
			}
			RetryWaitMS   int64  `json:"retry-wait-ms"`
			RetryWaitKind string `json:"retry-wait-kind"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.APIKey.ID != "key-1" || resp.APIKey.Name != "Team key" || !resp.APIKey.Disabled || resp.RequestedModel != "fast" {
		t.Fatalf("key / model = %+v %q", resp.APIKey, resp.RequestedModel)
	}
	if len(resp.Stages) != 2 || resp.Stages[1].Outcome != RoutingStageSkipped || resp.Route.Group != "team" || resp.ModelPolicy.FinalModel != "gpt-5" {
		t.Fatalf("stages %+v, route %+v, policy %+v", resp.Stages, resp.Route, resp.ModelPolicy)
	}
	cooling := resp.Selection.Groups[0].Credentials[1]
	if cooling.Blocked != "cooldown" || cooling.BlockedUntil != "2026-11-01T08:30:00Z" || cooling.BlockCause != coreauth.CooldownReasonRateLimited || cooling.Attempt != 0 {
		t.Fatalf("cooling credential = %+v", cooling)
	}
	if len(resp.Selection.Attempts) != 1 || resp.Selection.Attempts[0].AuthID != "codex-a" || resp.Selection.RetryWaitMS != 1500 || resp.Selection.RetryWaitKind != "busy-wait" {
		t.Fatalf("attempts %+v, retry wait %d (%q)", resp.Selection.Attempts, resp.Selection.RetryWaitMS, resp.Selection.RetryWaitKind)
	}
}

func TestGetRoutingSimulationRefusals(t *testing.T) {
	newRoutingSimulationTestDB(t)
	called := false
	h := &Handler{}
	h.SetRoutingSimulator(func(context.Context, RoutingSimulationRequest) (*RoutingSimulationResult, error) {
		called = true
		return &RoutingSimulationResult{}, nil
	})
	all := tenantPrincipal(identity.SystemTenantID, "routing.read", "api_keys.read", "auth_files.read")
	cases := []struct {
		name      string
		h         *Handler
		principal identity.Principal
		target    string
		status    int
		code      string
	}{
		{"without api_keys.read", h, tenantPrincipal(identity.SystemTenantID, "routing.read", "auth_files.read"),
			"?api-key-id=key-1&model=m", http.StatusForbidden, "permission_denied"},
		{"without auth_files.read", h, tenantPrincipal(identity.SystemTenantID, "routing.read", "api_keys.read"),
			"?api-key-id=key-1&model=m", http.StatusForbidden, "permission_denied"},
		{"no simulator", &Handler{}, all, "?api-key-id=key-1&model=m", http.StatusServiceUnavailable, "routing_simulation_unavailable"},
		{"no model", h, all, "?api-key-id=key-1", http.StatusBadRequest, "invalid_request"},
		{"no key", h, all, "?model=m", http.StatusBadRequest, "invalid_request"},
		{"unknown endpoint", h, all, "?api-key-id=key-1&model=m&endpoint=responses", http.StatusBadRequest, "invalid_request"},
		{"unknown key", h, all, "?api-key-id=key-404&model=m", http.StatusNotFound, "api_key_not_found"},
		{"another tenant's key", h, all, "?api-key-id=key-b&model=m", http.StatusNotFound, "api_key_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			rec := callRoutingSimulation(tc.h, tc.principal, "/v0/management/routing-simulation"+tc.target)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.status, rec.Body.String())
			}
			var body struct {
				Error struct{ Code string }
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q; body %s", body.Error.Code, tc.code, rec.Body.String())
			}
			if called {
				t.Fatal("the simulator ran for a refused request")
			}
		})
	}

	t.Run("the key's own tenant", func(t *testing.T) {
		rec := callRoutingSimulation(h, tenantPrincipal("tenant-b", "routing.read", "api_keys.read", "auth_files.read"),
			"/v0/management/routing-simulation?api-key-id=key-b&model=m")
		if rec.Code != http.StatusOK || !called {
			t.Fatalf("status = %d, simulator called %v; body %s", rec.Code, called, rec.Body.String())
		}
	})

	t.Run("simulator failure", func(t *testing.T) {
		failing := &Handler{}
		failing.SetRoutingSimulator(func(context.Context, RoutingSimulationRequest) (*RoutingSimulationResult, error) {
			return nil, errors.New("boom " + routingSimulationTestSecret)
		})
		rec := callRoutingSimulation(failing, all, "/v0/management/routing-simulation?api-key-id=key-1&model=m")
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), routingSimulationTestSecret) {
			t.Fatalf("status = %d, body %s: want a 500 that does not echo the error", rec.Code, rec.Body.String())
		}
	})
}

func TestGetRoutingConfigAdvertisesTheSimulator(t *testing.T) {
	newRoutingSimulationTestDB(t)
	for _, wired := range []bool{false, true} {
		h := &Handler{cfg: &config.Config{}}
		if wired {
			h.SetRoutingSimulator(func(context.Context, RoutingSimulationRequest) (*RoutingSimulationResult, error) { return nil, nil })
		}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set(managementPrincipalKey, tenantPrincipal(identity.SystemTenantID, "routing.read"))
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing-config", nil)
		h.GetRoutingConfig(c)
		var body struct {
			Capabilities map[string]bool
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		if got := body.Capabilities["routing-simulation"]; got != wired {
			t.Fatalf("routing-simulation capability = %v with the simulator wired = %v", got, wired)
		}
	}
}
