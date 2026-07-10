package routes

import (
	"net/http"
	"net/http/httptest"
	"slices"
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

	if got, want := len(routes), 232; got != want {
		t.Fatalf("route count = %d, want %d", got, want)
	}
	if got, want := sortedRouteKeys(routes), expectedManagementRoutes(); !slices.Equal(got, want) {
		t.Fatalf("management route snapshot mismatch\nmissing: %v\nextra: %v", diffStrings(want, got), diffStrings(got, want))
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
		"POST /v0/management/cline-api-key/usage",
		"GET /v0/management/ollama-cloud-api-key",
		"PUT /v0/management/ollama-cloud-api-key",
		"PATCH /v0/management/ollama-cloud-api-key",
		"DELETE /v0/management/ollama-cloud-api-key",
		"POST /v0/management/ollama-cloud-api-key/usage",
		"GET /v0/management/auth-files/models",
		"GET /v0/management/image-generation/size-presets",
		"PUT /v0/management/image-generation/size-presets",
		"GET /v0/management/identity-fingerprint/account",
		"PUT /v0/management/identity-fingerprint/account/policy",
		"DELETE /v0/management/identity-fingerprint/account/profile",
		"GET /v0/management/identity-fingerprint/codex/recommendations",
		"DELETE /v0/management/identity-fingerprint/account/profile",
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

func sortedRouteKeys(routes map[string]gin.RouteInfo) []string {
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func diffStrings(want, got []string) []string {
	gotSet := make(map[string]struct{}, len(got))
	for _, value := range got {
		gotSet[value] = struct{}{}
	}
	var diff []string
	for _, value := range want {
		if _, ok := gotSet[value]; !ok {
			diff = append(diff, value)
		}
	}
	return diff
}

func expectedManagementRoutes() []string {
	routes := []string{
		"DELETE /v0/management/ampcode/model-mappings",
		"DELETE /v0/management/ampcode/upstream-api-key",
		"DELETE /v0/management/ampcode/upstream-api-keys",
		"DELETE /v0/management/ampcode/upstream-url",
		"DELETE /v0/management/api-key-entries",
		"DELETE /v0/management/api-keys",
		"DELETE /v0/management/auth-files",
		"DELETE /v0/management/bedrock-api-key",
		"DELETE /v0/management/claude-api-key",
		"DELETE /v0/management/cline-api-key",
		"DELETE /v0/management/codex-api-key",
		"DELETE /v0/management/gemini-api-key",
		"DELETE /v0/management/identity-fingerprint/account/profile",
		"DELETE /v0/management/identity-fingerprint/learned",
		"DELETE /v0/management/logs",
		"DELETE /v0/management/model-configs/*id",
		"DELETE /v0/management/oauth-excluded-models",
		"DELETE /v0/management/oauth-model-alias",
		"DELETE /v0/management/ollama-cloud-api-key",
		"DELETE /v0/management/opencode-go-api-key",
		"DELETE /v0/management/openai-compatibility",
		"DELETE /v0/management/proxy-url",
		"DELETE /v0/management/usage/logs",
		"DELETE /v0/management/vertex-api-key",
		"GET /v0/management/ampcode",
		"GET /v0/management/ampcode/force-model-mappings",
		"GET /v0/management/ampcode/model-mappings",
		"GET /v0/management/ampcode/restrict-management-to-localhost",
		"GET /v0/management/ampcode/upstream-api-key",
		"GET /v0/management/ampcode/upstream-api-keys",
		"GET /v0/management/ampcode/upstream-url",
		"GET /v0/management/antigravity-auth-url",
		"GET /v0/management/anthropic-auth-url",
		"GET /v0/management/api-key-entries",
		"GET /v0/management/api-key-permission-profiles",
		"GET /v0/management/api-keys",
		"GET /v0/management/auth-files",
		"GET /v0/management/auth-files/download",
		"GET /v0/management/auth-files/models",
		"GET /v0/management/auth-group-model-owner-mappings",
		"GET /v0/management/auto-update/channel",
		"GET /v0/management/auto-update/enabled",
		"GET /v0/management/bedrock-api-key",
		"GET /v0/management/ccswitch-import-configs",
		"GET /v0/management/channel-groups",
		"GET /v0/management/claude-api-key",
		"GET /v0/management/cline-api-key",
		"GET /v0/management/codex-api-key",
		"GET /v0/management/codex-auth-url",
		"GET /v0/management/codex-oauth-admission",
		"GET /v0/management/config",
		"GET /v0/management/config.yaml",
		"GET /v0/management/dashboard-summary",
		"GET /v0/management/debug",
		"GET /v0/management/error-logs-max-files",
		"GET /v0/management/force-model-prefix",
		"GET /v0/management/gemini-api-key",
		"GET /v0/management/gemini-cli-auth-url",
		"GET /v0/management/get-auth-status",
		"GET /v0/management/identity-fingerprint",
		"GET /v0/management/identity-fingerprint/account",
		"GET /v0/management/identity-fingerprint/codex/recommendations",
		"GET /v0/management/iflow-auth-url",
		"GET /v0/management/image-generation/channels",
		"GET /v0/management/image-generation/size-presets",
		"GET /v0/management/image-generation/test/:task_id",
		"GET /v0/management/kimi-auth-url",
		"GET /v0/management/latest-version",
		"GET /v0/management/logging-to-file",
		"GET /v0/management/logs",
		"GET /v0/management/logs-max-total-size-mb",
		"GET /v0/management/max-retry-interval",
		"GET /v0/management/model-configs",
		"GET /v0/management/model-definitions/:channel",
		"GET /v0/management/model-openrouter-sync",
		"GET /v0/management/model-owner-presets",
		"GET /v0/management/model-path-availability",
		"GET /v0/management/model-pricing",
		"GET /v0/management/models",
		"GET /v0/management/models/configured-availability",
		"GET /v0/management/oauth-excluded-models",
		"GET /v0/management/oauth-model-alias",
		"GET /v0/management/ollama-cloud-api-key",
		"GET /v0/management/opencode-go-api-key",
		"GET /v0/management/openai-compatibility",
		"GET /v0/management/proxy-pool",
		"GET /v0/management/proxy-url",
		"GET /v0/management/public/ccswitch-import-configs",
		"GET /v0/management/public/ping",
		"GET /v0/management/public/usage",
		"GET /v0/management/public/usage/chart-data",
		"GET /v0/management/public/usage/logs",
		"GET /v0/management/public/usage/logs/:id/content",
		"GET /v0/management/qwen-auth-url",
		"GET /v0/management/quota-exceeded/switch-preview-model",
		"GET /v0/management/quota-exceeded/switch-project",
		"GET /v0/management/request-error-logs",
		"GET /v0/management/request-error-logs/:name",
		"GET /v0/management/request-log",
		"GET /v0/management/request-log-by-id/:id",
		"GET /v0/management/request-retry",
		"GET /v0/management/routing-config",
		"GET /v0/management/routing/strategy",
		"GET /v0/management/system-stats",
		"GET /v0/management/system-stats/ws",
		"GET /v0/management/update/check",
		"GET /v0/management/update/current",
		"GET /v0/management/update/events",
		"GET /v0/management/update/progress",
		"GET /v0/management/usage",
		"GET /v0/management/usage/auth-file-group-trend",
		"GET /v0/management/usage/auth-file-trend",
		"GET /v0/management/usage/chart-data",
		"GET /v0/management/usage/entity-stats",
		"GET /v0/management/usage/export",
		"GET /v0/management/usage/logs",
		"GET /v0/management/usage/logs/:id/content",
		"GET /v0/management/usage/logs/:id/egress",
		"GET /v0/management/usage-statistics-enabled",
		"GET /v0/management/vertex-api-key",
		"GET /v0/management/xai-auth-url",
		"GET /v0/management/ws-auth",
		"PATCH /v0/management/ampcode/force-model-mappings",
		"PATCH /v0/management/ampcode/model-mappings",
		"PATCH /v0/management/ampcode/restrict-management-to-localhost",
		"PATCH /v0/management/ampcode/upstream-api-key",
		"PATCH /v0/management/ampcode/upstream-api-keys",
		"PATCH /v0/management/ampcode/upstream-url",
		"PATCH /v0/management/api-key-entries",
		"PATCH /v0/management/api-keys",
		"PATCH /v0/management/auth-files/fields",
		"PATCH /v0/management/auth-files/status",
		"PATCH /v0/management/auth-group-model-owner-mappings",
		"PATCH /v0/management/auto-update/channel",
		"PATCH /v0/management/auto-update/enabled",
		"PATCH /v0/management/bedrock-api-key",
		"PATCH /v0/management/claude-api-key",
		"PATCH /v0/management/cline-api-key",
		"PATCH /v0/management/codex-api-key",
		"PATCH /v0/management/debug",
		"PATCH /v0/management/error-logs-max-files",
		"PATCH /v0/management/force-model-prefix",
		"PATCH /v0/management/gemini-api-key",
		"PATCH /v0/management/logging-to-file",
		"PATCH /v0/management/logs-max-total-size-mb",
		"PATCH /v0/management/max-retry-interval",
		"PATCH /v0/management/oauth-excluded-models",
		"PATCH /v0/management/oauth-model-alias",
		"PATCH /v0/management/ollama-cloud-api-key",
		"PATCH /v0/management/opencode-go-api-key",
		"PATCH /v0/management/openai-compatibility",
		"PATCH /v0/management/proxy-pool/:id",
		"PATCH /v0/management/proxy-url",
		"PATCH /v0/management/quota-exceeded/switch-preview-model",
		"PATCH /v0/management/quota-exceeded/switch-project",
		"PATCH /v0/management/request-log",
		"PATCH /v0/management/request-retry",
		"PATCH /v0/management/routing/strategy",
		"PATCH /v0/management/usage-statistics-enabled",
		"PATCH /v0/management/vertex-api-key",
		"PATCH /v0/management/ws-auth",
		"POST /v0/management/api-call",
		"POST /v0/management/auth-files",
		"POST /v0/management/iflow-auth-url",
		"POST /v0/management/image-generation/test",
		"POST /v0/management/model-configs",
		"POST /v0/management/model-openrouter-sync/run",
		"POST /v0/management/oauth-callback",
		"POST /v0/management/cline-api-key/usage",
		"POST /v0/management/ollama-cloud-api-key/usage",
		"POST /v0/management/opencode-go-api-key/usage",
		"POST /v0/management/proxy-pool/check",
		"POST /v0/management/public/ccswitch-import-configs",
		"POST /v0/management/public/usage",
		"POST /v0/management/public/usage/chart-data",
		"POST /v0/management/public/usage/logs",
		"POST /v0/management/public/usage/logs/:id/content",
		"POST /v0/management/public/usage/summary",
		"POST /v0/management/quota/clear-status",
		"POST /v0/management/quota/reconcile",
		"POST /v0/management/usage/auth-file-quota-snapshot",
		"POST /v0/management/usage/import",
		"POST /v0/management/update/apply",
		"POST /v0/management/vertex/import",
		"PUT /v0/management/ampcode/force-model-mappings",
		"PUT /v0/management/ampcode/model-mappings",
		"PUT /v0/management/ampcode/restrict-management-to-localhost",
		"PUT /v0/management/ampcode/upstream-api-key",
		"PUT /v0/management/ampcode/upstream-api-keys",
		"PUT /v0/management/ampcode/upstream-url",
		"PUT /v0/management/api-key-entries",
		"PUT /v0/management/api-key-permission-profiles",
		"PUT /v0/management/api-keys",
		"PUT /v0/management/auto-update/channel",
		"PUT /v0/management/auto-update/enabled",
		"PUT /v0/management/bedrock-api-key",
		"PUT /v0/management/ccswitch-import-configs",
		"PUT /v0/management/claude-api-key",
		"PUT /v0/management/cline-api-key",
		"PUT /v0/management/codex-api-key",
		"PUT /v0/management/codex-oauth-admission",
		"PUT /v0/management/config.yaml",
		"PUT /v0/management/debug",
		"PUT /v0/management/error-logs-max-files",
		"PUT /v0/management/force-model-prefix",
		"PUT /v0/management/gemini-api-key",
		"PUT /v0/management/identity-fingerprint",
		"PUT /v0/management/identity-fingerprint/account/policy",
		"PUT /v0/management/image-generation/size-presets",
		"PUT /v0/management/logging-to-file",
		"PUT /v0/management/logs-max-total-size-mb",
		"PUT /v0/management/max-retry-interval",
		"PUT /v0/management/model-configs/*id",
		"PUT /v0/management/model-openrouter-sync",
		"PUT /v0/management/model-owner-presets",
		"PUT /v0/management/model-pricing",
		"PUT /v0/management/oauth-excluded-models",
		"PUT /v0/management/oauth-model-alias",
		"PUT /v0/management/ollama-cloud-api-key",
		"PUT /v0/management/opencode-go-api-key",
		"PUT /v0/management/openai-compatibility",
		"PUT /v0/management/proxy-pool",
		"PUT /v0/management/proxy-url",
		"PUT /v0/management/quota-exceeded/switch-preview-model",
		"PUT /v0/management/quota-exceeded/switch-project",
		"PUT /v0/management/request-log",
		"PUT /v0/management/request-retry",
		"PUT /v0/management/routing-config",
		"PUT /v0/management/routing/strategy",
		"PUT /v0/management/usage-statistics-enabled",
		"PUT /v0/management/vertex-api-key",
		"PUT /v0/management/ws-auth",
	}
	slices.Sort(routes)
	return routes
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
