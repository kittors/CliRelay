package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// The panel follows the latest codeProxy release, so it only shows the
// first-token timeout and the busy-wait cap when the backend says it stores
// them; an older backend would drop them on save.
func TestGetRoutingConfigAdvertisesStreamGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{Routing: config.RoutingConfig{ChannelGroups: []config.RoutingChannelGroup{
		{Name: "team", FirstTokenTimeoutSeconds: 30, BusyWaitMaxSeconds: new(0)},
	}}}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/routing-config", nil)

	h.GetRoutingConfig(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		ChannelGroups []struct {
			FirstTokenTimeoutSeconds int  `json:"first-token-timeout-seconds"`
			BusyWaitMaxSeconds       *int `json:"busy-wait-max-seconds"`
		} `json:"channel-groups"`
		Capabilities struct {
			FirstTokenTimeout bool `json:"channel-group-first-token-timeout"`
			BusyWait          bool `json:"channel-group-busy-wait"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Capabilities.FirstTokenTimeout || !body.Capabilities.BusyWait {
		t.Fatalf("stream guard capabilities not advertised: %s", w.Body.String())
	}
	group := body.ChannelGroups[0]
	if group.FirstTokenTimeoutSeconds != 30 || group.BusyWaitMaxSeconds == nil || *group.BusyWaitMaxSeconds != 0 {
		t.Fatalf("group = %+v, want the stored timeout and the explicit 0", group)
	}
}

// Out-of-range values are rejected with the group named, before anything is
// stored, rather than clamped behind the operator's back.
func TestPutRoutingConfigRejectsStreamGuardsOutOfRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, payload := range []string{
		`{"channel-groups":[{"name":"team","first-token-timeout-seconds":2}]}`,
		`{"channel-groups":[{"name":"team","first-token-timeout-seconds":301}]}`,
		`{"channel-groups":[{"name":"team","busy-wait-max-seconds":241}]}`,
		`{"channel-groups":[{"name":"team","busy-wait-max-seconds":-1}]}`,
	} {
		h := &Handler{cfg: &config.Config{}}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/routing-config", strings.NewReader(payload))
		c.Request.Header.Set("Content-Type", "application/json")

		h.PutRoutingConfig(c)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `team`) {
			t.Fatalf("payload %s: status = %d body=%s, want 400 naming the group", payload, w.Code, w.Body.String())
		}
	}
}
