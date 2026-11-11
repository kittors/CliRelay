package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

func TestGetProxyPoolEgressStatusListsTheEntriesRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(&config.Config{ProxyPool: []config.ProxyPoolEntry{
		{ID: "hk", Name: "HK", URL: egressRejecting, Enabled: true},
		{ID: "sg", Name: "SG", URL: egressIdle, Enabled: true},
		// Same address, but switched off: it carries no traffic.
		{ID: "hk-old", Name: "HK old", URL: egressRejecting, Enabled: false},
	}}, "", nil)
	defer h.Close()
	breaker, _ := egressTestBreaker()
	h.egress = breaker
	for range egressbreaker.DefaultRejectionThreshold {
		rejectThrough(t, breaker, egressRejecting)
	}
	if _, err := breaker.Allow(egressRejecting); err == nil {
		t.Fatal("the breaker did not open")
	}

	body, raw := getEgressStatus(t, h)
	if !body.Enabled || len(body.Items) != 1 {
		t.Fatalf("body = %s, want the one enabled entry that is refused", raw)
	}
	item := body.Items[0]
	opened := time.Date(2026, 11, 11, 16, 20, 0, 0, time.UTC)
	if item.ID != "hk" || item.Shared || item.Fault != "auth_rejected" || !item.Since.Equal(opened) ||
		!item.NextProbeAt.Equal(opened.Add(egressbreaker.DefaultBaseOpen)) || item.Probing || item.Refused != 1 || item.FailedProbes != 0 {
		t.Fatalf("item = %+v", item)
	}
	for _, leak := range []string{"secret", "alice", "203.0.113"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the response gives away %q: %s", leak, raw)
		}
	}

	breaker.SetEnabled(false)
	body, raw = getEgressStatus(t, h)
	if body.Enabled || len(body.Items) != 0 || !strings.Contains(raw, `"items":[]`) {
		t.Fatalf("with the breaker off: %s", raw)
	}
}

func getEgressStatus(t *testing.T, h *Handler) (proxyEgressStatusResponse, string) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/proxy-pool/egress-status", nil)
	h.GetProxyPoolEgressStatus(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body proxyEgressStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body, w.Body.String()
}
