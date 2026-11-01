package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func initRoutingVirtualModelsDB(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

// callRouting runs handler as the management router would, with version as the
// one the client read (what CaptureConfigVersion takes from If-Match), or none.
func callRouting(t *testing.T, handler gin.HandlerFunc, method, body string, version ...int64) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/v0/management/routing-config", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if len(version) > 0 {
		c.Set(configVersionContextKey, version[0])
	}
	handler(c)
	return w
}

type storedVirtualModels struct {
	VirtualModels []config.VirtualModel `json:"virtual-models"`
	Version       int64                 `json:"version"`
}

func readVirtualModels(t *testing.T, h *Handler) storedVirtualModels {
	t.Helper()
	w := callRouting(t, h.GetRoutingVirtualModels, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET virtual-models: %d %s", w.Code, w.Body.String())
	}
	var out storedVirtualModels
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.VirtualModels == nil {
		t.Fatalf("virtual-models = null, want a list: %s", w.Body.String())
	}
	if etag := w.Header().Get("ETag"); out.Version > 0 && etag == "" {
		t.Fatal("no ETag for a stored version")
	}
	return out
}

func storedRoutingForTest(t *testing.T) config.RoutingConfig {
	t.Helper()
	stored := usage.GetRoutingConfigForTenant("")
	if stored == nil {
		t.Fatal("nothing stored")
	}
	return *stored
}

const smartVirtualModel = `{"name":"smart","members":[{"group":"team","model":"gpt-5"},{"model":"claude-sonnet-4-5"}]}`

// A panel that predates virtual models saves the routing config without the
// key: that leaves them as they are. Naming the key replaces them.
func TestPutRoutingConfigKeepsVirtualModelsTheClientDidNotSend(t *testing.T) {
	initRoutingVirtualModelsDB(t)
	h := &Handler{cfg: &config.Config{}}

	w := callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"channel-groups":[{"name":"team"}],"virtual-models":[`+smartVirtualModel+`]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	w = callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"channel-groups":[{"name":"team"},{"name":"ops"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT without virtual-models: %d %s", w.Code, w.Body.String())
	}
	routing := storedRoutingForTest(t)
	if len(routing.ChannelGroups) != 2 || len(routing.VirtualModels) != 1 || routing.VirtualModels[0].Name != "smart" {
		t.Fatalf("stored = %+v, want both groups and smart kept", routing)
	}
	if h.cfg.Routing.VirtualModels == nil {
		t.Fatal("the running config lost the virtual models")
	}

	w = callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"channel-groups":[{"name":"team"}],"virtual-models":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT with an empty list: %d %s", w.Code, w.Body.String())
	}
	if routing := storedRoutingForTest(t); len(routing.VirtualModels) != 0 {
		t.Fatalf("virtual models = %+v, want none after an empty list", routing.VirtualModels)
	}
}

func TestPutRoutingConfigChecksVirtualModels(t *testing.T) {
	initRoutingVirtualModelsDB(t)
	h := &Handler{cfg: &config.Config{}}

	// Checked as sent: a member of a group that does not exist is refused, not
	// dropped.
	w := callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"virtual-models":[`+smartVirtualModel+`]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `channel group \"team\" does not exist`) {
		t.Fatalf("PUT: %d %s, want 400 naming the group", w.Code, w.Body.String())
	}
	if stored := usage.GetRoutingConfigForTenant(""); stored != nil {
		t.Fatalf("a refused PUT stored %+v", stored)
	}

	if w := callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"channel-groups":[{"name":"team"}],"virtual-models":[`+smartVirtualModel+`]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	// A save that leaves virtual models alone cannot take away a group one
	// of them runs on.
	w = callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"channel-groups":[{"name":"ops"}]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `channel group \"team\" is used by virtual model \"smart\"`) {
		t.Fatalf("PUT removing team: %d %s, want 400 naming the virtual model", w.Code, w.Body.String())
	}
	if routing := storedRoutingForTest(t); len(routing.ChannelGroups) != 1 || routing.ChannelGroups[0].Name != "team" {
		t.Fatalf("stored groups = %+v, want team kept", routing.ChannelGroups)
	}
}

