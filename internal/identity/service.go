package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrAccountDisabled    = errors.New("account disabled")
	ErrAccountLocked      = errors.New("account locked")
	ErrTenantSuspended    = errors.New("tenant suspended")
	ErrTenantExpired      = errors.New("tenant expired")
	ErrSessionExpired     = errors.New("session expired")
	ErrSessionRevoked     = errors.New("session revoked")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrTenantScope        = errors.New("tenant scope forbidden")
	ErrVersionConflict    = errors.New("version conflict")
	ErrProtectedResource  = errors.New("protected resource")
	ErrValidation         = errors.New("validation failed")
)

const dummyPasswordHash = "$2a$10$7EqJtq98hPqEX7fNZaFWoO5fKvR2qv4V5BfQWqHkVq3VP7N5x5V7e"

type Service struct {
	db *sql.DB
}

var (
	defaultMu      sync.RWMutex
	defaultService *Service
)

func SetDefault(service *Service) {
	defaultMu.Lock()
	defaultService = service
	defaultMu.Unlock()
}

func Default() *Service {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultService
}

func NewService(db *sql.DB) *Service { return &Service{db: db} }

func NormalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", fmt.Errorf("%w: password must contain at least 12 characters", ErrValidation)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func generatedIdentifier(prefix string) string {
	return prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func validIdentifier(value string, maxLength int, allowAt bool) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxLength {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		case allowAt && r == '@':
		default:
			return false
		}
	}
	return true
}

func ensureActorTenantScope(actor Principal, tenantID string) error {
	if actor.PlatformAdmin || tenantID == actor.EffectiveTenant.ID {
		return nil
	}
	return ErrTenantScope
}

