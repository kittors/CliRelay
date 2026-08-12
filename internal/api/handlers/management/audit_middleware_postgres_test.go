package management

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
)

// TestManagementAuditRecordsOnlySecurityRelevantRequests pins the write policy
// against a real database, because the defect it replaces was invisible in unit
// tests: every non-2xx management response, reads included, became an audit row.
// A single panel tab polling GET /update/progress produced 33k rows in 18 hours
// on a live deployment.
func TestManagementAuditRecordsOnlySecurityRelevantRequests(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	db, service := newDisposableIdentityService(t, ctx, dsn, "audit_policy")

	h := NewHandler(nil, "", nil)
	h.identityService = service
	t.Cleanup(h.Close)

	// One operator may read system status, the other may not: the panel subscribes
	// every authenticated user to update progress, so both shapes occur in the wild.
	statusToken := seedPlatformUser(t, ctx, db, service, platformUserFixture{
		username:    "status-reader",
		password:    "Status-Password-123!",
		userID:      "dddddddd-dddd-dddd-dddd-ddddddddddd1",
		roleID:      "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1",
		permissions: []string{"system.status.read", "system.update.manage"},
	})
	noStatusToken := seedPlatformUser(t, ctx, db, service, platformUserFixture{
		username:    "status-blind",
		password:    "Blind-Password-123!",
		userID:      "dddddddd-dddd-dddd-dddd-ddddddddddd2",
		roleID:      "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee2",
		permissions: []string{"system.logs.read"},
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(h.Middleware())
	// Mirrors the deployed failure: the updater sidecar is not installed, so the
	// read proxies upstream and answers 502 forever.
	router.GET("/v0/management/update/progress", func(c *gin.Context) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_progress_failed"})
	})
	router.POST("/v0/management/update/apply", func(c *gin.Context) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "apply_failed"})
	})

	serve := func(method, path, token string) int {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "127.0.0.1:4321"
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	countAudit := func(action, resourceType, resourceID, result string) int64 {
		t.Helper()
		var count int64
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM audit_logs
			 WHERE action = ? AND resource_type = ? AND resource_id = ? AND result = ?
		`, action, resourceType, resourceID, result).Scan(&count); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		return count
	}

	// A read that fails upstream is an operational fault, not an audit event.
	for i := 0; i < 5; i++ {
		if code := serve(http.MethodGet, "/v0/management/update/progress", statusToken); code != http.StatusBadGateway {
			t.Fatalf("permitted read status = %d, want %d", code, http.StatusBadGateway)
		}
	}
	if got := countAudit("management.get", "update", "progress", auditResultFailed); got != 0 {
		t.Fatalf("failed reads wrote %d audit rows, want 0", got)
	}

	// A refused read is worth recording once; repeats collapse into that row.
	for i := 0; i < 20; i++ {
		if code := serve(http.MethodGet, "/v0/management/update/progress", noStatusToken); code != http.StatusForbidden {
			t.Fatalf("refused read status = %d, want %d", code, http.StatusForbidden)
		}
	}
	if got := countAudit("management.get", "update", "progress", auditResultDenied); got != 1 {
		t.Fatalf("refused reads wrote %d audit rows, want exactly 1", got)
	}

	// Writes are never collapsed: each attempt to change something is its own row.
	for i := 0; i < 3; i++ {
		if code := serve(http.MethodPost, "/v0/management/update/apply", statusToken); code != http.StatusInternalServerError {
			t.Fatalf("failed write status = %d, want %d", code, http.StatusInternalServerError)
		}
	}
	if got := countAudit("management.post", "update", "apply", auditResultFailed); got != 3 {
		t.Fatalf("failed writes wrote %d audit rows, want 3", got)
	}
	// A refused write is also recorded per attempt.
	for i := 0; i < 2; i++ {
		if code := serve(http.MethodPost, "/v0/management/update/apply", noStatusToken); code != http.StatusForbidden {
			t.Fatalf("refused write status = %d, want %d", code, http.StatusForbidden)
		}
	}
	if got := countAudit("management.post", "update", "apply", auditResultDenied); got != 2 {
		t.Fatalf("refused writes wrote %d audit rows, want 2", got)
	}
}

// TestManagementAuditRecordsDenialAccurately pins what a denial row says about
// itself. Both defects it covers were found in a live trail rather than in review:
//
//   - Every one of 1,623 refusals carried http.status 200, because the row was
//     written before the response and gin reports 200 until something sets a code.
//     The rows an audit reader reaches for first all claimed to have succeeded.
//   - A missing permission and a tenant-scope refusal are both 403 and produced
//     byte-identical rows, so the trail could not say which had happened — and the
//     two have opposite remedies.
func TestManagementAuditRecordsDenialAccurately(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	db, service := newDisposableIdentityService(t, ctx, dsn, "audit_denial")

	h := NewHandler(nil, "", nil)
	h.identityService = service
	t.Cleanup(h.Close)

	// Lacks system.status.read: refused by the permission check.
	noPermissionToken := seedPlatformUser(t, ctx, db, service, platformUserFixture{
		username:    "denial-no-permission",
		password:    "Denial-Password-123!",
		userID:      "dddddddd-dddd-dddd-dddd-ddddddddddf1",
		roleID:      "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeef1",
		permissions: []string{"system.logs.read"},
	})
	// Holds the permission but sits on a business tenant, so the same route is
	// refused one check later, for an entirely different reason.
	tenantScopedToken := seedTenantUser(t, ctx, db, service, tenantUserFixture{
		tenantID: "cccccccc-cccc-cccc-cccc-ccccccccccf1",
		platformUserFixture: platformUserFixture{
			username:    "denial-tenant-scope",
			password:    "Denial-Password-123!",
			userID:      "dddddddd-dddd-dddd-dddd-ddddddddddf2",
			roleID:      "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeef2",
			permissions: []string{"system.status.read"},
		},
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(h.Middleware())
	router.GET("/v0/management/update/progress", func(c *gin.Context) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_progress_failed"})
	})

	serve := func(token string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/update/progress", nil)
		req.RemoteAddr = "127.0.0.1:4321"
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	// The denial row for one actor, as JSON fields rather than a blob: a status that
	// disagrees with the response is exactly what this test exists to catch.
	readDenial := func(actorUserID string) (status int, reason string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `
			SELECT (changes->'http'->>'status')::int, COALESCE(changes->>'denial_reason', '')
			  FROM audit_logs
			 WHERE actor_user_id = ? AND result = ?
			 ORDER BY id DESC LIMIT 1
		`, actorUserID, auditResultDenied).Scan(&status, &reason); err != nil {
			t.Fatalf("read denial row for %s: %v", actorUserID, err)
		}
		return status, reason
	}

	if code := serve(noPermissionToken); code != http.StatusForbidden {
		t.Fatalf("missing-permission response = %d, want %d", code, http.StatusForbidden)
	}
	status, reason := readDenial("dddddddd-dddd-dddd-dddd-ddddddddddf1")
	if status != http.StatusForbidden {
		t.Fatalf("missing-permission row recorded status %d, want %d (the code the client actually received)", status, http.StatusForbidden)
	}
	if reason != string(denialPermission) {
		t.Fatalf("missing-permission row reason = %q, want %q", reason, denialPermission)
	}

	if code := serve(tenantScopedToken); code != http.StatusForbidden {
		t.Fatalf("tenant-scope response = %d, want %d", code, http.StatusForbidden)
	}
	status, reason = readDenial("dddddddd-dddd-dddd-dddd-ddddddddddf2")
	if status != http.StatusForbidden {
		t.Fatalf("tenant-scope row recorded status %d, want %d", status, http.StatusForbidden)
	}
	if reason != string(denialTenantScope) {
		t.Fatalf("tenant-scope row reason = %q, want %q — the two refusals must stay distinguishable", reason, denialTenantScope)
	}
}

type platformUserFixture struct {
	username, password, userID, roleID string
	permissions                        []string
}

type tenantUserFixture struct {
	platformUserFixture
	tenantID string
}

// seedTenantUser creates a business tenant and a user inside it. Such a user is
// refused on process-global routes by the tenant-scope check rather than by RBAC,
// which is the second of the two 403s this file distinguishes.
func seedTenantUser(t *testing.T, ctx context.Context, db *sql.DB, service *identity.Service, fx tenantUserFixture) string {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tenants (id, slug, name, type, status, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, 'standard', 'active', now() + interval '30 days', now(), now())
	`, fx.tenantID, "tenant-"+fx.username, fx.username+" tenant"); err != nil {
		t.Fatalf("seed tenant %s: %v", fx.username, err)
	}
	return seedUserInTenant(t, ctx, db, service, fx.tenantID, fx.platformUserFixture)
}

