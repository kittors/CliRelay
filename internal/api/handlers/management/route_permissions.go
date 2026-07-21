package management

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

// ManagementRequestPermission returns the RBAC permission for a management
// method+path. Empty means middleware denies the request (fail closed).
// Exported so package routes can assert every registered route is mapped.
func ManagementRequestPermission(method, path string) string {
	return permissionForManagementRequest(method, path)
}

func principalHasManagementRequestPermission(principal identity.Principal, method, path, permission string) bool {
	if permission != "" && principal.Has(permission) {
		return true
	}
	relative := strings.TrimSuffix(strings.TrimPrefix(path, "/v0/management"), "/")
	return (method == http.MethodGet || method == http.MethodHead) &&
		strings.HasPrefix(relative, "/end-users/") &&
		strings.HasSuffix(relative, "/daily-spending/reset-history") &&
		principal.Has("end_users.write")
}

// permissionForManagementRequest maps a management HTTP method+path to a
// permission code. Unknown routes return "" so middleware fails closed.
func permissionForManagementRequest(method, path string) string {
	relative := strings.TrimPrefix(path, "/v0/management")
	write := method != http.MethodGet && method != http.MethodHead
	switch {
	case relative == "/tenants" && method == http.MethodGet:
		return "platform.tenants.read"
	case relative == "/tenants" && method == http.MethodPost:
		return "platform.tenants.create"
	case strings.HasPrefix(relative, "/tenants/"):
		return "platform.tenants.update"
	case relative == "/users" && method == http.MethodGet:
		return "tenant.users.read"
	case relative == "/users" && method == http.MethodPost:
		return "tenant.users.create"
	// Key sub-resources under end-users use api_keys.* so legacy key-admin roles
	// keep working after the sidebar moves to 用户账号.
	case strings.HasPrefix(relative, "/end-users/") && strings.Contains(relative, "/api-keys"):
		if write {
			return "api_keys.write"
		}
		return "api_keys.read"
	case strings.HasPrefix(relative, "/end-users"):
		if write {
			return "end_users.write"
		}
		return "end_users.read"
	case strings.HasSuffix(relative, "/reset-password") && strings.HasPrefix(relative, "/users/"):
		return "tenant.users.reset_password"
	case strings.HasSuffix(relative, "/roles") && strings.HasPrefix(relative, "/users/"):
		return "tenant.users.assign_roles"
	case strings.HasPrefix(relative, "/users/") && method == http.MethodDelete:
		return "tenant.users.delete"
	case strings.HasPrefix(relative, "/users/"):
		return "tenant.users.update"
	case relative == "/audit-logs" || (strings.HasPrefix(relative, "/audit-logs/") && method == http.MethodGet):
		return "tenant.audit.read"
	case strings.HasPrefix(relative, "/audit-logs/") && method == http.MethodDelete:
		return "tenant.audit.delete"
	case relative == "/menus" && method == http.MethodGet:
		return "platform.menus.read"
	case relative == "/menus" && write:
		return "platform.menus.update"
	case strings.HasPrefix(relative, "/menus/"):
		return "platform.menus.update"
	case relative == "/roles" && method == http.MethodGet, relative == "/permissions":
		return "tenant.roles.read"
	case relative == "/roles" && method == http.MethodPost:
		return "tenant.roles.create"
	case strings.HasSuffix(relative, "/users") && strings.HasPrefix(relative, "/roles/"):
		return "tenant.users.assign_roles"
	case strings.HasPrefix(relative, "/roles/") && method == http.MethodDelete:
		return "tenant.roles.delete"
	case strings.HasPrefix(relative, "/roles/"):
		return "tenant.roles.update"
	case strings.HasPrefix(relative, "/dashboard-summary"):
		return "dashboard.read"
	case strings.HasPrefix(relative, "/system-stats"):
		return "system.status.read"
	// Sensitive request/error log files — not system.config.
	case strings.HasPrefix(relative, "/request-error-logs"), strings.HasPrefix(relative, "/request-log-by-id"):
		return "system.logs.read"
	// Exact /logs only: /logs-max-total-size-mb is a config knob below.
	// DELETE clears rotated logs and truncates main.log — not a read capability.
	case relative == "/logs" || strings.HasPrefix(relative, "/logs/"):
		if method == http.MethodDelete {
			return "system.logs.delete"
		}
		return "system.logs.read"
	case strings.HasPrefix(relative, "/usage/logs"):
		if method == http.MethodDelete {
			return "request_logs.delete"
		}
		if strings.Contains(relative, "/content") || strings.Contains(relative, "/egress") {
			return "request_logs.content.read"
		}
		return "request_logs.read"
	// Write ops under /usage must not share monitor.read.
	case relative == "/usage/import" && write:
		return "system.config.write"
	case relative == "/usage/auth-file-quota-snapshot" && write:
		return "auth_files.write"
	// Exact /usage* stats paths only — /usage-statistics-enabled is config.
	case relative == "/usage" || strings.HasPrefix(relative, "/usage/"):
		return "monitor.read"
	// AI account status read model: same capability surface as auth-files list.
	case strings.HasPrefix(relative, "/ai-accounts/status-refresh") && write:
		return "auth_files.write"
	case strings.HasPrefix(relative, "/ai-accounts/status"):
		if write {
			return "auth_files.write"
		}
		return "auth_files.read"
	case strings.HasPrefix(relative, "/auth-files"), relative == "/vertex/import", relative == "/get-auth-status", strings.Contains(relative, "auth-url"), strings.Contains(relative, "oauth"):
		if relative == "/get-auth-status" || strings.Contains(relative, "auth-url") || strings.Contains(relative, "oauth") {
			return "auth_files.oauth"
		}
		if write {
			return "auth_files.write"
		}
		return "auth_files.read"
	// Account/profile fingerprint APIs are auth-file scoped and read the shared
	// AI-account catalog (same account_key → same fingerprints). Global preset
	// PUT (/identity-fingerprint) still uses process runtime settings; tenants
	// can read learned state but only platform admins rewrite the shared preset.
	case relative == "/identity-fingerprint" && write:
		return "system.config.write"
	case strings.HasPrefix(relative, "/identity-fingerprint"):
		if write {
			return "auth_files.write"
		}
		return "auth_files.read"
	case strings.HasPrefix(relative, "/api-key-permission-profiles"):
		if write {
			return "api_key_profiles.write"
		}
		return "api_key_profiles.read"
	case strings.HasPrefix(relative, "/api-keys"), strings.HasPrefix(relative, "/api-key-entries"):
		if write {
			return "api_keys.write"
		}
		return "api_keys.read"
	case strings.HasPrefix(relative, "/model"), strings.Contains(relative, "model-"):
		if write {
			return "models.write"
		}
		return "models.read"
	case strings.HasPrefix(relative, "/image-generation"):
		if strings.Contains(relative, "/test") {
			return "image_generation.test"
		}
		if write {
			return "image_generation.write"
		}
		return "image_generation.read"
	case strings.HasPrefix(relative, "/channel-groups"), strings.HasPrefix(relative, "/routing"), strings.HasPrefix(relative, "/ccswitch"):
		if write {
			return "routing.write"
		}
		return "routing.read"
	case strings.HasPrefix(relative, "/proxy-pool"):
		if strings.Contains(relative, "/check") {
			return "proxies.test"
		}
		if write {
			return "proxies.write"
		}
		return "proxies.read"
	case strings.HasPrefix(relative, "/ampcode"):
		if strings.Contains(relative, "model-mapping") {
			if write {
				return "models.write"
			}
			return "models.read"
		}
		if strings.Contains(relative, "upstream-api-key") {
			if write {
				return "providers.write"
			}
			return "providers.read"
		}
		if write {
			return "system.config.write"
		}
		return "system.config.read"
	case strings.HasPrefix(relative, "/update"), strings.HasPrefix(relative, "/latest-version"), strings.HasPrefix(relative, "/auto-update"):
		if write {
			return "system.update.manage"
		}
		return "system.status.read"
	case strings.HasPrefix(relative, "/config.yaml"):
		if write {
			return "system.config.write"
		}
		return "system.config.read"
	// Process-global config knobs (explicit so default stays fail-closed).
	case relative == "/config":
		return "tenant_settings.read"
	case relative == "/debug", relative == "/logging-to-file", relative == "/logs-max-total-size-mb",
		relative == "/error-logs-max-files", relative == "/usage-statistics-enabled",
		relative == "/request-log", strings.HasPrefix(relative, "/request-log-storage"),
		relative == "/ws-auth":
		if write {
			return "system.config.write"
		}
		return "system.config.read"
	case strings.HasPrefix(relative, "/proxy-url"), strings.HasPrefix(relative, "/quota"), strings.HasPrefix(relative, "/request-retry"), strings.HasPrefix(relative, "/max-retry"), strings.HasPrefix(relative, "/force-model-prefix"):
		if write {
			return "tenant_settings.write"
		}
		return "tenant_settings.read"
	case strings.HasSuffix(relative, "/usage") && strings.Contains(relative, "api-key"):
		return "providers.test"
	case strings.Contains(relative, "api-key"), strings.HasPrefix(relative, "/openai-compatibility"):
		if write {
			return "providers.write"
		}
		return "providers.read"
	case strings.HasPrefix(relative, "/api-call"):
		return "providers.test"
	default:
		// Fail closed: unmapped routes get no permission (middleware rejects "").
		return ""
	}
}
