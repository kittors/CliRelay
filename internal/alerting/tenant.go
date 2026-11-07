package alerting

import "github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"

// systemTenantID mirrors identity.SystemTenantID; see configsync for why it is
// duplicated rather than imported.
const systemTenantID = "00000000-0000-0000-0000-000000000001"

// TrustedTenant reports whether tenantID is the platform operator's own tenant.
// Only it may aim a generic webhook at a private address or at plain http: the
// server makes the request from inside its own network, so that is a power the
// operator has and a tenant must not (see ValidateWebhookURL).
func TrustedTenant(tenantID string) bool {
	return configsync.NormalizeTenantID(tenantID) == systemTenantID
}

func init() {
	// These rows are read by this package, not by a config field, so their writes
	// are announced under their own domain: left on the default one, every write
	// of a history entry would make the other nodes reload the whole runtime
	// config.
	for _, key := range []string{SettingKey, EventsKey, SentKey} {
		configsync.RouteRuntimeSettingKey(key, configsync.DomainAlerts)
	}
}
