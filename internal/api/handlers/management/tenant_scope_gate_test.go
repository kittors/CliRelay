package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

func TestDeniesTenantResourceScopeAllowsPlatformAdminOnGlobalLogs(t *testing.T) {
	businessTenant := identity.Principal{
		PlatformAdmin:   true,
		EffectiveTenant: identity.Tenant{ID: "tenant-potato"},
		HomeTenant:      identity.Tenant{ID: identity.SystemTenantID},
	}
	if deniesTenantResourceScope(businessTenant, "/v0/management/logs") {
		t.Fatal("platform super-admin switched into a business tenant must still reach process-global /logs")
	}

	tenantOperator := identity.Principal{
		PlatformAdmin:   false,
		EffectiveTenant: identity.Tenant{ID: "tenant-potato"},
		HomeTenant:      identity.Tenant{ID: "tenant-potato"},
	}
	if !deniesTenantResourceScope(tenantOperator, "/v0/management/logs") {
		t.Fatal("non-platform tenant session must not reach process-global /logs")
	}
	if deniesTenantResourceScope(tenantOperator, "/v0/management/usage/logs") {
		t.Fatal("tenant-scoped request logs must stay available to business tenants")
	}

	systemSession := identity.Principal{
		PlatformAdmin:   false,
		EffectiveTenant: identity.Tenant{ID: identity.SystemTenantID},
	}
	if deniesTenantResourceScope(systemSession, "/v0/management/logs") {
		t.Fatal("system-tenant session must reach process-global /logs")
	}
}

func TestTenantScopedManagementPathIncludesProviderRuntimeRoutes(t *testing.T) {
	for _, path := range []string{
		"/v0/management/gemini-api-key",
		"/v0/management/claude-api-key",
		"/v0/management/bedrock-api-key",
		"/v0/management/opencode-go-api-key/usage",
		"/v0/management/cline-api-key/usage",
		"/v0/management/ollama-cloud-api-key/usage",
		"/v0/management/codex-api-key",
		"/v0/management/vertex-api-key",
		"/v0/management/openai-compatibility",
		"/v0/management/oauth-model-alias",
		"/v0/management/codex-oauth-admission",
		// Auth-file quota preview for tenant-imported OAuth credentials.
		"/v0/management/api-call",
		// Per-account identity fingerprints must stay readable by tenant admins
		// after credentials migrate off the system tenant.
		"/v0/management/identity-fingerprint",
		"/v0/management/identity-fingerprint/account",
		"/v0/management/identity-fingerprint/account/policy",
		"/v0/management/identity-fingerprint/codex/recommendations",
		// Portal end-user accounts + per-user API keys are tenant business data.
		"/v0/management/end-users",
		"/v0/management/end-users/eu-1",
		"/v0/management/end-users/eu-1/api-keys",
		"/v0/management/end-users/eu-1/api-keys/k1/default",
	} {
		if !isTenantScopedManagementPath(path) {
			t.Errorf("isTenantScopedManagementPath(%q) = false", path)
		}
	}
}

// The model catalogue's probes run on management authority and scope
// themselves to the caller's effective tenant, so a business tenant must be
// able to reach all three. /models/test and /video-generation/test were left
// out, which is why testing a chat, image or video model from the catalogue
// answered "tenant business resources are not enabled for this tenant".
func TestTenantScopedManagementPathIncludesModelProbes(t *testing.T) {
	tenantOperator := identity.Principal{
		PlatformAdmin:   false,
		EffectiveTenant: identity.Tenant{ID: "tenant-potato"},
		HomeTenant:      identity.Tenant{ID: "tenant-potato"},
	}
	for _, path := range []string{
		"/v0/management/models/test",
		"/v0/management/image-generation/test",
		"/v0/management/image-generation/test/task-1",
		"/v0/management/video-generation/test",
		"/v0/management/video-generation/test/task-1",
	} {
		if !isTenantScopedManagementPath(path) {
			t.Errorf("isTenantScopedManagementPath(%q) = false", path)
		}
		if deniesTenantResourceScope(tenantOperator, path) {
			t.Errorf("business tenant must reach %s", path)
		}
	}
}

func TestDeniesTenantResourceScopeAllowsBusinessTenantEndUsers(t *testing.T) {
	tenantOperator := identity.Principal{
		PlatformAdmin:   false,
		EffectiveTenant: identity.Tenant{ID: "tenant-potato"},
		HomeTenant:      identity.Tenant{ID: "tenant-potato"},
	}
	for _, path := range []string{
		"/v0/management/end-users",
		"/v0/management/end-users/eu-1",
		"/v0/management/end-users/eu-1/api-keys",
	} {
		if deniesTenantResourceScope(tenantOperator, path) {
			t.Fatalf("business tenant must reach %s", path)
		}
	}
}

func TestTenantScopedManagementPathKeepsGlobalRuntimeRoutesBlocked(t *testing.T) {
	for _, path := range []string{
		"/v0/management/config.yaml",
		"/v0/management/oauth-excluded-models",
		"/v0/management/proxy-url",
		"/v0/management/request-retry",
		"/v0/management/update/apply",
		"/v0/management/logs",
	} {
		if isTenantScopedManagementPath(path) {
			t.Errorf("isTenantScopedManagementPath(%q) = true", path)
		}
	}
}

func TestProviderManagementRoutesUseProviderPermissions(t *testing.T) {
	for _, path := range []string{
		"/v0/management/gemini-api-key",
		"/v0/management/openai-compatibility",
	} {
		if got := permissionForManagementRequest("GET", path); got != "providers.read" {
			t.Errorf("GET %s permission = %q, want providers.read", path, got)
		}
		if got := permissionForManagementRequest("PUT", path); got != "providers.write" {
			t.Errorf("PUT %s permission = %q, want providers.write", path, got)
		}
	}
	for _, path := range []string{
		"/v0/management/api-call",
		"/v0/management/opencode-go-api-key/usage",
	} {
		if got := permissionForManagementRequest("POST", path); got != "providers.test" {
			t.Errorf("POST %s permission = %q, want providers.test", path, got)
		}
	}
}