func ensureRolesDelegable(ctx context.Context, tx *sql.Tx, actor Principal, tenantID string, roleIDs []string) error {
	seen := make(map[string]struct{}, len(roleIDs))
	for _, roleID := range roleIDs {
		roleID = strings.TrimSpace(roleID)
		if roleID == "" {
			return fmt.Errorf("%w: role id is required", ErrValidation)
		}
		if _, ok := seen[roleID]; ok {
			continue
		}
		seen[roleID] = struct{}{}

		var roleTenant string
		if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM roles WHERE id = ?`, roleID).Scan(&roleTenant); err != nil {
			return err
		}
		if roleTenant != tenantID {
			return ErrTenantScope
		}
		if actor.PlatformAdmin {
			continue
		}

		rows, err := tx.QueryContext(ctx, `SELECT permission_code FROM role_permissions WHERE role_id = ?`, roleID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var permission string
			if err = rows.Scan(&permission); err != nil {
				_ = rows.Close()
				return err
			}
			if !actor.Has(permission) {
				_ = rows.Close()
				return ErrPermissionDenied
			}
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if err = rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Bootstrap(ctx context.Context, initialPassword string) error {
	if s == nil || s.db == nil {
		return errors.New("identity: database is not initialized")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO tenants (id, slug, name, type, status, created_at, updated_at)
		VALUES (?, 'system', 'System Administration', 'system', 'active', now(), now())
		ON CONFLICT (id) DO NOTHING
	`, SystemTenantID); err != nil {
		return fmt.Errorf("identity: seed system tenant: %w", err)
	}

	for i, permission := range PermissionCatalog {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO permissions (code, name, scope, resource, action, sensitive, sort_order, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, now())
			ON CONFLICT (code) DO UPDATE SET
			  name = EXCLUDED.name, scope = EXCLUDED.scope, resource = EXCLUDED.resource,
			  action = EXCLUDED.action, sensitive = EXCLUDED.sensitive,
			  sort_order = EXCLUDED.sort_order, updated_at = now()
		`, permission.Code, permission.Name, permission.Scope, permission.Resource, permission.Action, permission.Sensitive, i); err != nil {
			return fmt.Errorf("identity: seed permission %s: %w", permission.Code, err)
		}
	}

	if err = seedMenus(ctx, tx); err != nil {
		return err
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO roles (id, tenant_id, code, name, description, scope, system_protected)
		VALUES (?, ?, 'platform_super_admin', 'Administrator',
		        'Built-in administrator role with every platform and tenant permission.', 'platform', true)
		ON CONFLICT (id) DO UPDATE SET
		  tenant_id = EXCLUDED.tenant_id, code = EXCLUDED.code, name = EXCLUDED.name,
		  description = EXCLUDED.description, scope = EXCLUDED.scope,
		  system_protected = true, updated_at = now()
	`, SystemRoleID, SystemTenantID); err != nil {
		return fmt.Errorf("identity: seed system role: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO role_permissions (role_id, permission_code)
		SELECT ?, code FROM permissions
		ON CONFLICT DO NOTHING
	`, SystemRoleID); err != nil {
		return fmt.Errorf("identity: seed system role permissions: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO role_permissions (role_id, permission_code)
		SELECT r.id, p.code
		  FROM roles r CROSS JOIN permissions p
		 WHERE r.code = 'tenant_admin' AND r.scope = 'tenant' AND r.system_protected = true
		   AND p.scope = 'tenant'
		ON CONFLICT DO NOTHING
	`); err != nil {
		return fmt.Errorf("identity: seed tenant admin role permissions: %w", err)
	}

	var adminCount int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, SystemUserID).Scan(&adminCount); err != nil {
		return err
	}
	if adminCount == 0 {
		passwordHash := strings.TrimSpace(initialPassword)
		if !strings.HasPrefix(passwordHash, "$2") {
			passwordHash, err = HashPassword(passwordHash)
			if err != nil {
				return fmt.Errorf("identity: bootstrap admin password: %w", err)
			}
		}
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO users (
			  id, tenant_id, username, username_normalized, display_name, password_hash,
			  status, must_change_password, password_changed_at, created_at, updated_at
			) VALUES (?, ?, 'admin', 'admin', 'Administrator', ?, 'active', true, now(), now(), now())
		`, SystemUserID, SystemTenantID, passwordHash); err != nil {
			return fmt.Errorf("identity: seed admin: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET display_name = 'Super Administrator', updated_at = now() WHERE id = ?`, SystemUserID); err != nil {
		return fmt.Errorf("identity: update admin display name: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, SystemUserID, SystemRoleID); err != nil {
		return fmt.Errorf("identity: seed admin role: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func randomToken() (string, string, error) {
	return randomPrefixedToken("cps_")
}

func randomPrefixedToken(prefix string) (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token := prefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

func (s *Service) tenantSessionTTL(ctx context.Context, tenantID string, remember bool) (access, refresh time.Duration) {
	// Short access + long refresh. remember_me only keeps/extends refresh, never access.
	access, refresh = 12*time.Hour, 30*24*time.Hour
	if s == nil || s.db == nil {
		return
	}
	var a, r sql.NullInt64
	_ = s.db.QueryRowContext(ctx, `SELECT access_token_ttl_seconds, refresh_token_ttl_seconds FROM tenants WHERE id = ?`, tenantID).Scan(&a, &r)
	if a.Valid && a.Int64 > 0 {
		access = time.Duration(a.Int64) * time.Second
	}
	if r.Valid && r.Int64 > 0 {
		refresh = time.Duration(r.Int64) * time.Second
	}
	if remember && refresh < 30*24*time.Hour {
		refresh = 30 * 24 * time.Hour
	}
	return
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func (s *Service) Login(ctx context.Context, username, password string, remember bool, userAgent string) (LoginResult, error) {
	var result LoginResult
	if s == nil || s.db == nil {
		return result, ErrInvalidCredentials
	}
	normalized := NormalizeUsername(username)
	var user User
	var passwordHash, tenantStatus, tenantType string
	var tenant Tenant
	var expiresAt sql.NullTime
	var lastLogin sql.NullTime
	var lockedUntil sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.tenant_id, u.username, u.display_name, u.password_hash, u.status,
		       u.must_change_password, u.last_login_at, u.created_at, u.updated_at, u.version,
		       u.locked_until,
		       t.id, t.slug, t.name, t.type, t.status, t.expires_at, t.description,
		       t.created_at, t.updated_at, t.version
		  FROM users u JOIN tenants t ON t.id = u.tenant_id
		 WHERE u.username_normalized = ?
	`, normalized).Scan(
		&user.ID, &user.TenantID, &user.Username, &user.DisplayName, &passwordHash, &user.Status,
		&user.MustChangePassword, &lastLogin, &user.CreatedAt, &user.UpdatedAt, &user.Version,
		&lockedUntil,
		&tenant.ID, &tenant.Slug, &tenant.Name, &tenantType, &tenantStatus, &expiresAt, &tenant.Description,
		&tenant.CreatedAt, &tenant.UpdatedAt, &tenant.Version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))
		return result, ErrInvalidCredentials
	}
	if err != nil {
		return result, fmt.Errorf("identity: login lookup: %w", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		_, _ = s.db.ExecContext(ctx, `
			UPDATE users
			   SET failed_login_count = failed_login_count + 1,
			       status = CASE WHEN failed_login_count + 1 >= 10 THEN 'locked' ELSE status END,
			       locked_until = CASE WHEN failed_login_count + 1 >= 10 THEN now() + interval '15 minutes' ELSE locked_until END,
			       updated_at = now()
			 WHERE id = ?
		`, user.ID)
		s.RecordAudit(ctx, AuditEvent{TenantID: tenant.ID, ActorKind: "system", Action: "auth.login", ResourceType: "user", ResourceID: user.ID, Result: "denied"})
		return result, ErrInvalidCredentials
	}
	if user.Status == "disabled" {
		return result, ErrAccountDisabled
	}
	if user.Status == "locked" && (!lockedUntil.Valid || lockedUntil.Time.After(time.Now())) {
		return result, ErrAccountLocked
	}
	tenant.Type = tenantType
	tenant.Status = tenantStatus
	if expiresAt.Valid {
		tenant.ExpiresAt = &expiresAt.Time
	}
	if err = validateTenant(tenant, time.Now()); err != nil {
		return result, err
	}
	if lastLogin.Valid {
		user.LastLoginAt = &lastLogin.Time
	}

	token, hash, err := randomToken()
	if err != nil {
		return result, err
	}
	refreshToken, refreshHash, err := randomPrefixedToken("cpr_adm_")
	if err != nil {
		return result, err
	}
	accessTTL, refreshTTL := s.tenantSessionTTL(ctx, tenant.ID, remember)
	sessionID := uuid.NewString()
	now := time.Now().UTC()
	sessionExpiresAt := now.Add(accessTTL)
	refreshExpiresAt := now.Add(refreshTTL)
	uaSum := sha256.Sum256([]byte(userAgent))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO user_sessions (id, user_id, tenant_id, token_hash, expires_at, refresh_token_hash, refresh_expires_at, user_agent_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, sessionID, user.ID, tenant.ID, hash, sessionExpiresAt, refreshHash, refreshExpiresAt, hex.EncodeToString(uaSum[:])); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE users SET last_login_at = now(), failed_login_count = 0, locked_until = NULL,
		  status = CASE WHEN status = 'locked' THEN 'active' ELSE status END, updated_at = now()
		WHERE id = ?
	`, user.ID); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}

	principal, err := s.loadPrincipal(ctx, user.ID, sessionID, sessionExpiresAt, tenant.ID)
	if err != nil {
		return result, err
	}
	result = LoginResult{
		AccessToken: token, RefreshToken: refreshToken, TokenType: "Bearer",
		ExpiresAt: sessionExpiresAt, RefreshExpiresAt: refreshExpiresAt, Principal: principal,
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenant.ID, ActorKind: "user_session", ActorUserID: user.ID, ActorSessionID: sessionID, Action: "auth.login", ResourceType: "session", ResourceID: sessionID, Result: "success"})
	return result, nil
}

func (s *Service) RefreshSession(ctx context.Context, refreshToken string) (LoginResult, error) {
	var result LoginResult
	if s == nil || s.db == nil || !strings.HasPrefix(strings.TrimSpace(refreshToken), "cpr_adm_") {
		return result, ErrSessionRevoked
	}
	var sessionID, userID, tenantID string
	var refreshExp time.Time
	var revoked sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, tenant_id, refresh_expires_at, revoked_at
		  FROM user_sessions WHERE refresh_token_hash = ?
	`, tokenHash(refreshToken)).Scan(&sessionID, &userID, &tenantID, &refreshExp, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrSessionRevoked
	}
	if err != nil {
		return result, err
	}
	if revoked.Valid || !refreshExp.After(time.Now()) {
		return result, ErrSessionExpired
	}
	accessTTL, refreshTTL := s.tenantSessionTTL(ctx, tenantID, false)
	accessPlain, accessHash, err := randomToken()
	if err != nil {
		return result, err
	}
	refreshPlain, refreshHash, err := randomPrefixedToken("cpr_adm_")
	if err != nil {
		return result, err
	}
	now := time.Now().UTC()
	accessExp := now.Add(accessTTL)
	newRefreshExp := now.Add(refreshTTL)
	// Atomic consume: only one concurrent refresh of the same refresh token wins.
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_sessions SET token_hash = ?, expires_at = ?, refresh_token_hash = ?, refresh_expires_at = ?, last_seen_at = now()
		WHERE id = ? AND refresh_token_hash = ? AND revoked_at IS NULL AND refresh_expires_at > now()
	`, accessHash, accessExp, refreshHash, newRefreshExp, sessionID, tokenHash(refreshToken))
	if err != nil {
		return result, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return result, ErrSessionRevoked
	}
	principal, err := s.loadPrincipal(ctx, userID, sessionID, accessExp, tenantID)
	if err != nil {
		return result, err
	}
	return LoginResult{
		AccessToken: accessPlain, RefreshToken: refreshPlain, TokenType: "Bearer",
		ExpiresAt: accessExp, RefreshExpiresAt: newRefreshExp, Principal: principal,
	}, nil
}

func validateTenant(tenant Tenant, now time.Time) error {
	if tenant.Status != "active" {
		return ErrTenantSuspended
	}
	if tenant.Type != "system" && tenant.ExpiresAt != nil && !tenant.ExpiresAt.After(now) {
		return ErrTenantExpired
	}
	return nil
}

func (s *Service) Authenticate(ctx context.Context, token, effectiveTenantID string) (Principal, error) {
	var principal Principal
	var userID, sessionID, homeTenantID string
	var expiresAt time.Time
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, tenant_id, expires_at, revoked_at
		  FROM user_sessions WHERE token_hash = ?
	`, tokenHash(token)).Scan(&sessionID, &userID, &homeTenantID, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return principal, ErrSessionRevoked
	}
	if err != nil {
		return principal, err
	}
	if revokedAt.Valid {
		return principal, ErrSessionRevoked
	}
	if !expiresAt.After(time.Now()) {
		return principal, ErrSessionExpired
	}
	principal, err = s.loadPrincipal(ctx, userID, sessionID, expiresAt, homeTenantID)
	if err != nil {
		return principal, err
	}
	if effectiveTenantID != "" && effectiveTenantID != homeTenantID {
		if !principal.Has("platform.tenants.switch") {
			return Principal{}, ErrTenantScope
		}
		tenant, tenantErr := s.GetTenant(ctx, effectiveTenantID)
		if tenantErr != nil {
			return Principal{}, tenantErr
		}
		if tenantErr = validateTenant(tenant, time.Now()); tenantErr != nil {
			if principal.PlatformAdmin {
				return Principal{}, ErrTenantScope
			}
			return Principal{}, tenantErr
		}
		principal.EffectiveTenant = tenant
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE user_sessions SET last_seen_at = now() WHERE id = ?`, sessionID)
	return principal, nil
}

