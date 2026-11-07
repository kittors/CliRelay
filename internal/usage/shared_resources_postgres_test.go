package usage_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// TestPostgresSharedResourceGrants runs the grant model on PostgreSQL, where
// proxy_pool rows reference tenants and every query goes through the compat
// driver's placeholder rewrite: grants, the cascade when the platform drops a
// proxy, the cross-tenant override lookup and the merged tenant pool.
func TestPostgresSharedResourceGrants(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	usage.CloseDB()
	t.Cleanup(usage.CloseDB)
	if err := usage.InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	db := usage.RuntimeDB()
	ctx := context.Background()

	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	prefix := fmt.Sprintf("grant-test-%d-", time.Now().UnixNano())
	hk, sg := prefix+"hk", prefix+"sg"
	previousSystemPool := usage.ListProxyPool()
	for _, tenant := range []string{tenantA, tenantB} {
		if _, err := db.Exec(`INSERT INTO tenants (id, slug, name, type, status, expires_at)
			VALUES ($1, $2, $3, 'standard', 'active', now() + interval '1 day')`, tenant, prefix+tenant[:8], prefix+tenant[:8]); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = usage.ReplaceProxyPool(previousSystemPool)
		_, _ = db.Exec(`DELETE FROM platform_resource_grants WHERE resource_id LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM proxy_pool WHERE tenant_id IN ($1, $2)`, tenantA, tenantB)
		_, _ = db.Exec(`DELETE FROM config_versions WHERE tenant_id IN ($1, $2)`, tenantA, tenantB)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id IN ($1, $2)`, tenantA, tenantB)
	})

	systemPool := append(append([]config.ProxyPoolEntry(nil), previousSystemPool...),
		config.ProxyPoolEntry{ID: hk, URL: "socks5://203.0.113.1:1080", Enabled: true},
		config.ProxyPoolEntry{ID: sg, URL: "socks5://203.0.113.2:1080", Enabled: true},
	)
	if err := usage.ReplaceProxyPool(systemPool); err != nil {
		t.Fatalf("replace system pool: %v", err)
	}
	for _, tenant := range []string{tenantA, tenantB} {
		if err := usage.ReplaceProxyPoolForTenant(tenant, []config.ProxyPoolEntry{{ID: sg, URL: "http://198.51.100.1:3128", Enabled: true}}); err != nil {
			t.Fatalf("replace tenant pool: %v", err)
		}
	}
	if _, err := usage.ReplaceResourceGrants(ctx, resourcegrants.KindProxy, []string{hk, sg}, []string{tenantA}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := usage.ReplaceResourceGrants(ctx, resourcegrants.KindAuth, []string{prefix + "codex.json"}, []string{resourcegrants.AllTenants}); err != nil {
		t.Fatalf("grant credential to everybody: %v", err)
	}

	holders, err := usage.TenantsOverridingProxies(ctx, []string{hk, sg})
	if err != nil {
		t.Fatalf("override lookup: %v", err)
	}
	wantHolders := []string{tenantA, tenantB}
	slices.Sort(wantHolders)
	if got := holders[sg]; !slices.Equal(got, wantHolders) {
		t.Fatalf("holders of %s = %v, want %v", sg, got, wantHolders)
	}
	if _, ok := holders[hk]; ok {
		t.Fatalf("no tenant holds %s: %v", hk, holders)
	}

	tenantCfg := usage.BuildTenantRuntimeConfig(&config.Config{}, tenantA)
	if got := tenantCfg.ResolveProxyURL(hk, ""); got != "socks5://203.0.113.1:1080" {
		t.Fatalf("tenant A %s = %q, want the shared entry", hk, got)
	}
	if got := tenantCfg.ResolveProxyURL(sg, ""); got != "http://198.51.100.1:3128" {
		t.Fatalf("tenant A %s = %q, want its own entry", sg, got)
	}

	before := configsync.CollectionVersion(ctx, db, configsync.DomainResourceGrants, "")
	if err := usage.ReplaceProxyPool(append(append([]config.ProxyPoolEntry(nil), previousSystemPool...), systemPool[len(systemPool)-1])); err != nil {
		t.Fatalf("drop %s from the system pool: %v", hk, err)
	}
	grants, err := usage.ListResourceGrants(ctx, resourcegrants.KindProxy)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := grants[hk]; ok {
		t.Fatalf("grant of the dropped proxy survived: %v", grants[hk])
	}
	if !slices.Equal(grants[sg], []string{tenantA}) {
		t.Fatalf("grant of the kept proxy = %v", grants[sg])
	}
	if after := configsync.CollectionVersion(ctx, db, configsync.DomainResourceGrants, ""); after != before+1 {
		t.Fatalf("grants version %d -> %d, want one bump for the cascade", before, after)
	}
	authGrants, err := usage.ListResourceGrants(ctx, resourcegrants.KindAuth)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(authGrants[prefix+"codex.json"], []string{resourcegrants.AllTenants}) {
		t.Fatalf("credential grant = %v, want [*] untouched by the proxy cascade", authGrants[prefix+"codex.json"])
	}
}
