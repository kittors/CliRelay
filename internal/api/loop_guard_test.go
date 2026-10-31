package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loopguard"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
)

const (
	loopGuardTenantA = "aaaaaaaa-0000-0000-0000-00000000000a"
	loopGuardTenantB = "bbbbbbbb-0000-0000-0000-00000000000b"
)

func newLoopGuardRegistry(t *testing.T) *loopguard.Registry {
	t.Helper()
	registry := loopguard.New()
	registry.SetStore(loopguard.NewMemoryStore())
	return registry
}

// gateOf runs a request through the middleware, behind a stand-in for the
// authentication that sets the tenant and the key, and returns the gate the
// handler is given (nil when it is given none).
func gateOf(t *testing.T, registry *loopguard.Registry, method, tenantID, keyID string, mutate func(*http.Request)) handlers.LoopGuardGate {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var gate handlers.LoopGuardGate
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if tenantID != "" {
			c.Set("tenantID", tenantID)
		}
		if keyID != "" {
			c.Set(apiKeyIDContextKey, keyID)
		}
	})
	engine.Use(loopGuardMiddleware(registry))
	engine.Handle(method, "/v1/x", func(c *gin.Context) {
		gate = handlers.LoopGuardGateFromGin(c)
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(method, "/v1/x", nil)
	if mutate != nil {
		mutate(req)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	return gate
}

func enable(t *testing.T, registry *loopguard.Registry, tenantID string, mutate func(*loopguard.Settings)) {
	t.Helper()
	if _, err := registry.Update(context.Background(), tenantID, func(s *loopguard.Settings) error {
		mutate(s)
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestLoopGuardMiddlewareGivesPostsAGateThatReadsTheSettingsWhenAsked(t *testing.T) {
	registry := newLoopGuardRegistry(t)
	gate := gateOf(t, registry, http.MethodPost, loopGuardTenantA, "key-1", nil)
	if gate == nil {
		t.Fatal("a POST was given no gate")
	}
	if policy := gate(); policy != nil {
		t.Fatalf("nothing is switched on but the gate returned %+v", policy)
	}

	// A setting saved after the request began applies at the next question,
	// which is what a WebSocket turn needs.
	enable(t, registry, loopGuardTenantA, func(s *loopguard.Settings) {
		s.Reply = loopguard.ReplyRule{Enabled: loopguard.Bool(true), Window: 512, MaxDistinct: 40}
		s.Tool = loopguard.ToolRule{Enabled: loopguard.Bool(true)}
	})
	policy := gate()
	if policy == nil || policy.Reply == nil || policy.Tool == nil {
		t.Fatalf("policy after the setting was saved = %+v", policy)
	}
	if *policy.Reply != (loopdetect.ReplyLimits{Window: 512, Distinct: 40}) || policy.Tool.Repeats != loopdetect.DefaultToolRepeats {
		t.Fatalf("policy = reply %+v tool %+v", *policy.Reply, *policy.Tool)
	}
	observer, ok := policy.Observer.(executor.LoopGuardObserver)
	if !ok || observer.ReplySource != "tenant" || observer.ToolSource != "tenant" {
		t.Fatalf("observer = %#v", policy.Observer)
	}

	enable(t, registry, loopGuardTenantA, func(s *loopguard.Settings) { s.Reply.Enabled = loopguard.Bool(false) })
	if policy := gate(); policy == nil || policy.Reply != nil || policy.Tool == nil {
		t.Fatalf("policy after the reply check was switched off = %+v", policy)
	}
}

func TestLoopGuardMiddlewareAppliesTheTenantAndTheKey(t *testing.T) {
	registry := newLoopGuardRegistry(t)
	enable(t, registry, loopGuardTenantA, func(s *loopguard.Settings) {
		s.Tool = loopguard.ToolRule{Enabled: loopguard.Bool(true), Repeats: 5}
		s.Keys = map[string]loopguard.Scope{
			"key-exempt": {Tool: loopguard.ToolRule{Enabled: loopguard.Bool(false)}},
			"key-strict": {Tool: loopguard.ToolRule{Repeats: 3}, Reply: loopguard.ReplyRule{Enabled: loopguard.Bool(true)}},
		}
	})

	for name, tc := range map[string]struct {
		tenant, key string
		wantTool    int // 0: off
		wantReply   bool
		wantSource  string
	}{
		"a key of the tenant follows it":        {loopGuardTenantA, "key-other", 5, false, "tenant"},
		"a request without a key id follows it": {loopGuardTenantA, "", 5, false, "tenant"},
		"an exempted key":                       {loopGuardTenantA, "key-exempt", 0, false, ""},
		"a key with its own thresholds":         {loopGuardTenantA, "key-strict", 3, true, "tenant"},
		"another tenant has nothing":            {loopGuardTenantB, "key-other", 0, false, ""},
		"another tenant's key of the same id":   {loopGuardTenantB, "key-strict", 0, false, ""},
		"no tenant is the system tenant":        {"", "key-other", 0, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			gate := gateOf(t, registry, http.MethodPost, tc.tenant, tc.key, nil)
			policy := gate()
			gotTool := 0
			if policy != nil && policy.Tool != nil {
				gotTool = policy.Tool.Repeats
			}
			if gotTool != tc.wantTool {
				t.Fatalf("tool repeats = %d, want %d (policy %+v)", gotTool, tc.wantTool, policy)
			}
			if gotReply := policy != nil && policy.Reply != nil; gotReply != tc.wantReply {
				t.Fatalf("reply on = %v, want %v", gotReply, tc.wantReply)
			}
			if tc.wantTool > 0 {
				if source := policy.Observer.(executor.LoopGuardObserver).ToolSource; source != tc.wantSource {
					t.Fatalf("tool source = %q, want %q", source, tc.wantSource)
				}
			}
		})
	}
}

func TestLoopGuardMiddlewareOnlyGatesRequestsThatCarryAConversation(t *testing.T) {
	registry := newLoopGuardRegistry(t)
	if gate := gateOf(t, registry, http.MethodGet, loopGuardTenantA, "key-1", nil); gate != nil {
		t.Fatal("a plain GET (the model list) was given a gate")
	}
	upgrade := func(r *http.Request) {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	if gate := gateOf(t, registry, http.MethodGet, loopGuardTenantA, "key-1", upgrade); gate == nil {
		t.Fatal("the WebSocket upgrade of the Responses API was given no gate; its turns would never be checked")
	}
}

func TestLoopGuardMiddlewareWithoutAStoreIsOff(t *testing.T) {
	gate := gateOf(t, loopguard.New(), http.MethodPost, loopGuardTenantA, "key-1", nil)
	if gate == nil {
		t.Fatal("no gate")
	}
	if policy := gate(); policy != nil {
		t.Fatalf("policy without a store = %+v", policy)
	}
}