func (s *Service) loadPrincipal(ctx context.Context, userID, sessionID string, expiresAt time.Time, homeTenantID string) (Principal, error) {
	var principal Principal
	var lastLogin sql.NullTime
	var tenantExpires sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.tenant_id, u.username, u.display_name, u.status, u.must_change_password,
		       u.last_login_at, u.created_at, u.updated_at, u.version,
		       t.id, t.slug, t.name, t.type, t.status, t.expires_at, t.description,
		       t.created_at, t.updated_at, t.version
		  FROM users u JOIN tenants t ON t.id = u.tenant_id WHERE u.id = ?
	`, userID).Scan(
		&principal.User.ID, &principal.User.TenantID, &principal.User.Username, &principal.User.DisplayName,
		&principal.User.Status, &principal.User.MustChangePassword, &lastLogin,
		&principal.User.CreatedAt, &principal.User.UpdatedAt, &principal.User.Version,
		&principal.HomeTenant.ID, &principal.HomeTenant.Slug, &principal.HomeTenant.Name,
		&principal.HomeTenant.Type, &principal.HomeTenant.Status, &tenantExpires,
		&principal.HomeTenant.Description, &principal.HomeTenant.CreatedAt,
		&principal.HomeTenant.UpdatedAt, &principal.HomeTenant.Version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return principal, ErrSessionRevoked
	}
	if err != nil {
		return principal, err
	}
	if principal.User.Status == "disabled" {
		return principal, ErrAccountDisabled
	}
	if principal.User.Status == "locked" {
		return principal, ErrAccountLocked
	}
	if lastLogin.Valid {
		principal.User.LastLoginAt = &lastLogin.Time
	}
	if tenantExpires.Valid {
		principal.HomeTenant.ExpiresAt = &tenantExpires.Time
	}
	if err = validateTenant(principal.HomeTenant, time.Now()); err != nil {
		return principal, err
	}
	principal.HomeTenant.EffectiveStatus = "active"
	principal.EffectiveTenant = principal.HomeTenant
	principal.Kind = "user_session"
	principal.SessionID = sessionID
	principal.SessionExpiresAt = &expiresAt
	principal.Permissions = make(map[string]bool)

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.tenant_id, r.code, r.name, r.description, r.scope,
		       r.system_protected, r.version, p.code
		  FROM user_roles ur
		  JOIN roles r ON r.id = ur.role_id
		  LEFT JOIN role_permissions rp ON rp.role_id = r.id
		  LEFT JOIN permissions p ON p.code = rp.permission_code
		 WHERE ur.user_id = ?
		 ORDER BY r.name, p.code
	`, userID)
	if err != nil {
		return principal, err
	}
	defer rows.Close()
	roleByID := make(map[string]*Role)
	roleOrder := make([]string, 0)
	for rows.Next() {
		var role Role
		var permission sql.NullString
		if err = rows.Scan(&role.ID, &role.TenantID, &role.Code, &role.Name, &role.Description,
			&role.Scope, &role.SystemProtected, &role.Version, &permission); err != nil {
			return principal, err
		}
		stored := roleByID[role.ID]
		if stored == nil {
			role.Permissions = []string{}
			roleByID[role.ID] = &role
			roleOrder = append(roleOrder, role.ID)
			stored = &role
		}
		if permission.Valid {
			stored.Permissions = append(stored.Permissions, permission.String)
			principal.Permissions[permission.String] = true
		}
		if role.Code == "platform_super_admin" && role.Scope == "platform" {
			principal.PlatformAdmin = true
		}
	}
	if err = rows.Err(); err != nil {
		return principal, err
	}
	for _, roleID := range roleOrder {
		principal.Roles = append(principal.Roles, *roleByID[roleID])
	}
	for permission := range principal.Permissions {
		principal.PermissionList = append(principal.PermissionList, permission)
	}
	sort.Strings(principal.PermissionList)
	principal.Menus, err = s.ListPrincipalMenus(ctx, principal)
	if err != nil {
		return Principal{}, err
	}
	return principal, nil
}

