package configsync

import (
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// Domains name the reload unit of a cluster.ConfigEvent. The values travel
// between nodes of different builds during a rolling upgrade, so they must
// never be renamed; a receiver that does not know a domain falls back to a
// full reload.
const (
	DomainAPIKeys            = "api_keys"
	DomainPermissionProfiles = "permission_profiles"
	DomainEndUsers           = "end_users"
	DomainRouting            = "routing"
	DomainProxyPool          = "proxy_pool"
	DomainRuntimeSettings    = "runtime_settings"
	DomainModelConfigs       = "model_configs"
	DomainModelOwnerPresets  = "model_owner_presets"
	DomainPricing            = "pricing"
	DomainIPAccessPolicy     = "ip_access_policy"
	DomainIPAccessRules      = "ip_access_rules"
	DomainTenants            = "tenants"
	DomainCcSwitch           = "ccswitch"
)

// systemTenantID mirrors identity.SystemTenantID. It is duplicated rather than
// imported because identity itself publishes through this package.
const systemTenantID = "00000000-0000-0000-0000-000000000001"

// NormalizeTenantID maps the empty tenant to the system tenant, the way every
// tenant-scoped table stores it.
func NormalizeTenantID(tenantID string) string {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return systemTenantID
	}
	return tenantID
}

// Event builds the ConfigEvent for a write to domain in tenantID.
func Event(domain, tenantID string) cluster.ConfigEvent {
	return cluster.ConfigEvent{Domain: domain, TenantID: NormalizeTenantID(tenantID)}
}

// KeyEvent builds the ConfigEvent for a versioned write of one key.
func KeyEvent(domain, tenantID, key string, version int64) cluster.ConfigEvent {
	ev := Event(domain, tenantID)
	ev.Key = key
	ev.Version = version
	return ev
}

var (
	settingDomainsMu sync.RWMutex
	settingDomains   = map[string]string{}
)

// RouteRuntimeSettingKey announces writes of a runtime_settings key under a
// dedicated domain instead of DomainRuntimeSettings, for settings whose
// receiver is not a config field (the IP access protection policy).
func RouteRuntimeSettingKey(key, domain string) {
	key = strings.TrimSpace(key)
	if key == "" || strings.TrimSpace(domain) == "" {
		return
	}
	settingDomainsMu.Lock()
	settingDomains[key] = domain
	settingDomainsMu.Unlock()
}

// RuntimeSettingDomain returns the domain announced for a runtime_settings key.
func RuntimeSettingDomain(key string) string {
	settingDomainsMu.RLock()
	domain := settingDomains[strings.TrimSpace(key)]
	settingDomainsMu.RUnlock()
	if domain == "" {
		return DomainRuntimeSettings
	}
	return domain
}
