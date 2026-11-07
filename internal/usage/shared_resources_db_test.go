package usage

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
)

func TestBuildTenantRuntimeConfigResolvesSharedProxies(t *testing.T) {
	cleanup := setupProxyPoolTestDB(t)
	defer cleanup()

	const tenantA = "11111111-1111-1111-1111-111111111111"
	const tenantB = "22222222-2222-2222-2222-222222222222"
	if err := ReplaceProxyPool([]config.ProxyPoolEntry{
		{ID: "hk-1", URL: "socks5://user:pass@203.0.113.1:1080", Enabled: true},
		{ID: "sg-1", URL: "socks5://user:pass@203.0.113.2:1080", Enabled: true},
		{ID: "private", URL: "socks5://203.0.113.3:1080", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	// Tenant A keeps its own sg-1, and has disabled it: its accounts must not
	// fall through to the platform's address for the same ID.
	if err := ReplaceProxyPoolForTenant(tenantA, []config.ProxyPoolEntry{
		{ID: "sg-1", URL: "http://198.51.100.9:3128", Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := ReplaceResourceGrants(ctx, resourcegrants.KindProxy, []string{"hk-1"}, []string{resourcegrants.AllTenants}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceResourceGrants(ctx, resourcegrants.KindProxy, []string{"sg-1"}, []string{tenantA, tenantB}); err != nil {
		t.Fatal(err)
	}

	base := &config.Config{}
	a := BuildTenantRuntimeConfig(base, tenantA)
	if got := a.ResolveProxyURL("hk-1", ""); got != "socks5://user:pass@203.0.113.1:1080" {
		t.Fatalf("tenant A hk-1 = %q, want the shared entry", got)
	}
	if got := a.ResolveProxyURL("sg-1", ""); got != "" {
		t.Fatalf("tenant A sg-1 = %q, want nothing: its own disabled entry overrides the shared one", got)
	}
	if got := a.ResolveProxyURL("private", ""); got != "" {
		t.Fatalf("tenant A resolved an unshared platform proxy: %q", got)
	}

	b := BuildTenantRuntimeConfig(base, tenantB)
	if got := b.ResolveProxyURL("sg-1", ""); got != "socks5://user:pass@203.0.113.2:1080" {
		t.Fatalf("tenant B sg-1 = %q, want the shared entry", got)
	}
	if len(b.ProxyPool) != 2 {
		t.Fatalf("tenant B pool = %#v, want hk-1 and sg-1", b.ProxyPool)
	}

	// Shared entries never land in a tenant's own pool.
	if own := ListProxyPoolForTenant(tenantB); len(own) != 0 {
		t.Fatalf("tenant B own pool = %#v", own)
	}
	if shared := SharedProxyPoolForTenant(systemTenantID); shared != nil {
		t.Fatalf("the system tenant owns its pool; shared view = %#v", shared)
	}
}