func (s *Service) Logout(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = now(), revoke_reason = 'logout' WHERE id = ? AND revoked_at IS NULL`, sessionID)
	return err
}

func (s *Service) ChangePassword(ctx context.Context, principal Principal, currentPassword, newPassword string) error {
	if principal.Kind != "user_session" {
		return ErrPermissionDenied
	}
	var currentHash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, principal.User.ID).Scan(&currentHash); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(currentPassword)) != nil {
		return ErrInvalidCredentials
	}
	newHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, must_change_password = false, password_changed_at = now(), updated_at = now(), version = version + 1 WHERE id = ?`, newHash, principal.User.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = now(), revoke_reason = 'password_changed' WHERE user_id = ? AND id <> ? AND revoked_at IS NULL`, principal.User.ID, principal.SessionID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: principal.HomeTenant.ID, ActorKind: principal.Kind, ActorUserID: principal.User.ID, ActorSessionID: principal.SessionID, Action: "auth.password.change", ResourceType: "user", ResourceID: principal.User.ID, Result: "success"})
	return nil
}

func (s *Service) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var tenant Tenant
	var expires sql.NullTime
	var accessTTL, refreshTTL sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, type, status, expires_at, description,
		       COALESCE(access_token_ttl_seconds, 43200), COALESCE(refresh_token_ttl_seconds, 2592000),
		       created_at, updated_at, version
		  FROM tenants WHERE id = ?`, id).Scan(
		&tenant.ID, &tenant.Slug, &tenant.Name, &tenant.Type, &tenant.Status, &expires,
		&tenant.Description, &accessTTL, &refreshTTL, &tenant.CreatedAt, &tenant.UpdatedAt, &tenant.Version)
	if err != nil {
		// Fallback for DBs that have not yet applied TTL columns.
		err2 := s.db.QueryRowContext(ctx, `SELECT id, slug, name, type, status, expires_at, description, created_at, updated_at, version FROM tenants WHERE id = ?`, id).Scan(
			&tenant.ID, &tenant.Slug, &tenant.Name, &tenant.Type, &tenant.Status, &expires,
			&tenant.Description, &tenant.CreatedAt, &tenant.UpdatedAt, &tenant.Version)
		if err2 != nil {
			return tenant, err
		}
		tenant.AccessTokenTTLSeconds = 43200
		tenant.RefreshTokenTTLSeconds = 2592000
	} else {
		if accessTTL.Valid {
			tenant.AccessTokenTTLSeconds = int(accessTTL.Int64)
		} else {
			tenant.AccessTokenTTLSeconds = 43200
		}
		if refreshTTL.Valid {
			tenant.RefreshTokenTTLSeconds = int(refreshTTL.Int64)
		} else {
			tenant.RefreshTokenTTLSeconds = 2592000
		}
	}
	if expires.Valid {
		tenant.ExpiresAt = &expires.Time
	}
	tenant.EffectiveStatus = tenant.Status
	if tenant.Status == "active" && tenant.Type != "system" && tenant.ExpiresAt != nil && !tenant.ExpiresAt.After(time.Now()) {
		tenant.EffectiveStatus = "expired"
	}
	return tenant, nil
}