// Carrying the virtual models over does not drop the version check: a client
// that read the config before someone edited them is refused, as for any other
// change made since its read.
func TestPutRoutingConfigWithoutVirtualModelsKeepsTheVersionCheck(t *testing.T) {
	initRoutingVirtualModelsDB(t)
	h := &Handler{cfg: &config.Config{}}
	if w := callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"channel-groups":[{"name":"team"}],"virtual-models":[`+smartVirtualModel+`]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	version := readVirtualModels(t, h).Version
	if w := callRouting(t, h.PutRoutingVirtualModels, http.MethodPut, `{"virtual-models":[]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT virtual-models: %d %s", w.Code, w.Body.String())
	}
	// The client read the version before that edit and says so.
	w := callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"channel-groups":[{"name":"team"}]}`, version)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "config_version_conflict") {
		t.Fatalf("stale PUT: %d %s, want 409", w.Code, w.Body.String())
	}
	if got := readVirtualModels(t, h).VirtualModels; len(got) != 0 {
		t.Fatalf("virtual models = %+v, want the newer edit kept", got)
	}
}

func TestRoutingVirtualModelsEndpoint(t *testing.T) {
	initRoutingVirtualModelsDB(t)
	h := &Handler{cfg: &config.Config{}}

	if got := readVirtualModels(t, h); len(got.VirtualModels) != 0 || got.Version != 0 {
		t.Fatalf("nothing stored: %+v", got)
	}
	if w := callRouting(t, h.PutRoutingConfig, http.MethodPut, `{"channel-groups":[{"name":"team","allowed-models":["gpt-5"]}]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT routing-config: %d %s", w.Code, w.Body.String())
	}

	w := callRouting(t, h.PutRoutingVirtualModels, http.MethodPut, `{"virtual-models":[`+smartVirtualModel+`]}`)
	if w.Code != http.StatusOK || w.Header().Get("ETag") == "" {
		t.Fatalf("PUT: %d %s, want 200 with the new version", w.Code, w.Body.String())
	}
	got := readVirtualModels(t, h)
	if len(got.VirtualModels) != 1 || got.VirtualModels[0].Name != "smart" || got.Version == 0 {
		t.Fatalf("stored = %+v", got)
	}
	// The rest of the routing config is kept as it was.
	if routing := storedRoutingForTest(t); len(routing.ChannelGroups) != 1 || len(routing.ChannelGroups[0].AllowedModels) != 1 {
		t.Fatalf("channel groups = %+v, want team untouched", routing.ChannelGroups)
	}

	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"no key":        {`{}`, "virtual-models is required"},
		"not a list":    {`{"virtual-models":{}}`, "virtual-models is required"},
		"unknown group": {`{"virtual-models":[{"name":"x","members":[{"group":"nope","model":"a"}]}]}`, `channel group \"nope\" does not exist`},
		"nesting":       {`{"virtual-models":[` + smartVirtualModel + `,{"name":"outer","members":[{"model":"smart"}]}]}`, "cannot contain other virtual models"},
	} {
		w := callRouting(t, h.PutRoutingVirtualModels, http.MethodPut, tc.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s: %d %s, want 400 with %q", name, w.Code, w.Body.String(), tc.want)
		}
	}

	w = callRouting(t, h.PutRoutingVirtualModels, http.MethodPut, `{"virtual-models":[]}`, got.Version-1)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale PUT: %d %s, want 409", w.Code, w.Body.String())
	}
	w = callRouting(t, h.PutRoutingVirtualModels, http.MethodPut, `{"virtual-models":[]}`, got.Version)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT at the version read: %d %s", w.Code, w.Body.String())
	}
	if got := readVirtualModels(t, h); len(got.VirtualModels) != 0 {
		t.Fatalf("stored = %+v, want none", got)
	}
}

func TestGetRoutingConfigAdvertisesVirtualModels(t *testing.T) {
	h := &Handler{cfg: &config.Config{Routing: config.RoutingConfig{VirtualModels: []config.VirtualModel{
		{Name: "smart", Members: []config.VirtualModelMember{{Model: "a"}}},
	}}}}
	w := callRouting(t, h.GetRoutingConfig, http.MethodGet, "")
	var body struct {
		VirtualModels []config.VirtualModel `json:"virtual-models"`
		Capabilities  struct {
			VirtualModels bool `json:"virtual-models"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Capabilities.VirtualModels || len(body.VirtualModels) != 1 {
		t.Fatalf("GET routing-config = %s, want the capability and the virtual model", w.Body.String())
	}
}
