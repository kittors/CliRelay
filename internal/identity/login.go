package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

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
	// The password comparison stays ahead of the status checks so that
	// ErrInvalidCredentials is the only error that can mean "a secret was guessed
	// wrong". The API layer relies on exactly that to decide what consumes a
	// guessing budget; a locked or suspended account must not.
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		if _, failErr := s.registerLoginFailure(ctx, tenant.ID, user.ID); failErr != nil {
			return result, failErr
		}
		s.RecordAudit(ctx, AuditEvent{TenantID: tenant.ID, ActorKind: "system", Action: "auth.login", ResourceType: "user", ResourceID: user.ID, Result: "denied"})
		return result, ErrInvalidCredentials
	}
	now := time.Now()
	// Three separate judgements, deliberately not collapsed: an automatic cooldown
	// expires on its own, an administrative lock does not, and neither is allowed
	// to be healed by a successful password entry further down.
	if lockedUntil.Valid && lockedUntil.Time.After(now) {
		return result, ErrAccountLocked
	}
	if user.Status == "locked" {
		return result, ErrAccountLocked
	}
	if user.Status == "disabled" {
		return result, ErrAccountDisabled
	}
	tenant.Type = tenantType
	tenant.Status = tenantStatus
	if expiresAt.Valid {
		tenant.ExpiresAt = &expiresAt.Time
	}
	if err = validateTenant(tenant, now); err != nil {
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
	policy := s.sessionPolicy()
	accessTTL, refreshTTL := s.tenantSessionTTL(ctx, tenant.ID, remember)
	sessionID := uuid.NewString()
	issuedAt := time.Now().UTC()
	sessionExpiresAt := issuedAt.Add(accessTTL)
	refreshExpiresAt := issuedAt.Add(refreshTTL)
	absoluteExpiresAt := issuedAt.Add(policy.AbsoluteRefreshTTL)
	if refreshExpiresAt.After(absoluteExpiresAt) {
		refreshExpiresAt = absoluteExpiresAt
	}
	uaSum := sha256.Sum256([]byte(userAgent))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	// token_hash / refresh_token_hash keep mirroring the current head of the
	// session. They are no longer the authentication source (user_session_tokens
	// is), but they preserve the existing UNIQUE semantics, keep the management
	// session views working, and let a rollback to the previous binary still find
	// a usable token instead of logging everyone out.
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO user_sessions (id, user_id, tenant_id, token_hash, expires_at,
		  refresh_token_hash, refresh_expires_at, user_agent_hash,
		  remember_me, refresh_ttl_seconds, refresh_absolute_expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sessionID, user.ID, tenant.ID, hash, sessionExpiresAt, refreshHash, refreshExpiresAt,
		hex.EncodeToString(uaSum[:]), remember, int(refreshTTL.Seconds()), absoluteExpiresAt); err != nil {
		return result, err
	}
	if err = s.insertSessionTokens(ctx, tx, sessionTokenInsert{
		SessionID:   sessionID,
		AccessHash:  hash,
		AccessExp:   sessionExpiresAt,
		RefreshHash: refreshHash,
		RefreshExp:  refreshExpiresAt,
	}); err != nil {
		return result, err
	}
	// A successful login clears the automatic cooldown state only. status is
	// deliberately left alone: an administratively locked account already
	// returned above, so reaching here with status='locked' is impossible, and
	// re-adding the old "heal status on login" clause would let a future change
	// silently unlock accounts an administrator disabled.
	if _, err = tx.ExecContext(ctx, `
		UPDATE users SET last_login_at = now(), failed_login_count = 0, last_failed_login_at = NULL,
		  lock_stage = 0, locked_until = NULL, updated_at = now()
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
