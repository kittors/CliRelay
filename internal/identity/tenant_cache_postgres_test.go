package identity

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// In single-node mode the tenant cache must not delay a suspension: the write
// invalidates the row, so the very next access check refuses the tenant. And
// while the database is unreachable, a recently checked tenant keeps working.
func TestPostgresTenantCacheFollowsLocalWritesAndSurvivesOutage(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	ctx := context.Background()
	db, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`TRUNCATE audit_logs,user_sessions,user_roles,role_permissions,menus,users,roles,permissions,tenants CASCADE`); err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	if err = service.Bootstrap(ctx, "Bootstrap-Password-123!"); err != nil {
		t.Fatal(err)
	}
	login, err := service.Login(ctx, "admin", "Bootstrap-Password-123!", false, "test")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := service.Authenticate(ctx, login.AccessToken, "")
	if err != nil {
		t.Fatal(err)
	}
	tenant, _, err := service.CreateTenant(ctx, admin, CreateTenantInput{
		Name: "Cache Tenant", ExpiresAt: time.Now().Add(time.Hour),
		AdminUsername: "cache-admin", AdminDisplayName: "Cache Admin", AdminPassword: "Tenant-Password-123!",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.ValidateTenantAccess(ctx, tenant.ID); err != nil {
		t.Fatalf("access check of an active tenant: %v", err)
	}

	suspended, err := service.UpdateTenant(ctx, admin, tenant.ID, "suspended", nil, tenant.Version)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Status != "suspended" {
		t.Fatalf("UpdateTenant returned %+v, want the new row", suspended)
	}
	if err = service.ValidateTenantAccess(ctx, tenant.ID); !errors.Is(err, ErrTenantSuspended) {
		t.Fatalf("access check right after suspension = %v, want ErrTenantSuspended", err)
	}

	active, err := service.UpdateTenant(ctx, admin, tenant.ID, "active", nil, suspended.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.ValidateTenantAccess(ctx, tenant.ID); err != nil {
		t.Fatalf("access check after reactivation: %v", err)
	}
	disabled, err := service.DeleteTenant(ctx, admin, tenant.ID, active.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.ValidateTenantAccess(ctx, tenant.ID); !errors.Is(err, ErrTenantSuspended) {
		t.Fatalf("access check right after disabling = %v, want ErrTenantSuspended", err)
	}
	if _, err = service.UpdateTenant(ctx, admin, tenant.ID, "active", nil, disabled.Version); err != nil {
		t.Fatal(err)
	}
	if err = service.ValidateTenantAccess(ctx, tenant.ID); err != nil {
		t.Fatalf("access check after re-enabling: %v", err)
	}

	// The database goes away; the row checked a moment ago keeps the tenant up.
	service.tenants.age(tenant.ID, tenantCacheTTL+time.Second)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = service.ValidateTenantAccess(ctx, tenant.ID); err != nil {
		t.Fatalf("access check during the outage = %v, want the cached row", err)
	}
	if service.TenantStaleServed() == 0 {
		t.Fatal("stale answer not counted")
	}
}
