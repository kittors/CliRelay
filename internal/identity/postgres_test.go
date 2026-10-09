package identity

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

func TestPostgresBootstrapFreshAdminRequiresPassword(t *testing.T) {
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
	err = service.Bootstrap(ctx, "")
	if !errors.Is(err, ErrValidation) {
		t.Errorf("Bootstrap() error = %v, want ErrValidation", err)
	}
	if err = service.Bootstrap(ctx, "Bootstrap-Password-123!"); err != nil {
		t.Fatalf("restore shared postgres identity fixture: %v", err)
	}
}

func TestPostgresBootstrapRetiresAppearanceMenu(t *testing.T) {
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
	// The row CliRelay#1134 seeded: a deployment that ran that build still has it.
	seedRetired := func() {
		t.Helper()
		if _, err := db.Exec(`
			INSERT INTO menus (code,parent_code,menu_type,path,component,label_key,icon,sort_order,system_protected)
			VALUES ('system.appearance','group.system','menu','/system/appearance','appearance','shell.nav_appearance','palette',30,true)
		`); err != nil {
			t.Fatalf("seed retired appearance menu: %v", err)
		}
	}
	appearanceRows := func() int {
		t.Helper()
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM menus WHERE code = 'system.appearance'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	seedRetired()
	if err = service.Bootstrap(ctx, ""); err != nil {
		t.Fatalf("bootstrap over the retired appearance menu: %v", err)
	}
	if got := appearanceRows(); got != 0 {
		t.Fatalf("system.appearance rows after bootstrap = %d, want 0", got)
	}

	// An admin hung a menu under it: deleting would violate the parent_code RESTRICT, and a failed
	// seed aborts startup, so the row stays and bootstrap still succeeds.
	seedRetired()
	if _, err = db.Exec(`
		INSERT INTO menus (code,parent_code,menu_type,path,component,label_key,sort_order,system_protected)
		VALUES ('custom.theme-notes','system.appearance','menu','/system/appearance/notes','embed','custom.theme-notes',1,false)
	`); err != nil {
		t.Fatalf("seed admin child menu: %v", err)
	}
	if err = service.Bootstrap(ctx, ""); err != nil {
		t.Fatalf("bootstrap with a child under the retired menu: %v", err)
	}
	if got := appearanceRows(); got != 1 {
		t.Fatalf("system.appearance rows with a child = %d, want 1 (kept)", got)
	}
	if _, err = db.Exec(`DELETE FROM menus WHERE code IN ('custom.theme-notes','system.appearance')`); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresIdentityLifecycle(t *testing.T) {
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
	if err = service.Bootstrap(ctx, ""); err != nil {
		t.Fatalf("bootstrap with existing admin and empty password: %v", err)
	}
	users, err := service.ListUsers(ctx, SystemTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].ID != SystemUserID || users[0].DisplayName != "Super Administrator" || len(users[0].RoleCodes) != 1 || users[0].RoleCodes[0] != "platform_super_admin" {
		t.Fatalf("system users=%+v", users)
	}
	systemRoles, err := service.ListRoles(ctx, SystemTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(systemRoles) != 1 || systemRoles[0].ID != SystemRoleID || systemRoles[0].Name != "Administrator" || !systemRoles[0].SystemProtected {
		t.Fatalf("system roles=%+v", systemRoles)
	}
	login, err := service.Login(ctx, "admin", "Bootstrap-Password-123!", false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !login.Principal.PlatformAdmin || login.Principal.HomeTenant.ID != SystemTenantID {
		t.Fatalf("principal=%+v", login.Principal)
	}
	if !containsMenu(login.Principal.Menus, MenuManagementCode) {
		t.Fatalf("platform principal menus=%+v", login.Principal.Menus)
	}
	principal, err := service.Authenticate(ctx, login.AccessToken, "")
	if err != nil {
		t.Fatal(err)
	}
	tenant, admin, err := service.CreateTenant(ctx, principal, CreateTenantInput{Name: "Tenant A", ExpiresAt: time.Now().Add(time.Hour), AdminUsername: "tenant-admin", AdminDisplayName: "Tenant Admin", AdminPassword: "Tenant-Password-123!"})
	if err != nil {
		t.Fatal(err)
	}
	if tenant.ID == "" || admin.TenantID != tenant.ID {
		t.Fatalf("tenant=%+v admin=%+v", tenant, admin)
	}
	if !strings.HasPrefix(tenant.Slug, "tenant-") || len(tenant.Slug) != len("tenant-")+32 {
		t.Fatalf("generated tenant slug = %q", tenant.Slug)
	}
	tenantAdminLogin, err := service.Login(ctx, "tenant-admin", "Tenant-Password-123!", false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if containsMenu(tenantAdminLogin.Principal.Menus, MenuManagementCode) || !containsMenu(tenantAdminLogin.Principal.Menus, "governance.users") {
		t.Fatalf("tenant admin menus=%+v", tenantAdminLogin.Principal.Menus)
	}
	if err = service.ChangePassword(ctx, tenantAdminLogin.Principal, "Tenant-Password-123!", "Tenant-Password-456!"); err != nil {
		t.Fatal(err)
	}
	tenantAdmin, err := service.Authenticate(ctx, tenantAdminLogin.AccessToken, "")
	if err != nil {
		t.Fatal(err)
	}
	roles, err := service.ListRoles(ctx, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tenantAdminRoleID string
	var tenantAdminRoleVersion int64
	for _, role := range roles {
		if role.Code == "tenant_admin" {
			tenantAdminRoleID = role.ID
			tenantAdminRoleVersion = role.Version
		}
	}
	if tenantAdminRoleID == "" {
		t.Fatal("tenant admin role not found")
	}
	limitedRole, err := service.CreateRole(ctx, tenantAdmin, tenant.ID, "Limited user manager", "", []string{
		"tenant.users.read",
		"tenant.users.create",
		"tenant.users.assign_roles",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(limitedRole.Code, "role_") || len(limitedRole.Code) != len("role_")+32 {
		t.Fatalf("generated role code = %q", limitedRole.Code)
	}
	limitedUser, _, err := service.CreateUser(ctx, tenantAdmin, tenant.ID, "limited-manager", "Limited Manager", "Limited-Password-123!", []string{limitedRole.ID})
	if err != nil {
		t.Fatal(err)
	}
	limitedLogin, err := service.Login(ctx, "limited-manager", "Limited-Password-123!", false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = service.ChangePassword(ctx, limitedLogin.Principal, "Limited-Password-123!", "Limited-Password-456!"); err != nil {
		t.Fatal(err)
	}
	limitedPrincipal, err := service.Authenticate(ctx, limitedLogin.AccessToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.CreateUser(ctx, limitedPrincipal, tenant.ID, "escalated-user", "Escalated User", "Escalated-Password-123!", []string{tenantAdminRoleID}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("create user with non-delegable role err=%v", err)
	}
	rolelessUser, _, err := service.CreateUser(ctx, limitedPrincipal, tenant.ID, "roleless-user", "Roleless User", "Roleless-Password-123!", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.AssignUserRoles(ctx, limitedPrincipal, tenant.ID, rolelessUser.ID, []string{tenantAdminRoleID}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("assign non-delegable role err=%v", err)
	}
	if err = service.ReplaceRoleUsers(ctx, tenantAdmin, tenant.ID, limitedRole.ID, []string{rolelessUser.ID}, limitedRole.Version); err != nil {
		t.Fatalf("replace role users: %v", err)
	}
	users, err = service.ListUsers(ctx, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	var roleAssigned bool
	for _, user := range users {
		if user.ID == rolelessUser.ID {
			for _, roleID := range user.RoleIDs {
				roleAssigned = roleAssigned || roleID == limitedRole.ID
			}
		}
	}
	if !roleAssigned {
		t.Fatalf("role %s was not assigned to user %s", limitedRole.ID, rolelessUser.ID)
	}
	if err = service.ReplaceRoleUsers(ctx, tenantAdmin, tenant.ID, limitedRole.ID, nil, limitedRole.Version); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("replace role users stale version err=%v", err)
	}
	if err = service.ReplaceRoleUsers(ctx, tenantAdmin, tenant.ID, tenantAdminRoleID, nil, tenantAdminRoleVersion); !errors.Is(err, ErrProtectedResource) {
		t.Fatalf("remove all tenant admins err=%v", err)
	}
	if err = service.DeleteUser(ctx, tenantAdmin, tenant.ID, limitedUser.ID); err != nil {
		t.Fatalf("delete audited user: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if _, err = service.UpdateTenant(ctx, principal, tenant.ID, "active", &past, tenant.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Login(ctx, "tenant-admin", "Tenant-Password-456!", false, "test"); !errors.Is(err, ErrTenantExpired) {
		t.Fatalf("expired login err=%v", err)
	}
}

func TestPostgresTenantAdminUserProtection(t *testing.T) {
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
	platformAdmin, err := service.Authenticate(ctx, login.AccessToken, "")
	if err != nil {
		t.Fatal(err)
	}
	tenant, _, err := service.CreateTenant(ctx, platformAdmin, CreateTenantInput{
		Name:             "Protected Tenant",
		ExpiresAt:        time.Now().Add(time.Hour),
		AdminUsername:    "protected-tenant-admin",
		AdminDisplayName: "Protected Tenant Admin",
		AdminPassword:    "Tenant-Password-123!",
	})
	if err != nil {
		t.Fatal(err)
	}
	tenantLogin, err := service.Login(ctx, "protected-tenant-admin", "Tenant-Password-123!", false, "test")
	if err != nil {
		t.Fatal(err)
	}
	tenantAdmin, err := service.Authenticate(ctx, tenantLogin.AccessToken, "")
	if err != nil {
		t.Fatal(err)
	}

	roles, err := service.ListRoles(ctx, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tenantAdminRole Role
	for _, role := range roles {
		if role.Code == "tenant_admin" {
			tenantAdminRole = role
			break
		}
	}
	if tenantAdminRole.ID == "" {
		t.Fatal("tenant admin role not found")
	}
	customRole, err := service.CreateRole(ctx, tenantAdmin, tenant.ID, "Custom role", "", []string{"tenant.users.read"})
	if err != nil {
		t.Fatal(err)
	}

	createUser := func(username string, roleIDs []string) User {
		t.Helper()
		user, _, createErr := service.CreateUser(ctx, tenantAdmin, tenant.ID, username, username, "User-Password-123!", roleIDs)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return user
	}
	deleteProtectedAdmin := createUser("delete-protected-admin", []string{tenantAdminRole.ID})
	rolesProtectedAdmin := createUser("roles-protected-admin", []string{tenantAdminRole.ID})
	membershipProtectedAdmin := createUser("membership-protected-admin", []string{tenantAdminRole.ID, customRole.ID})
	assignableUser := createUser("assignable-user", nil)
	deletableUser := createUser("deletable-user", nil)

	if err = service.AssignUserRoles(ctx, tenantAdmin, tenant.ID, assignableUser.ID, []string{customRole.ID}); err != nil {
		t.Errorf("assign roles to non-admin user: %v", err)
	}
	if err = service.DeleteUser(ctx, tenantAdmin, tenant.ID, deletableUser.ID); err != nil {
		t.Errorf("delete non-admin user: %v", err)
	}
	if err = service.DeleteUser(ctx, tenantAdmin, tenant.ID, deleteProtectedAdmin.ID); !errors.Is(err, ErrProtectedResource) {
		t.Errorf("delete tenant admin err=%v, want ErrProtectedResource", err)
	}
	if err = service.AssignUserRoles(ctx, tenantAdmin, tenant.ID, rolesProtectedAdmin.ID, []string{customRole.ID}); !errors.Is(err, ErrProtectedResource) {
		t.Errorf("assign tenant admin roles err=%v, want ErrProtectedResource", err)
	}
	if err = service.ReplaceRoleUsers(ctx, tenantAdmin, tenant.ID, customRole.ID, []string{assignableUser.ID}, customRole.Version); !errors.Is(err, ErrProtectedResource) {
		t.Errorf("remove custom role from tenant admin err=%v, want ErrProtectedResource", err)
	}
	if err = service.ReplaceRoleUsers(ctx, tenantAdmin, tenant.ID, tenantAdminRole.ID, []string{tenantAdmin.User.ID, membershipProtectedAdmin.ID}, tenantAdminRole.Version); !errors.Is(err, ErrProtectedResource) {
		t.Errorf("replace tenant admin membership err=%v, want ErrProtectedResource", err)
	}
}

func containsMenu(menus []Menu, code string) bool {
	for _, menu := range menus {
		if menu.Code == code {
			return true
		}
	}
	return false
}
