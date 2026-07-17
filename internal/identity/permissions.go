package identity

type PermissionSeed struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	Resource  string `json:"resource"`
	Action    string `json:"action"`
	MenuCode  string `json:"menu_code"`
	Sensitive bool   `json:"sensitive"`
}

var PermissionCatalog = []PermissionSeed{
	{Code: "platform.tenants.read", Name: "Read tenants", Scope: "platform", Resource: "tenants", Action: "read"},
	{Code: "platform.tenants.create", Name: "Create tenants", Scope: "platform", Resource: "tenants", Action: "create", Sensitive: true},
	{Code: "platform.tenants.update", Name: "Update tenants", Scope: "platform", Resource: "tenants", Action: "update", Sensitive: true},
	{Code: "platform.tenants.switch", Name: "Switch tenant", Scope: "platform", Resource: "tenants", Action: "switch", Sensitive: true},
	{Code: "platform.users.read", Name: "Read users across tenants", Scope: "platform", Resource: "users", Action: "read"},
	{Code: "platform.users.manage", Name: "Manage users across tenants", Scope: "platform", Resource: "users", Action: "manage", Sensitive: true},
	{Code: "platform.roles.read", Name: "Read roles across tenants", Scope: "platform", Resource: "roles", Action: "read"},
	{Code: "platform.roles.manage", Name: "Manage roles across tenants", Scope: "platform", Resource: "roles", Action: "manage", Sensitive: true},
	{Code: "platform.audit.read", Name: "Read platform audit logs", Scope: "platform", Resource: "audit", Action: "read", Sensitive: true},
	{Code: "platform.audit.delete", Name: "Delete platform audit logs", Scope: "platform", Resource: "audit", Action: "delete", Sensitive: true},
	{Code: "platform.menus.read", Name: "Read menu configuration", Scope: "platform", Resource: "menus", Action: "read"},
	{Code: "platform.menus.update", Name: "Update menu configuration", Scope: "platform", Resource: "menus", Action: "update", Sensitive: true},
	{Code: "system.status.read", Name: "Read system status", Scope: "platform", Resource: "system", Action: "read"},
	{Code: "system.logs.read", Name: "Read system logs", Scope: "platform", Resource: "system_logs", Action: "read", Sensitive: true},
	// Destroying rotated/active file logs is a separate capability from reading them.
	{Code: "system.logs.delete", Name: "Delete system logs", Scope: "platform", Resource: "system_logs", Action: "delete", Sensitive: true},
	{Code: "system.config.read", Name: "Read system configuration", Scope: "platform", Resource: "system_config", Action: "read", Sensitive: true},
	{Code: "system.config.write", Name: "Write system configuration", Scope: "platform", Resource: "system_config", Action: "write", Sensitive: true},
	{Code: "system.update.manage", Name: "Manage system updates", Scope: "platform", Resource: "system_update", Action: "manage", Sensitive: true},
	{Code: "tenant.profile.read", Name: "Read tenant profile", Scope: "tenant", Resource: "tenant_profile", Action: "read"},
	{Code: "tenant.users.read", Name: "Read tenant users", Scope: "tenant", Resource: "users", Action: "read"},
	{Code: "tenant.users.create", Name: "Create tenant users", Scope: "tenant", Resource: "users", Action: "create", Sensitive: true},
	{Code: "tenant.users.update", Name: "Update tenant users", Scope: "tenant", Resource: "users", Action: "update", Sensitive: true},
	{Code: "tenant.users.delete", Name: "Delete tenant users", Scope: "tenant", Resource: "users", Action: "delete", Sensitive: true},
	{Code: "tenant.users.reset_password", Name: "Reset user passwords", Scope: "tenant", Resource: "users", Action: "reset_password", Sensitive: true},
	{Code: "tenant.users.assign_roles", Name: "Assign user roles", Scope: "tenant", Resource: "users", Action: "assign_roles", Sensitive: true},
	{Code: "tenant.roles.read", Name: "Read tenant roles", Scope: "tenant", Resource: "roles", Action: "read"},
	{Code: "tenant.roles.create", Name: "Create tenant roles", Scope: "tenant", Resource: "roles", Action: "create", Sensitive: true},
	{Code: "tenant.roles.update", Name: "Update tenant roles", Scope: "tenant", Resource: "roles", Action: "update", Sensitive: true},
	{Code: "tenant.roles.delete", Name: "Delete tenant roles", Scope: "tenant", Resource: "roles", Action: "delete", Sensitive: true},
	{Code: "tenant.audit.read", Name: "Read tenant audit logs", Scope: "tenant", Resource: "audit", Action: "read", Sensitive: true},
	{Code: "tenant.audit.delete", Name: "Delete tenant audit logs", Scope: "tenant", Resource: "audit", Action: "delete", Sensitive: true},
	{Code: "dashboard.read", Name: "Read dashboard", Scope: "tenant", Resource: "dashboard", Action: "read"},
	{Code: "monitor.read", Name: "Read monitor", Scope: "tenant", Resource: "monitor", Action: "read"},
	{Code: "request_logs.read", Name: "Read request logs", Scope: "tenant", Resource: "request_logs", Action: "read"},
	{Code: "request_logs.content.read", Name: "Read request content", Scope: "tenant", Resource: "request_logs", Action: "read_content", Sensitive: true},
	{Code: "request_logs.delete", Name: "Delete request logs", Scope: "tenant", Resource: "request_logs", Action: "delete", Sensitive: true},
	{Code: "providers.read", Name: "Read providers", Scope: "tenant", Resource: "providers", Action: "read"},
	{Code: "providers.write", Name: "Write providers", Scope: "tenant", Resource: "providers", Action: "write", Sensitive: true},
	{Code: "providers.test", Name: "Test providers", Scope: "tenant", Resource: "providers", Action: "test", Sensitive: true},
	{Code: "auth_files.read", Name: "Read auth files", Scope: "tenant", Resource: "auth_files", Action: "read", Sensitive: true},
	{Code: "auth_files.write", Name: "Write auth files", Scope: "tenant", Resource: "auth_files", Action: "write", Sensitive: true},
	{Code: "auth_files.oauth", Name: "Start OAuth flows", Scope: "tenant", Resource: "auth_files", Action: "oauth", Sensitive: true},
	{Code: "api_keys.read", Name: "Read API keys", Scope: "tenant", Resource: "api_keys", Action: "read", Sensitive: true},
	{Code: "api_keys.write", Name: "Write API keys", Scope: "tenant", Resource: "api_keys", Action: "write", Sensitive: true},
	{Code: "end_users.read", Name: "Read end users", Scope: "tenant", Resource: "end_users", Action: "read", Sensitive: true},
	{Code: "end_users.write", Name: "Write end users", Scope: "tenant", Resource: "end_users", Action: "write", Sensitive: true},
	{Code: "api_key_profiles.read", Name: "Read API key profiles", Scope: "tenant", Resource: "api_key_profiles", Action: "read"},
	{Code: "api_key_profiles.write", Name: "Write API key profiles", Scope: "tenant", Resource: "api_key_profiles", Action: "write", Sensitive: true},
	{Code: "models.read", Name: "Read models", Scope: "tenant", Resource: "models", Action: "read"},
	{Code: "models.write", Name: "Write models", Scope: "tenant", Resource: "models", Action: "write", Sensitive: true},
	{Code: "image_generation.read", Name: "Read image generation", Scope: "tenant", Resource: "image_generation", Action: "read"},
	{Code: "image_generation.write", Name: "Write image generation", Scope: "tenant", Resource: "image_generation", Action: "write", Sensitive: true},
	{Code: "image_generation.test", Name: "Test image generation", Scope: "tenant", Resource: "image_generation", Action: "test", Sensitive: true},
	{Code: "routing.read", Name: "Read routing", Scope: "tenant", Resource: "routing", Action: "read"},
	{Code: "routing.write", Name: "Write routing", Scope: "tenant", Resource: "routing", Action: "write", Sensitive: true},
	{Code: "proxies.read", Name: "Read proxies", Scope: "tenant", Resource: "proxies", Action: "read", Sensitive: true},
	{Code: "proxies.write", Name: "Write proxies", Scope: "tenant", Resource: "proxies", Action: "write", Sensitive: true},
	{Code: "proxies.test", Name: "Test proxies", Scope: "tenant", Resource: "proxies", Action: "test", Sensitive: true},
	{Code: "tenant_settings.read", Name: "Read tenant settings", Scope: "tenant", Resource: "tenant_settings", Action: "read"},
	{Code: "tenant_settings.write", Name: "Write tenant settings", Scope: "tenant", Resource: "tenant_settings", Action: "write", Sensitive: true},
}

func menuCodeForPermission(permission PermissionSeed) string {
	switch permission.Resource {
	case "tenants", "tenant_profile":
		return "governance.tenants"
	case "users":
		return "governance.users"
	case "roles":
		return "governance.roles"
	case "audit":
		return "governance.audit"
	case "menus":
		return MenuManagementCode
	case "system":
		return "runtime.system"
	case "system_logs":
		return "runtime.logs"
	case "system_config", "system_update", "tenant_settings":
		return "system.config"
	case "dashboard":
		return "dashboard"
	case "monitor":
		return "runtime.monitor"
	case "request_logs":
		return "runtime.request-logs"
	case "providers":
		return "access.providers"
	case "auth_files":
		return "system.account-security"
	case "api_keys":
		return "access.api-keys"
	case "end_users":
		return "access.end-users"
	case "api_key_profiles":
		return "system.api-key-permissions"
	case "models":
		return "models.catalog"
	case "image_generation":
		return "models.image-generation"
	case "routing":
		return "models.channel-groups"
	case "proxies":
		return "models.proxies"
	default:
		return ""
	}
}