func (s *Service) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tenants ORDER BY type, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tenants []Tenant
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		tenant, e := s.GetTenant(ctx, id)
		if e != nil {
			return nil, e
		}
		tenants = append(tenants, tenant)
	}
	return tenants, rows.Err()
}

type CreateTenantInput struct {
	Name, Description, AdminUsername, AdminDisplayName, AdminPassword string
	ExpiresAt                                                         time.Time
}

func (s *Service) CreateTenant(ctx context.Context, actor Principal, input CreateTenantInput) (Tenant, User, error) {
	var tenant Tenant
	var admin User
	if !actor.Has("platform.tenants.create") {
		return tenant, admin, ErrPermissionDenied
	}
	slug := generatedIdentifier("tenant-")
	name := strings.TrimSpace(input.Name)
	adminUsername := NormalizeUsername(input.AdminUsername)
	adminDisplayName := strings.TrimSpace(input.AdminDisplayName)
	if name == "" || len(name) > 128 || len(strings.TrimSpace(input.Description)) > 1000 || !validIdentifier(adminUsername, 128, true) || adminDisplayName == "" || len(adminDisplayName) > 128 || input.ExpiresAt.IsZero() || !input.ExpiresAt.After(time.Now()) {
		return tenant, admin, fmt.Errorf("%w: invalid tenant input", ErrValidation)
	}
	passwordHash, err := HashPassword(input.AdminPassword)
	if err != nil {
		return tenant, admin, err
	}
	tenantID, roleID, userID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tenant, admin, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO tenants (id, slug, name, type, status, expires_at, description, created_by) VALUES (?, ?, ?, 'standard', 'active', ?, ?, ?)`, tenantID, slug, name, input.ExpiresAt.UTC(), strings.TrimSpace(input.Description), actor.User.ID); err != nil {
		return tenant, admin, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO roles (id, tenant_id, code, name, description, scope, system_protected) VALUES (?, ?, 'tenant_admin', 'Tenant Administrator', 'Built-in tenant administrator role.', 'tenant', true)`, roleID, tenantID); err != nil {
		return tenant, admin, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO role_permissions (role_id, permission_code, created_by) SELECT ?, code, ? FROM permissions WHERE scope = 'tenant'`, roleID, actor.User.ID); err != nil {
		return tenant, admin, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password, created_by) VALUES (?, ?, ?, ?, ?, ?, true, ?)`, userID, tenantID, strings.TrimSpace(input.AdminUsername), adminUsername, adminDisplayName, passwordHash, actor.User.ID); err != nil {
		return tenant, admin, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role_id, created_by) VALUES (?, ?, ?)`, userID, roleID, actor.User.ID); err != nil {
		return tenant, admin, err
	}
	if err = tx.Commit(); err != nil {
		return tenant, admin, err
	}
	tenant, err = s.GetTenant(ctx, tenantID)
	if err != nil {
		return tenant, admin, err
	}
	users, err := s.ListUsers(ctx, tenantID)
	if err != nil {
		return tenant, admin, err
	}
	for _, item := range users {
		if item.ID == userID {
			admin = item
			break
		}
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "tenant.create", ResourceType: "tenant", ResourceID: tenantID, Result: "success"})
	return tenant, admin, nil
}

func (s *Service) UpdateTenant(ctx context.Context, actor Principal, id, status string, expiresAt *time.Time, version int64) (Tenant, error) {
	return s.UpdateTenantDetails(ctx, actor, id, nil, nil, status, expiresAt, nil, nil, version)
}

func (s *Service) UpdateTenantDetails(ctx context.Context, actor Principal, id string, name, description *string, status string, expiresAt *time.Time, accessTTL, refreshTTL *int, version int64) (Tenant, error) {
	if !actor.Has("platform.tenants.update") {
		return Tenant{}, ErrPermissionDenied
	}
	if id == SystemTenantID {
		return Tenant{}, ErrProtectedResource
	}
	status = strings.TrimSpace(status)
	if status != "" && status != "active" && status != "suspended" && status != "disabled" {
		return Tenant{}, fmt.Errorf("%w: invalid tenant status", ErrValidation)
	}
	nameValue := ""
	if name != nil {
		nameValue = strings.TrimSpace(*name)
		if nameValue == "" || len(nameValue) > 128 {
			return Tenant{}, fmt.Errorf("%w: invalid tenant name", ErrValidation)
		}
	}
	var descriptionValue any
	if description != nil {
		trimmed := strings.TrimSpace(*description)
		if len(trimmed) > 1000 {
			return Tenant{}, fmt.Errorf("%w: tenant description is too long", ErrValidation)
		}
		descriptionValue = trimmed
	}
	if accessTTL != nil && (*accessTTL < 60 || *accessTTL > 30*24*3600) {
		return Tenant{}, fmt.Errorf("%w: access_token_ttl_seconds out of range", ErrValidation)
	}
	if refreshTTL != nil && (*refreshTTL < 300 || *refreshTTL > 365*24*3600) {
		return Tenant{}, fmt.Errorf("%w: refresh_token_ttl_seconds out of range", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tenant{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE tenants SET name = COALESCE(NULLIF(?, ''), name), description = COALESCE(?, description), status = COALESCE(NULLIF(?, ''), status), expires_at = COALESCE(?, expires_at),
		access_token_ttl_seconds = COALESCE(?, access_token_ttl_seconds),
		refresh_token_ttl_seconds = COALESCE(?, refresh_token_ttl_seconds),
		updated_at = now(), version = version + 1 WHERE id = ? AND version = ?`,
		nameValue, descriptionValue, status, expiresAt, accessTTL, refreshTTL, id, version)
	if err != nil {
		return Tenant{}, err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return Tenant{}, ErrVersionConflict
	}
	if status == "suspended" || status == "disabled" {
		if _, err = tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = now(), revoke_reason = 'tenant_status_changed' WHERE tenant_id = ? AND revoked_at IS NULL`, id); err != nil {
			return Tenant{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Tenant{}, err
	}
	tenant, err := s.GetTenant(ctx, id)
	if err == nil {
		s.RecordAudit(ctx, AuditEvent{TenantID: id, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "tenant.update", ResourceType: "tenant", ResourceID: id, Result: "success"})
	}
	return tenant, err
}

func (s *Service) ListUsers(ctx context.Context, tenantID string) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, username, display_name, status, must_change_password, last_login_at, created_at, updated_at, version FROM users WHERE tenant_id = ? ORDER BY username_normalized`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var user User
		var last sql.NullTime
		if err = rows.Scan(&user.ID, &user.TenantID, &user.Username, &user.DisplayName, &user.Status, &user.MustChangePassword, &last, &user.CreatedAt, &user.UpdatedAt, &user.Version); err != nil {
			return nil, err
		}
		if last.Valid {
			user.LastLoginAt = &last.Time
		}
		roleRows, roleErr := s.db.QueryContext(ctx, `SELECT r.id,r.code FROM roles r JOIN user_roles ur ON ur.role_id=r.id WHERE ur.user_id=? ORDER BY r.name`, user.ID)
		if roleErr != nil {
			return nil, roleErr
		}
		for roleRows.Next() {
			var roleID, roleCode string
			if roleRows.Scan(&roleID, &roleCode) == nil {
				user.RoleIDs = append(user.RoleIDs, roleID)
				user.RoleCodes = append(user.RoleCodes, roleCode)
			}
		}
		_ = roleRows.Close()
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Service) CreateUser(ctx context.Context, actor Principal, tenantID, username, displayName, password string, roleIDs []string) (User, error) {
	if !actor.Has("tenant.users.create") && !actor.Has("platform.users.manage") {
		return User{}, ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return User{}, err
	}
	if len(roleIDs) > 0 && !actor.Has("tenant.users.assign_roles") && !actor.Has("platform.users.manage") {
		return User{}, ErrPermissionDenied
	}
	normalizedUsername := NormalizeUsername(username)
	displayName = strings.TrimSpace(displayName)
	if !validIdentifier(normalizedUsername, 128, true) || displayName == "" || len(displayName) > 128 {
		return User{}, fmt.Errorf("%w: invalid user input", ErrValidation)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	userID := uuid.NewString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password, created_by) VALUES (?, ?, ?, ?, ?, ?, true, ?)`, userID, tenantID, strings.TrimSpace(username), normalizedUsername, displayName, hash, actor.User.ID); err != nil {
		return User{}, err
	}
	if err = ensureRolesDelegable(ctx, tx, actor, tenantID, roleIDs); err != nil {
		return User{}, err
	}
	for _, roleID := range roleIDs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role_id, created_by) VALUES (?, ?, ?)`, userID, roleID, actor.User.ID); err != nil {
			return User{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "user.create", ResourceType: "user", ResourceID: userID, Result: "success"})
	users, err := s.ListUsers(ctx, tenantID)
	if err != nil {
		return User{}, err
	}
	for _, user := range users {
		if user.ID == userID {
			return user, nil
		}
	}
	return User{}, sql.ErrNoRows
}

