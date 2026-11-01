package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

func virtualModelTestServer(models ...config.VirtualModel) *Server {
	return &Server{cfg: &config.Config{Routing: config.RoutingConfig{
		IncludeDefaultGroup: true,
		ChannelGroups: []config.RoutingChannelGroup{
			{Name: "default"},
			{Name: "team", AllowedModels: []string{"gpt-5", "o3"}},
			{Name: "ops"},
		},
		VirtualModels: models,
	}}}
}

func members(plan *handlers.VirtualModelPlan) []string {
	out := make([]string, 0, len(plan.Members))
	for _, member := range plan.Members {
		out = append(out, member.Route()+"="+member.Skip)
	}
	return out
}

func TestPlanVirtualModelSaysWhichMembersARequestMayUse(t *testing.T) {
	server := virtualModelTestServer(config.VirtualModel{Name: "smart", Strategy: config.VirtualModelStrategyWeighted, Members: []config.VirtualModelMember{
		{Group: "team", Model: "gpt-5", Weight: 2},
		{Group: "team", Model: "gpt-4o", Weight: 1},
		{Model: "claude-sonnet-4-5"},
		{Model: "o4-mini(low)"},
	}})
	routing := server.routingConfigForTenant("")

	plan := server.planVirtualModel(virtualModelScope{}, routing, "smart")
	if plan == nil || plan.Name != "smart" || !plan.Weighted {
		t.Fatalf("plan = %+v, want the weighted virtual model", plan)
	}
	// team serves only the models it lists.
	want := "team/gpt-5=,team/gpt-4o=" + routetrace.MemberNotAllowed + ",claude-sonnet-4-5=,o4-mini(low)="
	if got := strings.Join(members(plan), ","); got != want {
		t.Fatalf("members = %s, want %s", got, want)
	}
	if detail := plan.Members[1].Detail; !strings.Contains(detail, `"gpt-4o"`) || !strings.Contains(detail, `"team"`) {
		t.Fatalf("refusal = %q, want the group's refusal of the model", detail)
	}
	if plan.Members[0].Weight != 2 {
		t.Fatalf("weight = %d, want the member's own", plan.Members[0].Weight)
	}

	// A thinking suffix on the name goes to the members that set none.
	plan = server.planVirtualModel(virtualModelScope{}, routing, "smart(high)")
	var models []string
	for _, member := range plan.Members {
		models = append(models, member.Model)
	}
	if got := strings.Join(models, ","); got != "gpt-5(high),gpt-4o(high),claude-sonnet-4-5(high),o4-mini(low)" {
		t.Fatalf("models = %s, want the suffix on the members without one", got)
	}

	for _, name := range []string{"gpt-5", "SMART", "", "team/smart"} {
		if plan := server.planVirtualModel(virtualModelScope{}, routing, name); plan != nil {
			t.Fatalf("plan(%q) = %+v, want none", name, plan)
		}
	}

	// On another group's path, a member of team is not reachable; a member
	// without a group runs on the path the request came in on.
	plan = server.planVirtualModel(virtualModelScope{routeGroup: "ops"}, routing, "smart")
	want = "team/gpt-5=" + routetrace.MemberNotAllowed + ",team/gpt-4o=" + routetrace.MemberNotAllowed + ",claude-sonnet-4-5=,o4-mini(low)="
	if got := strings.Join(members(plan), ","); got != want {
		t.Fatalf("members on ops = %s, want %s", got, want)
	}
	if detail := plan.Members[0].Detail; !strings.Contains(detail, `routed to channel group "ops"`) {
		t.Fatalf("refusal = %q, want the route named", detail)
	}

	// A key limited to some channel groups reaches the members in them only.
	plan = server.planVirtualModel(virtualModelScope{allowedGroups: map[string]struct{}{"default": {}}}, routing, "smart")
	if got := plan.Members[0]; got.Skip != routetrace.MemberNotAllowed || !strings.Contains(got.Detail, `may not use channel group "team"`) {
		t.Fatalf("member = %+v, want the key's refusal of team", got)
	}
}

func TestModelGateRefusesAVirtualModelOnlyWhenNoMemberIsUsable(t *testing.T) {
	server := virtualModelTestServer(
		config.VirtualModel{Name: "smart", Members: []config.VirtualModelMember{{Group: "team", Model: "gpt-5"}, {Model: "claude-sonnet-4-5"}}},
		config.VirtualModel{Name: "teamonly", Members: []config.VirtualModelMember{{Group: "team", Model: "gpt-5"}}},
	)

	defaultOnly := map[string]string{"allowed-channel-groups": "default"}
	if status, code, message := gateVerdict(t, server, defaultOnly, nil, "smart"); status != 0 {
		t.Fatalf("smart refused with %d %s: %s, but claude-sonnet-4-5 is usable", status, code, message)
	}
	status, code, message := gateVerdict(t, server, defaultOnly, nil, "teamonly")
	if status != http.StatusForbidden || code != "virtual_model_unavailable" {
		t.Fatalf("verdict = %d %s, want 403 virtual_model_unavailable", status, code)
	}
	for _, want := range []string{"'teamonly'", "team/gpt-5", `may not use channel group "team"`} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q does not say %s", message, want)
		}
	}

	// The key's allowed-models name who may ask for the virtual model; they do
	// not have to list its members, which the key still cannot ask for itself.
	onlySmart := map[string]string{"allowed-models": "smart"}
	if status, code, message := gateVerdict(t, server, onlySmart, nil, "smart"); status != 0 {
		t.Fatalf("smart refused with %d %s: %s", status, code, message)
	}
	for _, model := range []string{"gpt-5", "teamonly"} {
		if status, code, _ := gateVerdict(t, server, onlySmart, nil, model); status != http.StatusForbidden || code != "model_not_allowed" {
			t.Fatalf("%s: verdict = %d %s, want 403 model_not_allowed", model, status, code)
		}
	}

	// On a group's path, the members of other groups do not count.
	if status, code, _ := gateVerdict(t, server, nil, &internalrouting.PathRouteContext{Group: "ops"}, "teamonly"); status != http.StatusForbidden || code != "virtual_model_unavailable" {
		t.Fatalf("teamonly on ops: verdict = %d %s, want 403 virtual_model_unavailable", status, code)
	}
}

