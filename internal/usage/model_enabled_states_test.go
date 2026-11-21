package usage

import "testing"

// The disabled-model filter lists only model_id and enabled so its 15-second
// refresh stays off the wire. It must still reach the answer the full listing
// gives, including a business tenant's overrides of the system catalog, so the
// two listings are compared row for row.
func TestListModelEnabledStatesForTenantMatchesFullListing(t *testing.T) {
	initModelConfigTestDB(t)

	seed := func(tenantID, modelID string, enabled bool) {
		t.Helper()
		row := ModelConfigRow{ModelID: modelID, Enabled: enabled, PricingMode: "token", Source: "user"}
		if err := UpsertModelConfigForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertModelConfigForTenant(%s, %s) error = %v", tenantID, modelID, err)
		}
	}
	seed(systemTenantID, "sys-off", false)
	seed(systemTenantID, "sys-on", true)
	seed(systemTenantID, "sys-off-tenant-on", false)
	seed(systemTenantID, "sys-on-tenant-off", true)
	seed(systemTenantID, "Case-Differs", false)
	seed(businessTenantID, "sys-off-tenant-on", true)
	seed(businessTenantID, "sys-on-tenant-off", false)
	seed(businessTenantID, "case-differs", true)
	seed(businessTenantID, "tenant-only-off", false)

	const tenantWithoutRows = "00000000-0000-0000-0000-0000000000bb"
	for _, tenantID := range []string{"", systemTenantID, businessTenantID, tenantWithoutRows} {
		full := ListModelConfigsForTenant(tenantID)
		narrow := ListModelEnabledStatesForTenant(tenantID)
		if len(narrow) != len(full) {
			t.Fatalf("tenant %q: %d enabled states, want %d like the full listing", tenantID, len(narrow), len(full))
		}
		for i := range full {
			if narrow[i].ModelID != full[i].ModelID || narrow[i].Enabled != full[i].Enabled {
				t.Errorf("tenant %q row %d = {%s %v}, want {%s %v}", tenantID, i, narrow[i].ModelID, narrow[i].Enabled, full[i].ModelID, full[i].Enabled)
			}
		}
	}

	// Pin the merge itself, so the comparison above cannot pass by both
	// listings ignoring the tenant's rows.
	states := make(map[string]bool)
	for _, state := range ListModelEnabledStatesForTenant(businessTenantID) {
		states[state.ModelID] = state.Enabled
	}
	for modelID, want := range map[string]bool{
		"sys-off":           false,
		"sys-on":            true,
		"sys-off-tenant-on": true,
		"sys-on-tenant-off": false,
		"case-differs":      true,
		"tenant-only-off":   false,
	} {
		if got, ok := states[modelID]; !ok || got != want {
			t.Errorf("business tenant %s: enabled=%v present=%v, want enabled=%v", modelID, got, ok, want)
		}
	}
	if _, ok := states["Case-Differs"]; ok {
		t.Error("system row Case-Differs should be replaced by the tenant's case-differs row")
	}
}

// The disabled-model filter skips listing when this says no disabled row can
// reach the tenant's merged catalog, so it must say yes whenever the tenant or
// the system catalog holds one, and only then.
func TestHasDisabledModelConfigsForTenantFollowsStoredRows(t *testing.T) {
	initModelConfigTestDB(t)
	const otherTenant = "00000000-0000-0000-0000-0000000000bb"

	check := func(step, tenantID string, want bool) {
		t.Helper()
		got, err := HasDisabledModelConfigsForTenant(tenantID)
		if err != nil {
			t.Fatalf("%s: HasDisabledModelConfigsForTenant(%q) error = %v", step, tenantID, err)
		}
		if got != want {
			t.Errorf("%s: HasDisabledModelConfigsForTenant(%q) = %v, want %v", step, tenantID, got, want)
		}
	}
	seed := func(tenantID, modelID string, enabled bool) {
		t.Helper()
		row := ModelConfigRow{ModelID: modelID, Enabled: enabled, PricingMode: "token", Source: "user"}
		if err := UpsertModelConfigForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertModelConfigForTenant(%s, %s) error = %v", tenantID, modelID, err)
		}
	}
	all := []string{"", systemTenantID, businessTenantID, otherTenant}

	seed(systemTenantID, "sys-on", true)
	seed(businessTenantID, "tenant-on", true)
	for _, tenantID := range all {
		check("nothing disabled", tenantID, false)
	}

	seed(businessTenantID, "tenant-off", false)
	check("tenant row disabled", businessTenantID, true)
	check("tenant row disabled", systemTenantID, false)
	check("tenant row disabled", otherTenant, false)

	seed(systemTenantID, "sys-off", false)
	for _, tenantID := range all {
		check("system row disabled", tenantID, true)
	}
}