func (s *Service) ResetPassword(ctx context.Context, actor Principal, tenantID, userID, password string) error {
	if !actor.Has("tenant.users.reset_password") && !actor.Has("platform.users.manage") {
		return ErrPermissionDenied
	}
	if userID == actor.User.ID {
		return ErrProtectedResource
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, must_change_password = true, password_changed_at = now(), updated_at = now(), version = version + 1 WHERE id = ? AND tenant_id = ?`, hash, userID, tenantID)
	if err != nil {
		return err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = now(), revoke_reason = 'password_reset' WHERE user_id = ? AND revoked_at IS NULL`, userID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "user.password.reset", ResourceType: "user", ResourceID: userID, Result: "success"})
	return nil
}

func (s *Service) ListRoles(ctx context.Context, tenantID string) ([]Role, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.tenant_id, r.code, r.name, r.description, r.scope, r.system_protected, r.version, p.code FROM roles r LEFT JOIN role_permissions rp ON rp.role_id = r.id LEFT JOIN permissions p ON p.code = rp.permission_code WHERE r.tenant_id = ? ORDER BY r.name, p.code`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]*Role{}
	var order []string
	for rows.Next() {
		var role Role
		var perm sql.NullString
		if err = rows.Scan(&role.ID, &role.TenantID, &role.Code, &role.Name, &role.Description, &role.Scope, &role.SystemProtected, &role.Version, &perm); err != nil {
			return nil, err
		}
		stored := byID[role.ID]
		if stored == nil {
			role.Permissions = []string{}
			byID[role.ID] = &role
			order = append(order, role.ID)
			stored = &role
		}
		if perm.Valid {
			stored.Permissions = append(stored.Permissions, perm.String)
		}
	}
	roles := make([]Role, 0, len(order))
	for _, id := range order {
		roles = append(roles, *byID[id])
	}
	return roles, rows.Err()
}

func (s *Service) ListPermissions(ctx context.Context) ([]PermissionSeed, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT code, name, scope, resource, action, sensitive FROM permissions ORDER BY sort_order, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []PermissionSeed
	for rows.Next() {
		var item PermissionSeed
		if err = rows.Scan(&item.Code, &item.Name, &item.Scope, &item.Resource, &item.Action, &item.Sensitive); err != nil {
			return nil, err
		}
		item.MenuCode = menuCodeForPermission(item)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) CreateRole(ctx context.Context, actor Principal, tenantID, name, description string, permissions []string) (Role, error) {
	if !actor.Has("tenant.roles.create") && !actor.Has("platform.roles.manage") {
		return Role{}, ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return Role{}, err
	}
	code := generatedIdentifier("role_")
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if !validIdentifier(code, 64, false) || name == "" || len(name) > 128 || len(description) > 1000 {
		return Role{}, fmt.Errorf("%w: invalid role input", ErrValidation)
	}
	roleID := uuid.NewString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Role{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO roles (id,tenant_id,code,name,description,scope) VALUES (?,?,?,?,?,'tenant')`, roleID, tenantID, code, name, description); err != nil {
		return Role{}, err
	}
	for _, permission := range permissions {
		var scope string
		if err = tx.QueryRowContext(ctx, `SELECT scope FROM permissions WHERE code=?`, permission).Scan(&scope); err != nil {
			return Role{}, err
		}
		if scope != "tenant" {
			return Role{}, ErrPermissionDenied
		}
		if !actor.Has(permission) && !actor.PlatformAdmin {
			return Role{}, ErrPermissionDenied
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO role_permissions(role_id,permission_code,created_by)VALUES(?,?,?)`, roleID, permission, actor.User.ID); err != nil {
			return Role{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Role{}, err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "role.create", ResourceType: "role", ResourceID: roleID, Result: "success"})
	roles, err := s.ListRoles(ctx, tenantID)
	if err != nil {
		return Role{}, err
	}
	for _, role := range roles {
		if role.ID == roleID {
			return role, nil
		}
	}
	return Role{}, sql.ErrNoRows
}

func (s *Service) ReplaceRolePermissions(ctx context.Context, actor Principal, tenantID, roleID string, permissions []string, version int64) (Role, error) {
	if !actor.Has("tenant.roles.update") && !actor.Has("platform.roles.manage") {
		return Role{}, ErrPermissionDenied
	}
	if err := ensureActorTenantScope(actor, tenantID); err != nil {
		return Role{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Role{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var protected bool
	var currentVersion int64
	if err = tx.QueryRowContext(ctx, `SELECT system_protected,version FROM roles WHERE id=? AND tenant_id=? FOR UPDATE`, roleID, tenantID).Scan(&protected, &currentVersion); err != nil {
		return Role{}, err
	}
	if protected {
		return Role{}, ErrProtectedResource
	}
	if currentVersion != version {
		return Role{}, ErrVersionConflict
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM role_permissions WHERE role_id=?`, roleID); err != nil {
		return Role{}, err
	}
	for _, permission := range permissions {
		var scope string
		if err = tx.QueryRowContext(ctx, `SELECT scope FROM permissions WHERE code=?`, permission).Scan(&scope); err != nil {
			return Role{}, err
		}
		if scope != "tenant" || (!actor.Has(permission) && !actor.PlatformAdmin) {
			return Role{}, ErrPermissionDenied
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO role_permissions(role_id,permission_code,created_by)VALUES(?,?,?)`, roleID, permission, actor.User.ID); err != nil {
			return Role{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE roles SET updated_at=now(),version=version+1 WHERE id=?`, roleID); err != nil {
		return Role{}, err
	}
	if err = tx.Commit(); err != nil {
		return Role{}, err
	}
	s.RecordAudit(ctx, AuditEvent{TenantID: tenantID, ActorKind: actor.Kind, ActorUserID: actor.User.ID, ActorSessionID: actor.SessionID, Action: "role.permissions.replace", ResourceType: "role", ResourceID: roleID, Result: "success"})
	roles, err := s.ListRoles(ctx, tenantID)
	if err != nil {
		return Role{}, err
	}
	for _, role := range roles {
		if role.ID == roleID {
			return role, nil
		}
	}
	return Role{}, sql.ErrNoRows
}
