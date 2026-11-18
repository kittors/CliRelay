package serviceapp

import (
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/modelconfig"
	internalusage "github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func initCatalogScopeTestDB(t *testing.T) {
	t.Helper()
	internalusage.CloseDB()
	if err := internalusage.InitDB(filepath.Join(t.TempDir(), "usage.db"), internalconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(internalusage.CloseDB)
}

func TestOAuthCatalogScopeIsTenantIsolated(t *testing.T) {
	// Credential registration used to read the system tenant's model library only,
	// so a model added by any other tenant never became routable for that tenant's
	// own accounts.
	initCatalogScopeTestDB(t)

	const (
		tenantA = "aaaaaaaa-1111-2222-3333-444444444444"
		tenantB = "bbbbbbbb-1111-2222-3333-444444444444"
	)
	if err := internalusage.UpsertModelConfigForTenant(tenantA, internalusage.ModelConfigRow{
		ModelID: "grok-4.7", OwnedBy: "grok-build", Source: "seed", Enabled: true, PricingMode: "token",
	}); err != nil {
		t.Fatalf("UpsertModelConfigForTenant(A) error = %v", err)
	}
	if err := internalusage.UpsertModelConfigForTenant(tenantB, internalusage.ModelConfigRow{
		ModelID: "other-tenant-model", OwnedBy: "grok-build", Source: "seed", Enabled: true, PricingMode: "token",
	}); err != nil {
		t.Fatalf("UpsertModelConfigForTenant(B) error = %v", err)
	}
	if _, err := modelconfigsettings.UpsertAuthGroupOwnerMappingForTenant(tenantA, "xai", "grok-build"); err != nil {
		t.Fatalf("UpsertAuthGroupOwnerMappingForTenant() error = %v", err)
	}

	rows := ListOAuthProviderModelConfigRowsForTenant(tenantA)
	ids := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		ids[row.ModelID] = struct{}{}
	}
	if _, ok := ids["grok-4.7"]; !ok {
		t.Fatalf("tenant A rows = %v, want its own catalog model", ids)
	}
	if _, ok := ids["other-tenant-model"]; ok {
		t.Fatalf("tenant A rows = %v, must not see another tenant's catalog", ids)
	}

	owners := ListModelOwnersForAuthGroupsForTenant(tenantA, []string{"xai", "unmapped-channel"})
	if len(owners) != 1 || owners[0] != "grok-build" {
		t.Fatalf("tenant A owners = %v, want [grok-build]", owners)
	}
	// Tenant B never created the mapping, so its accounts must not inherit it.
	if owners := ListModelOwnersForAuthGroupsForTenant(tenantB, []string{"xai"}); len(owners) != 0 {
		t.Fatalf("tenant B owners = %v, want none", owners)
	}
}
