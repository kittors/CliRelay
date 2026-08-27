package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestBuildModelTestPayloadShapesOneUserTurn(t *testing.T) {
	payload, err := buildModelTestPayload("kimi-k3", "How is the weather?")
	if err != nil {
		t.Fatalf("buildModelTestPayload: %v", err)
	}
	if got := gjson.GetBytes(payload, "model").String(); got != "kimi-k3" {
		t.Fatalf("model = %q, want kimi-k3", got)
	}
	if got := gjson.GetBytes(payload, "messages.#").Int(); got != 1 {
		t.Fatalf("messages = %d, want exactly the probe turn", got)
	}
	if got := gjson.GetBytes(payload, "messages.0.content").String(); got != "How is the weather?" {
		t.Fatalf("content = %q, want the prompt", got)
	}
	if gjson.GetBytes(payload, "stream").Bool() {
		t.Fatal("the probe must not stream; the handler reads one response body")
	}
}

// The channel selector in the panel used to only pick an API key, so a probe
// could land on a different account than the operator selected. Scoping the
// execution is what makes that selector mean something.
func TestModelTestMetadataScopesTenantAndChannel(t *testing.T) {
	meta := modelTestMetadata("tenant-a", " kimi ")
	if got := meta["allowed-channels"]; got != "kimi" {
		t.Fatalf("allowed-channels = %v, want the trimmed channel", got)
	}
	if meta["tenant_id"] != "tenant-a" {
		t.Fatalf("tenant_id = %v, want tenant-a", meta["tenant_id"])
	}

	// No channel selected means "any account that serves the model", not a
	// restriction to the empty channel.
	if _, ok := modelTestMetadata("tenant-a", "  ")["allowed-channels"]; ok {
		t.Fatal("an unset channel must not restrict the probe")
	}
}

func TestModelTestContentPrefersAssistantText(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"sunny"}}]}`)
	if got := modelTestContent(body); got != "sunny" {
		t.Fatalf("content = %q, want sunny", got)
	}
	// An unexpected shape stays inspectable rather than turning into "".
	raw := []byte(`{"unexpected":true}`)
	if got := modelTestContent(raw); got != string(raw) {
		t.Fatalf("content = %q, want the raw body for an unknown shape", got)
	}
}

func postModelTest(t *testing.T, h *Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/models/test", strings.NewReader(string(encoded)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PostModelTest(c)
	return rec
}

func TestPostModelTestRejectsIncompleteRequests(t *testing.T) {
	h := &Handler{}
	for name, body := range map[string]map[string]string{
		"no model":  {"prompt": "hi"},
		"no prompt": {"model": "kimi-k3"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := postModelTest(t, h, body).Code; got != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", got)
			}
		})
	}
}

// A model no provider serves is a 404, not the generic selection failure an
// operator would otherwise have to interpret.
func TestPostModelTestReportsUnknownModel(t *testing.T) {
	h := &Handler{}
	rec := postModelTest(t, h, map[string]string{"model": "not-a-real-model-xyz", "prompt": "hi"})
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 404 (unknown model) or 503 (no auth manager)", rec.Code)
	}
}
