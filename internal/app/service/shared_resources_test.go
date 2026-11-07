package serviceapp

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	_ "modernc.org/sqlite"
)

const (
	sharedProxyTenantA = "11111111-1111-1111-1111-111111111111"
	sharedProxyTenantB = "22222222-2222-2222-2222-222222222222"
)

func setupSharedProxyRuntime(t *testing.T) {
	t.Helper()
	if err := internalusage.InitDB(filepath.Join(t.TempDir(), "usage.db"), internalconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("init usage db: %v", err)
	}
	t.Cleanup(internalusage.CloseDB)

	db, err := sql.Open("sqlite", fmt.Sprintf("file:shared_proxy_tenants_%d?mode=memory&cache=shared", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	// ListTenants reads each tenant while its cursor is open; one connection
	// would deadlock.
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE tenants (
		id TEXT PRIMARY KEY, slug TEXT NOT NULL, name TEXT NOT NULL, type TEXT NOT NULL, status TEXT NOT NULL,
		expires_at TIMESTAMP NULL, description TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		version INTEGER NOT NULL DEFAULT 1)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{identity.SystemTenantID, sharedProxyTenantA, sharedProxyTenantB} {
		kind := "standard"
		if id == identity.SystemTenantID {
			kind = "system"
		}
		if _, err := db.Exec(`INSERT INTO tenants (id, slug, name, type, status) VALUES (?, ?, ?, ?, 'active')`, id, id, id, kind); err != nil {
			t.Fatal(err)
		}
	}
	previous := identity.Default()
	identity.SetDefault(identity.NewService(db))
	t.Cleanup(func() { identity.SetDefault(previous) })

	boundTenantProxyPoolsMu.Lock()
	boundTenantProxyPools = map[string][]internalconfig.ProxyPoolEntry{}
	boundTenantProxyPoolsMu.Unlock()
}

func TestRefreshSharedProxyPoolsRebindsOnlyTenantsWhosePoolChanged(t *testing.T) {
	setupSharedProxyRuntime(t)
	ctx := context.Background()
	if err := internalusage.ReplaceProxyPool([]internalconfig.ProxyPoolEntry{
		{ID: "hk-1", URL: "socks5://203.0.113.1:1080", Enabled: true},
		{ID: "sg-1", URL: "socks5://203.0.113.2:1080", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := internalusage.ReplaceResourceGrants(ctx, resourcegrants.KindProxy, []string{"hk-1"}, []string{sharedProxyTenantA}); err != nil {
		t.Fatal(err)
	}
	if _, err := internalusage.ReplaceResourceGrants(ctx, resourcegrants.KindProxy, []string{"sg-1"}, []string{sharedProxyTenantB}); err != nil {
		t.Fatal(err)
	}

	base := &config.Config{}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, tenant := range []string{sharedProxyTenantA, sharedProxyTenantB} {
		auth := &coreauth.Auth{ID: tenant + "/claude.json", TenantID: tenant, Provider: "claude", Status: coreauth.StatusActive}
		if _, err := manager.Register(ctx, auth); err != nil {
			t.Fatal(err)
		}
		RegisterExecutorForAuth(manager, base, auth, true, nil)
	}
	executorOf := func(tenant string) coreauth.ProviderExecutor {
		exec, ok := manager.ExecutorForTenant(tenant, "claude")
		if !ok {
			t.Fatalf("no claude executor for %s", tenant)
		}
		return exec
	}
	beforeA, beforeB := executorOf(sharedProxyTenantA), executorOf(sharedProxyTenantB)

	// Nothing changed: no rebind, no cut websocket sessions.
	RefreshSharedProxyPools(base, manager)
	if executorOf(sharedProxyTenantA) != beforeA || executorOf(sharedProxyTenantB) != beforeB {
		t.Fatal("a refresh without changes rebound executors")
	}

	// The platform moves hk-1, which only tenant A uses.
	if err := internalusage.ReplaceProxyPool([]internalconfig.ProxyPoolEntry{
		{ID: "hk-1", URL: "socks5://203.0.113.9:1080", Enabled: true},
		{ID: "sg-1", URL: "socks5://203.0.113.2:1080", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	RefreshSharedProxyPools(base, manager)
	if executorOf(sharedProxyTenantA) == beforeA {
		t.Fatal("tenant A kept the executor bound to the old hk-1 address")
	}
	if executorOf(sharedProxyTenantB) != beforeB {
		t.Fatal("tenant B was rebound although its effective pool did not change")
	}

	// Revoking a grant is a change too.
	afterA := executorOf(sharedProxyTenantA)
	if _, err := internalusage.ReplaceResourceGrants(ctx, resourcegrants.KindProxy, []string{"hk-1"}, nil); err != nil {
		t.Fatal(err)
	}
	RefreshSharedProxyPools(base, manager)
	if executorOf(sharedProxyTenantA) == afterA {
		t.Fatal("tenant A kept resolving a proxy no longer shared with it")
	}
}

func TestLoadSharedAuthGrantsAppliesStoredGrants(t *testing.T) {
	setupSharedProxyRuntime(t)
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "codex-platform.json", TenantID: identity.SystemTenantID, Provider: "codex", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if _, err := internalusage.ReplaceResourceGrants(context.Background(), resourcegrants.KindAuth, []string{auth.ID}, []string{resourcegrants.AllTenants}); err != nil {
		t.Fatal(err)
	}
	LoadSharedAuthGrants(manager)
	if shared := manager.ListSharedWithTenant(sharedProxyTenantB); len(shared) != 1 {
		t.Fatalf("shared with B = %d credentials, want 1", len(shared))
	}
}
