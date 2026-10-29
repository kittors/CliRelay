package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/modeldiscovery"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// The preset editor offers a group's allow list as its model choices. For a
// group that follows upstream it also has to offer what discovery added since,
// or the models the gate now admits could never be picked.
func TestGetChannelGroupsReportsFollowingAndTheModelsItAdds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modeldiscovery.ResetForTest()
	t.Cleanup(modeldiscovery.ResetForTest)
	modeldiscovery.Store(identity.SystemTenantID, "codex", []*sdkmodelcatalog.ModelInfo{
		{ID: "gpt-6.1-sol"}, {ID: "gpt-6.1-terra"}, {ID: "gpt-6-chat-thinking"},
	})

	on := true
	h := &Handler{cfg: &config.Config{Routing: config.RoutingConfig{
		IncludeDefaultGroup: true,
		ChannelGroups: []config.RoutingChannelGroup{
			{Name: "deepseekv4flash+chatgpt", AllowedModels: []string{"gpt-6.1-sol"}},
			{Name: "following", AllowedModels: []string{"gpt-6.1-sol"}, ExcludedModels: []string{"gpt-6-chat-*"}, FollowUpstreamModels: &on},
			{Name: "open"},
		},
	}}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/channel-groups", nil)
	h.GetChannelGroups(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var body struct {
		Items []struct {
			Name                 string   `json:"name"`
			FollowUpstreamModels bool     `json:"follow-upstream-models"`
			DiscoveredModels     []string `json:"discovered-models"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	type state struct {
		follows    bool
		discovered string
	}
	got := map[string]state{}
	for _, item := range body.Items {
		got[item.Name] = state{item.FollowUpstreamModels, strings.Join(item.DiscoveredModels, ",")}
	}
	want := map[string]state{
		// Implicit, with no allow list: follows, and has nothing to add.
		"default": {follows: true},
		// Frozen, so these are what turning the setting on would open; the panel
		// shows them before the operator decides.
		"deepseekv4flash+chatgpt": {follows: false, discovered: "gpt-6-chat-thinking,gpt-6.1-terra"},
		// gpt-6.1-sol is already on the list and gpt-6-chat-thinking is excluded.
		"following": {follows: true, discovered: "gpt-6.1-terra"},
		"open":      {follows: false},
	}
	for name, expected := range want {
		actual, ok := got[name]
		if !ok {
			t.Fatalf("group %q missing from %s", name, w.Body.String())
		}
		if actual != expected {
			t.Errorf("group %q = %+v, want %+v", name, actual, expected)
		}
	}
}

// The models a frozen group would open come from asking its channels, not its
// allow list, which refuses them by definition: asking with the list in place
// left the preview empty for exactly the groups that need it.
func TestGetChannelGroupsAsksAFrozenGroupsChannelsWhatFollowingWouldOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modeldiscovery.ResetForTest()
	t.Cleanup(modeldiscovery.ResetForTest)
	modeldiscovery.Store(identity.SystemTenantID, "codex", []*sdkmodelcatalog.ModelInfo{
		{ID: "gpt-6.1-sol"}, {ID: "gpt-6.1-terra"}, {ID: "gpt-6-chat-thinking"},
	})

	frozen := false
	cfg := &config.Config{Routing: config.RoutingConfig{
		IncludeDefaultGroup: true,
		ChannelGroups: []config.RoutingChannelGroup{
			{Name: "default", AllowedModels: []string{"gpt-6.1-sol"}, FollowUpstreamModels: &frozen},
		},
	}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	// The account serves gpt-6.1-terra but not gpt-6-chat-thinking.
	registerManagementImageAuthForTenant(t, manager, identity.SystemTenantID, "codex-follow-preview", "gpt-6.1-sol", "gpt-6.1-terra")

	h := &Handler{cfg: cfg, authManager: manager}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/channel-groups", nil)
	h.GetChannelGroups(c)

	var body struct {
		Items []struct {
			Name             string   `json:"name"`
			DiscoveredModels []string `json:"discovered-models"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, item := range body.Items {
		if item.Name != "default" {
			continue
		}
		if strings.Join(item.DiscoveredModels, ",") != "gpt-6.1-terra" {
			t.Fatalf("default offers %v, want only the served gpt-6.1-terra", item.DiscoveredModels)
		}
		return
	}
	t.Fatalf("default group missing from %s", w.Body.String())
}

// Discovery is per tenant, not per group: a following group of Claude channels
// must not offer a Codex model it would then fail to route.
func TestAttachDiscoveredModelsSkipsWhatTheGroupCannotServe(t *testing.T) {
	items := []channelGroupItem{
		{Name: "claude-team", AllowedModels: []string{"claude-opus-5"}, FollowUpstreamModels: true},
		{Name: "codex-team", AllowedModels: []string{"gpt-6.1-sol"}, FollowUpstreamModels: true},
	}
	attachDiscoveredModels(items, []string{"gpt-6.1-terra"}, func(group, model string) bool {
		return group == "codex-team"
	})
	if len(items[0].DiscoveredModels) != 0 {
		t.Fatalf("claude-team offers %v", items[0].DiscoveredModels)
	}
	if len(items[1].DiscoveredModels) != 1 || items[1].DiscoveredModels[0] != "gpt-6.1-terra" {
		t.Fatalf("codex-team offers %v, want gpt-6.1-terra", items[1].DiscoveredModels)
	}
}
