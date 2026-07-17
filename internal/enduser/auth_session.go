package enduser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func (s *Service) Login(ctx context.Context, username, password, userAgent string) (LoginResult, error) {
	var result LoginResult
	if s == nil || s.db == nil {
		return result, ErrInvalidCredentials
	}
	normalized := NormalizeUsername(username)
	var u User
	var passwordHash string
	var lastLogin, lockedUntil sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, username, display_name, password_hash, status, must_change_password,
			last_login_at, failed_login_count, lock_stage, locked_until, created_at, updated_at, version
		FROM end_users WHERE username_normalized = ?
	`, normalized).Scan(
		&u.ID, &u.TenantID, &u.Username, &u.DisplayName, &passwordHash, &u.Status, &u.MustChangePassword,
		&lastLogin, &u.FailedLoginCount, &u.LockStage, &lockedUntil, &u.CreatedAt, &u.UpdatedAt, &u.Version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))
		return result, ErrInvalidCredentials
	}
	if err != nil {
		return result, err
	}
	if lastLogin.Valid {
		t := lastLogin.Time
		u.LastLoginAt = &t
	}
	if lockedUntil.Valid {
		t := lockedUntil.Time
		u.LockedUntil = &t
	}
	if u.Status == "disabled" {
		return result, ErrAccountDisabled
	}
	if u.Status == "locked" {
		return result, ErrAccountLocked
	}
	if u.LockedUntil != nil && u.LockedUntil.After(time.Now()) {
		return result, ErrLoginCooldowned
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		// Atomic increment to avoid lost updates under concurrent failures.
		var newCount int
		_ = s.db.QueryRowContext(ctx, `
			UPDATE end_users SET failed_login_count = failed_login_count + 1, updated_at = now()
			WHERE id = ? RETURNING failed_login_count
		`, u.ID).Scan(&newCount)
		if newCount == 0 {
			newCount = u.FailedLoginCount + 1
		}
		stage, wait, permanent, apply := lockPenalty(newCount)
		if apply && permanent {
			_, _ = s.db.ExecContext(ctx, `
				UPDATE end_users SET lock_stage = ?, status = 'locked', locked_until = NULL, updated_at = now()
				WHERE id = ?
			`, stage, u.ID)
			return result, ErrAccountLocked
		}
		if apply && wait > 0 {
			until := time.Now().UTC().Add(wait)
			_, _ = s.db.ExecContext(ctx, `
				UPDATE end_users SET lock_stage = ?, locked_until = ?, updated_at = now()
				WHERE id = ?
			`, stage, until, u.ID)
			return result, ErrLoginCooldowned
		}
		return result, ErrInvalidCredentials
	}
	if err := s.ensureTenantActive(ctx, u.TenantID); err != nil {
		return result, err
	}

	accessTTL, refreshTTL := s.tenantTTL(ctx, u.TenantID)
	accessPlain, accessHash, err := randomPrefixedToken(accessPrefix)
	if err != nil {
		return result, err
	}
	refreshPlain, refreshHash, err := randomPrefixedToken(refreshPrefix)
	if err != nil {
		return result, err
	}
	sessionID := uuid.NewString()
	now := time.Now().UTC()
	accessExp := now.Add(accessTTL)
	refreshExp := now.Add(refreshTTL)
	uaSum := sha256.Sum256([]byte(userAgent))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO end_user_sessions (id, end_user_id, tenant_id, access_token_hash, refresh_token_hash, access_expires_at, refresh_expires_at, user_agent_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, sessionID, u.ID, u.TenantID, accessHash, refreshHash, accessExp, refreshExp, hex.EncodeToString(uaSum[:])); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE end_users SET last_login_at = now(), failed_login_count = 0, lock_stage = 0, locked_until = NULL, updated_at = now()
		WHERE id = ?
	`, u.ID); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	u.FailedLoginCount = 0
	u.LockStage = 0
	u.LockedUntil = nil
	nowLogin := now
	u.LastLoginAt = &nowLogin
	return LoginResult{
		AccessToken: accessPlain, RefreshToken: refreshPlain, TokenType: "Bearer",
		ExpiresAt: accessExp, RefreshExpiresAt: refreshExp, User: u, MustChangePassword: u.MustChangePassword,
	}, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (User, string, error) {
	var zero User
	if !strings.HasPrefix(token, accessPrefix) {
		return zero, "", ErrSessionRevoked
	}
	var sessionID, userID, tenantID string
	var accessExp time.Time
	var revoked sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, end_user_id, tenant_id, access_expires_at, revoked_at
		FROM end_user_sessions WHERE access_token_hash = ?
	`, tokenHash(token)).Scan(&sessionID, &userID, &tenantID, &accessExp, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, "", ErrSessionRevoked
	}
	if err != nil {
		return zero, "", err
	}
	if revoked.Valid {
		return zero, "", ErrSessionRevoked
	}
	if !accessExp.After(time.Now()) {
		return zero, "", ErrSessionExpired
	}
	u, err := s.GetUser(ctx, tenantID, userID)
	if err != nil {
		return zero, "", err
	}
	if u.Status == "disabled" {
		return zero, "", ErrAccountDisabled
	}
	if u.Status == "locked" {
		return zero, "", ErrAccountLocked
	}
	if err := s.ensureTenantActive(ctx, tenantID); err != nil {
		return zero, "", err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE end_user_sessions SET last_seen_at = now() WHERE id = ?`, sessionID)
	return u, sessionID, nil
}

