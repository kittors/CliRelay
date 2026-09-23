package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// The panel updates on its own schedule (it follows the latest codeProxy
// release), so it has to learn from the backend whether excluded-models are
// enforced before writing them: a backend that drops the field would be left
// with an emptied allow list and serve every model.
func TestGetRoutingConfigAdvertisesGroupExclusions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{cfg: &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "team", ExcludedModels: []string{"grok-imagine-*"}},
			},
		},
	}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing-config", nil)

	h.GetRoutingConfig(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var body struct {
		ChannelGroups []struct {
			Name           string   `json:"name"`
			ExcludedModels []string `json:"excluded-models"`
		} `json:"channel-groups"`
		Capabilities struct {
			ChannelGroupExcludedModels bool `json:"channel-group-excluded-models"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Capabilities.ChannelGroupExcludedModels {
		t.Fatalf("capabilities not advertised: %s", w.Body.String())
	}
	// The config itself must stay at the top level, where every existing
	// client reads it.
	if len(body.ChannelGroups) != 1 || len(body.ChannelGroups[0].ExcludedModels) != 1 {
		t.Fatalf("channel groups = %+v, want the stored group at the top level", body.ChannelGroups)
	}
}