// seedPlatformUser creates a platform-scoped role with exactly the given
// permissions, a user holding it, and returns a fresh access token. CreateRole
// rejects platform-scoped permissions, so the fixture seeds them the way an
// operator provisioning a custom platform role would.
func seedPlatformUser(t *testing.T, ctx context.Context, db *sql.DB, service *identity.Service, fx platformUserFixture) string {
	t.Helper()
	return seedUserInTenant(t, ctx, db, service, identity.SystemTenantID, fx)
}

func seedUserInTenant(t *testing.T, ctx context.Context, db *sql.DB, service *identity.Service, tenantID string, fx platformUserFixture) string {
	t.Helper()
	hash, err := identity.HashPassword(fx.password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `
		INSERT INTO roles (id, tenant_id, code, name, description, scope, system_protected)
		VALUES (?, ?, ?, ?, '', 'platform', false)
	`, fx.roleID, tenantID, "platform_"+fx.username, fx.username+" role"); err != nil {
		t.Fatalf("seed role %s: %v", fx.username, err)
	}
	for _, perm := range fx.permissions {
		if _, err = db.ExecContext(ctx, `INSERT INTO role_permissions (role_id, permission_code) VALUES (?, ?)`, fx.roleID, perm); err != nil {
			t.Fatalf("seed role permission %s: %v", perm, err)
		}
	}
	if _, err = db.ExecContext(ctx, `
		INSERT INTO users (
		  id, tenant_id, username, username_normalized, display_name, password_hash,
		  status, must_change_password, password_changed_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 'active', false, now(), now(), now())
	`, fx.userID, tenantID, fx.username, fx.username, fx.username, hash); err != nil {
		t.Fatalf("seed user %s: %v", fx.username, err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO user_roles (user_id, role_id) VALUES (?, ?)`, fx.userID, fx.roleID); err != nil {
		t.Fatalf("seed user role %s: %v", fx.username, err)
	}
	login, err := service.Login(ctx, fx.username, fx.password, false, "audit-policy-test")
	if err != nil {
		t.Fatalf("login %s: %v", fx.username, err)
	}
	return login.AccessToken
}

// newDisposableIdentityService gives the test its own database so package tests
// that TRUNCATE the shared catalog cannot race this fixture.
func newDisposableIdentityService(t *testing.T, ctx context.Context, dsn, prefix string) (*sql.DB, *identity.Service) {
	t.Helper()
	adminDB, err := sql.Open("pgxq", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = adminDB.PingContext(ctx); err != nil {
		_ = adminDB.Close()
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err = adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create disposable db: %v", err)
	}
	// Registered first so LIFO cleanup runs: runtime close → drop → admin close.
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := adminDB.ExecContext(cleanupCtx, `
			SELECT pg_terminate_backend(pid)
			  FROM pg_stat_activity
			 WHERE datname = $1 AND pid <> pg_backend_pid()
		`, dbName); err != nil {
			t.Errorf("terminate connections on disposable db %s: %v", dbName, err)
		}
		if _, err := adminDB.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Errorf("drop disposable db %s: %v", dbName, err)
		}
		if err := adminDB.Close(); err != nil {
			t.Errorf("close admin db: %v", err)
		}
	})
	testDSN, err := replacePostgresDatabaseForTest(dsn, dbName)
	if err != nil {
		t.Fatal(err)
	}
	db, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: testDSN, MaxOpenConns: 4, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close runtime db: %v", err)
		}
	})
	service := identity.NewService(db)
	if err = service.Bootstrap(ctx, "Bootstrap-Password-123!"); err != nil {
		t.Fatal(err)
	}
	return db, service
}