// The middleware hands the handlers a planner when the tenant has virtual
// models, also when nothing else about the request is restricted.
func TestModelRestrictionMiddlewarePublishesTheVirtualModelPlanner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	run := func(server *Server) handlers.VirtualModelPlanner {
		t.Helper()
		var planner handlers.VirtualModelPlanner
		engine := gin.New()
		engine.POST("/v1/chat/completions", server.modelRestrictionMiddleware(), func(c *gin.Context) {
			if value, ok := c.Get(handlers.VirtualModelPlannerContextKey); ok {
				planner, _ = value.(handlers.VirtualModelPlanner)
			}
			c.Status(http.StatusNoContent)
		})
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"smart"}`)))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		return planner
	}

	server := &Server{cfg: &config.Config{Routing: config.RoutingConfig{VirtualModels: []config.VirtualModel{
		{Name: "smart", Members: []config.VirtualModelMember{{Model: "a"}, {Model: "b"}}},
	}}}}
	planner := run(server)
	if planner == nil {
		t.Fatal("no planner for a tenant with virtual models")
	}
	if plan := planner("smart"); plan == nil || len(plan.Members) != 2 {
		t.Fatalf("plan = %+v, want smart's two members", plan)
	}
	if plan := planner("a"); plan != nil {
		t.Fatalf("plan(a) = %+v, want none for a real model", plan)
	}
	if planner := run(&Server{cfg: &config.Config{}}); planner != nil {
		t.Fatal("a planner for a tenant without virtual models")
	}
}

func TestModelsListingShowsTheVirtualModelsACallerCanUse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const authID = "vm-listing-auth"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{
		{ID: "vm-real-a", Object: "model", OwnedBy: "openai"},
		{ID: "vm-real-b", Object: "model", OwnedBy: "openai"},
	})
	authManager := coreauth.NewManager(nil, nil, nil)
	if _, err := authManager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register: %v", err)
	}

	cfg := &config.Config{Routing: config.RoutingConfig{VirtualModels: []config.VirtualModel{
		{Name: "smart", Description: "best of both", Members: []config.VirtualModelMember{{Model: "vm-missing"}, {Model: "vm-real-a"}}},
		{Name: "ghost", Members: []config.VirtualModelMember{{Model: "vm-missing"}}},
		// Named like a real model: it adds fallbacks to it, and the model is
		// listed once.
		{Name: "vm-real-b", Members: []config.VirtualModelMember{{Model: "vm-real-b"}, {Model: "vm-real-a"}}},
	}}}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager)
	server := &Server{handlers: base, cfg: cfg}
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		if allowed := c.Query("allowed"); allowed != "" {
			c.Set("accessMetadata", map[string]string{"allowed-models": allowed})
		}
		server.unifiedModelsHandler(openai.NewOpenAIAPIHandler(base), claude.NewClaudeCodeAPIHandler(base))(c)
	})
	list := func(target, userAgent string) map[string]map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
		}
		var response struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode: %v", err)
		}
		byID := make(map[string]map[string]any, len(response.Data))
		for _, entry := range response.Data {
			id, _ := entry["id"].(string)
			if _, dup := byID[id]; dup {
				t.Fatalf("%s listed twice", id)
			}
			byID[id] = entry
		}
		return byID
	}

	listed := list("/v1/models", "")
	smart, ok := listed["smart"]
	if !ok || smart["owned_by"] != virtualModelOwner || smart["description"] != "best of both" {
		t.Fatalf("smart = %+v, want the virtual model with its description", smart)
	}
	if _, ok := listed["ghost"]; ok {
		t.Fatal("ghost is listed, but no account serves any of its members")
	}
	if entry := listed["vm-real-b"]; entry == nil || entry["owned_by"] == virtualModelOwner {
		t.Fatalf("vm-real-b = %+v, want the real model's entry", entry)
	}

	if entry := list("/v1/models", "claude-cli/2.1.0")["smart"]; entry == nil || entry["display_name"] != "smart" || entry["type"] != "model" {
		t.Fatalf("claude listing of smart = %+v", entry)
	}
	restricted := list("/v1/models?allowed=vm-real-a", "")
	if _, ok := restricted["smart"]; ok {
		t.Fatal("smart is listed for a key whose allowed-models do not name it")
	}
	if _, ok := list("/v1/models?allowed=smart", "")["smart"]; !ok {
		t.Fatal("smart is not listed for a key whose allowed-models name it")
	}
	if _, ok := list("/v1/models?ignore_group_allowed_models=1", "")["smart"]; ok {
		t.Fatal("the channel-group editor's listing must only offer real models")
	}
}
