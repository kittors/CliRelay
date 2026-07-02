package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func TestRegisterManagementRouteTable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	RegisterManagement(engine, &managementhandlers.Handler{}, ManagementOptions{})

	routes := make(map[string]gin.RouteInfo)
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, exists := routes[key]; exists {
			t.Fatalf("duplicate route registered: %s", key)
		}
		routes[key] = route
	}

	if got, want := len(routes), 222; got != want {
		t.Fatalf("route count = %d, want %d", got, want)
	}

	required := []string{
		"GET /v0/management/dashboard-summary",
		"GET /v0/management/system-stats/ws",
		"GET /v0/management/model-configs",
		"PUT /v0/management/model-configs/*id",
		"PATCH /v0/management/auth-group-model-owner-mappings",
		"PATCH /v0/management/proxy-pool/:id",
		"GET /v0/management/usage/logs/:id/content",
		"GET /v0/management/usage/logs/:id/egress",
		"POST /v0/management/api-call",
		"PATCH /v0/management/api-key-entries",
		"POST /v0/management/opencode-go-api-key/usage",
		"GET /v0/management/cline-api-key",
		"PUT /v0/management/cline-api-key",
		"PATCH /v0/management/cline-api-key",
		"DELETE /v0/management/cline-api-key",
		"GET /v0/management/auth-files/models",
		"GET /v0/management/image-generation/size-presets",
		"PUT /v0/management/image-generation/size-presets",
		"GET /v0/management/identity-fingerprint/account",
		"GET /v0/management/identity-fingerprint/codex/recommendations",
		"DELETE /v0/management/identity-fingerprint/learned",
		"GET /v0/management/codex-oauth-admission",
		"PUT /v0/management/codex-oauth-admission",
		"POST /v0/management/quota/clear-status",
		"POST /v0/management/oauth-callback",
		"GET /v0/management/public/ping",
		"GET /v0/management/public/usage/logs/:id/content",
		"POST /v0/management/public/usage/summary",
	}
	for _, key := range required {
		if _, ok := routes[key]; !ok {
			t.Fatalf("required route %s was not registered", key)
		}
	}
}

func TestManagementRoutesApplySecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	RegisterManagement(engine, &managementhandlers.Handler{}, ManagementOptions{})

	req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "no-store, private, max-age=0" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
}
