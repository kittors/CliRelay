package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/modeldiscovery"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// The 2026-10-28 report: Codex, imported through the CC Switch preset
// /deepseekv4flash-chatgpt/cs_3e13ca9880fc, got "model 'gpt-6.1-terra' is not
// allowed for this API key" while the root path served the model. Two frozen
// lists refused it: the preset's own mappings, read as an allow list, and the
// allow list of the group the preset routes to. Neither was the key's.

func followUpstreamTestServer(curatedFollows *bool) *Server {
	frozen := false
	return &Server{cfg: &config.Config{Routing: config.RoutingConfig{
		IncludeDefaultGroup: true,
		ChannelGroups: []config.RoutingChannelGroup{
			{Name: "default", AllowedModels: []string{"gpt-5.5"}},
			{Name: "frozen-root", AllowedModels: []string{"gpt-5.5"}, FollowUpstreamModels: &frozen},
			{Name: "deepseekv4flash+chatgpt", AllowedModels: []string{"gpt-6.1-sol"}, FollowUpstreamModels: curatedFollows},
		},
	}}}
}

func storeCodexDiscovery(t *testing.T, ids ...string) {
	t.Helper()
	modeldiscovery.ResetForTest()
	t.Cleanup(modeldiscovery.ResetForTest)
	models := make([]*sdkmodelcatalog.ModelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, &sdkmodelcatalog.ModelInfo{ID: id})
	}
	modeldiscovery.Store("", "codex", models)
}

func presetRoute(group string) *internalrouting.PathRouteContext {
	return &internalrouting.PathRouteContext{
		RoutePath: "/deepseekv4flash-chatgpt/cs_3e13ca9880fc",
		Group:     group,
		Fallback:  "none",
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ClientType:   "codex",
			DefaultModel: "gpt-6.1-sol",
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{RequestModel: "gpt-6.1-sol", TargetModel: "gpt-6.1-sol"},
				{RequestModel: "fast", TargetModel: "gpt-6.1-sol"},
				{RequestModel: "slow", TargetModel: "gpt-5.4"},
			},
		},
	}
}

// gateVerdict runs model through the gate the model-restriction middleware
// builds for a request with the given key metadata and route.
func gateVerdict(t *testing.T, server *Server, metadata map[string]string, route *internalrouting.PathRouteContext, model string) (int, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if metadata != nil {
		c.Set("accessMetadata", metadata)
	}
	if route != nil {
		attachPathRouteContext(c, route)
	}
	gate := server.buildModelGate(c)
	if gate == nil {
		return 0, "", ""
	}
	verdict := gate(model)
	if verdict == nil {
		return 0, "", ""
	}
	body := modelGateErrorBody(verdict)["error"].(map[string]interface{})
	return verdict.StatusCode, body["code"].(string), body["message"].(string)
}

func TestPresetRouteServesAModelTheUpstreamAddedOnceItsGroupFollows(t *testing.T) {
	storeCodexDiscovery(t, "gpt-6.1-sol", "gpt-6.1-terra")
	on := true
	server := followUpstreamTestServer(&on)

	// Not among the preset's mappings: those only rename, so the group decides.
	if status, code, message := gateVerdict(t, server, nil, presetRoute("deepseekv4flash+chatgpt"), "gpt-6.1-terra"); status != 0 {
		t.Fatalf("gpt-6.1-terra refused with %d %s: %s", status, code, message)
	}
	// A mapped request name is checked as the model it is rewritten to.
	if status, code, message := gateVerdict(t, server, nil, presetRoute("deepseekv4flash+chatgpt"), "fast"); status != 0 {
		t.Fatalf("mapped name refused with %d %s: %s", status, code, message)
	}
}

func TestPresetRouteRefusalNamesTheGroupNotTheKey(t *testing.T) {
	storeCodexDiscovery(t, "gpt-6.1-sol", "gpt-6.1-terra")
	server := followUpstreamTestServer(nil)

	status, code, message := gateVerdict(t, server, nil, presetRoute("deepseekv4flash+chatgpt"), "gpt-6.1-terra")
	if status != http.StatusForbidden || code != "model_not_allowed_by_channel_group" {
		t.Fatalf("verdict = %d %s, want 403 model_not_allowed_by_channel_group", status, code)
	}
	for _, want := range []string{`"gpt-6.1-terra"`, `"deepseekv4flash+chatgpt"`, "follow-upstream-models"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q does not mention %s", message, want)
		}
	}
	if strings.Contains(message, "API key") {
		t.Fatalf("message %q blames the key, which has no model list", message)
	}

	// The preset sends "slow" to gpt-5.4, which the group does not serve and
	// discovery never listed: name both spellings, and do not offer following
	// upstream as a fix it is not.
	status, code, message = gateVerdict(t, server, nil, presetRoute("deepseekv4flash+chatgpt"), "slow")
	if status != http.StatusForbidden || code != "model_not_allowed_by_channel_group" {
		t.Fatalf("verdict = %d %s, want 403 model_not_allowed_by_channel_group", status, code)
	}
	if !strings.Contains(message, `"gpt-5.4"`) || !strings.Contains(message, `(requested as "slow")`) {
		t.Fatalf("message %q does not name the mapped model and the requested name", message)
	}
	if strings.Contains(message, "follow-upstream-models") {
		t.Fatalf("message %q suggests following upstream for a model discovery never listed", message)
	}
}

func TestKeyAllowListRefusalKeepsItsOwnWording(t *testing.T) {
	storeCodexDiscovery(t, "gpt-6.1-sol")
	on := true
	server := followUpstreamTestServer(&on)

	status, code, message := gateVerdict(t, server, map[string]string{"allowed-models": "gpt-5.5"}, presetRoute("deepseekv4flash+chatgpt"), "gpt-6.1-sol")
	if status != http.StatusForbidden || code != "model_not_allowed" || !strings.Contains(message, "not allowed for this API key") {
		t.Fatalf("verdict = %d %s %q, want the key's own refusal", status, code, message)
	}
}

func TestRootGroupFollowsUnlessSwitchedOff(t *testing.T) {
	storeCodexDiscovery(t, "gpt-6.1-terra")
	server := followUpstreamTestServer(nil)

	if status, code, message := gateVerdict(t, server, nil, nil, "gpt-6.1-terra"); status != 0 {
		t.Fatalf("root path refused a discovered model with %d %s: %s", status, code, message)
	}
	route := &internalrouting.PathRouteContext{RoutePath: "/frozen", Group: "frozen-root", Fallback: "none"}
	if status, code, _ := gateVerdict(t, server, nil, route, "gpt-6.1-terra"); status != http.StatusForbidden || code != "model_not_allowed_by_channel_group" {
		t.Fatalf("a group switched off still followed upstream: %d %s", status, code)
	}
}
