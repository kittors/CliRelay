package modelconfig

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// A business tenant inherits the system catalog's toggles and can override them
// in either direction (case-insensitively); the request path must refuse
// exactly what the merged catalog shows as off.
func TestDisabledModelIDsForTenantHonoursTenantOverrides(t *testing.T) {
	initModelConfigServiceTestDB(t)
	const tenant = "00000000-0000-0000-0000-0000000000aa"

	upsert := func(tenantID, modelID string, enabled bool) {
		t.Helper()
		row := usage.ModelConfigRow{ModelID: modelID, Enabled: enabled, PricingMode: "token", Source: "user"}
		if err := usage.UpsertModelConfigForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertModelConfigForTenant(%q, %q) error = %v", tenantID, modelID, err)
		}
	}
	upsert("", "sys-off", false)
	upsert("", "sys-off-tenant-on", false)
	upsert("", "sys-on-tenant-off", true)
	upsert(tenant, "Sys-Off-Tenant-On", true)
	upsert(tenant, "sys-on-tenant-off", false)
	// Writes through usage do not reach this package's cache.
	InvalidateDisabledModelCache()

	disabled := DisabledModelIDsForTenant(tenant)
	for _, want := range []string{"sys-off", "sys-on-tenant-off"} {
		if _, ok := disabled[want]; !ok {
			t.Errorf("DisabledModelIDsForTenant(tenant) missing %q (got %v)", want, disabled)
		}
	}
	if _, ok := disabled["sys-off-tenant-on"]; ok {
		t.Errorf("the tenant re-enabled sys-off-tenant-on, yet it is still disabled (got %v)", disabled)
	}

	system := DisabledModelIDsForTenant("")
	if _, ok := system["sys-off-tenant-on"]; !ok {
		t.Errorf("a tenant override leaked into the system set: sys-off-tenant-on missing (got %v)", system)
	}
	if _, ok := system["sys-on-tenant-off"]; ok {
		t.Errorf("a tenant override leaked into the system set: sys-on-tenant-off present (got %v)", system)
	}
}