func (s *Service) Refresh(ctx context.Context, refreshToken, userAgent string) (LoginResult, error) {
	var result LoginResult
	if !strings.HasPrefix(refreshToken, refreshPrefix) {
		return result, ErrSessionRevoked
	}
	var sessionID, userID, tenantID string
	var refreshExp time.Time
	var revoked sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, end_user_id, tenant_id, refresh_expires_at, revoked_at
		FROM end_user_sessions WHERE refresh_token_hash = ?
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
	u, err := s.GetUser(ctx, tenantID, userID)
	if err != nil {
		return result, err
	}
	if u.Status != "active" {
		return result, ErrAccountDisabled
	}
	if err := s.ensureTenantActive(ctx, tenantID); err != nil {
		return result, err
	}
	accessTTL, refreshTTL := s.tenantTTL(ctx, tenantID)
	accessPlain, accessHash, err := randomPrefixedToken(accessPrefix)
	if err != nil {
		return result, err
	}
	refreshPlain, refreshHash, err := randomPrefixedToken(refreshPrefix)
	if err != nil {
		return result, err
	}
	now := time.Now().UTC()
	accessExp := now.Add(accessTTL)
	newRefreshExp := now.Add(refreshTTL)
	// Atomic consume: only one concurrent refresh of the same refresh token wins.
	res, err := s.db.ExecContext(ctx, `
		UPDATE end_user_sessions SET access_token_hash = ?, refresh_token_hash = ?,
			access_expires_at = ?, refresh_expires_at = ?, last_seen_at = now()
		WHERE id = ? AND refresh_token_hash = ? AND revoked_at IS NULL AND refresh_expires_at > now()
	`, accessHash, refreshHash, accessExp, newRefreshExp, sessionID, tokenHash(refreshToken))
	if err != nil {
		return result, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return result, ErrSessionRevoked
	}
	return LoginResult{
		AccessToken: accessPlain, RefreshToken: refreshPlain, TokenType: "Bearer",
		ExpiresAt: accessExp, RefreshExpiresAt: newRefreshExp, User: u, MustChangePassword: u.MustChangePassword,
	}, nil
}

func (s *Service) Logout(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE end_user_sessions SET revoked_at = now(), revoke_reason = 'logout' WHERE id = ? AND revoked_at IS NULL`, sessionID)
	return err
}

func (s *Service) ChangePassword(ctx context.Context, user User, sessionID, current, newPassword string) error {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM end_users WHERE id = ?`, user.ID).Scan(&hash)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
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
	if _, err = tx.ExecContext(ctx, `
		UPDATE end_users SET password_hash = ?, must_change_password = false, password_changed_at = now(), updated_at = now(), version = version + 1
		WHERE id = ?
	`, newHash, user.ID); err != nil {
		return err
	}
	// Revoke other sessions; keep the caller's current session.
	if _, err = tx.ExecContext(ctx, `
		UPDATE end_user_sessions SET revoked_at = now(), revoke_reason = 'password_change'
		WHERE end_user_id = ? AND revoked_at IS NULL AND id <> ?
	`, user.ID, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}
