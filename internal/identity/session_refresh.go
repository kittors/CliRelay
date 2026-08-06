package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// adminRefreshTokenPrefix marks a refresh token minted for the admin panel.
const adminRefreshTokenPrefix = "cpr_adm_"

// refreshClaim is the outcome of successfully consuming a refresh token.
type refreshClaim struct {
	sessionID  string
	generation int64
	// reuseCount is 0 on the first consumption and >0 for every replay served
	// inside the grace window, which is exactly the "was this a grace hit"
	// predicate. It is preferred over comparing consumed_at against now()
	// because a slow statement would make a timestamp comparison lie.
	reuseCount int
	graceUntil sql.NullTime
}

// sessionRotationState is the session-level context a rotation needs.
type sessionRotationState struct {
	userID            string
	tenantID          string
	rememberMe        bool
	refreshTTLSeconds int
	absoluteExpiresAt sql.NullTime
}

// RefreshSession rotates a session's token pair.
//
// Rotation is not destructive: the consumed refresh token stays usable for
// SessionPolicy.RefreshGrace so that a client retry, or a second browser tab
// that lost the race, still receives a working pair instead of being signed
// out. Replays past the grace window (or past the reuse cap) are treated as
// token theft and revoke the whole session family — a grace window without
// reuse detection would be strictly worse than hard rotation, because a thief
// could then sit on a stolen token indefinitely without anyone noticing.
func (s *Service) RefreshSession(ctx context.Context, refreshToken string) (LoginResult, error) {
	var result LoginResult
	if s == nil || s.db == nil || !strings.HasPrefix(strings.TrimSpace(refreshToken), adminRefreshTokenPrefix) {
		return result, ErrSessionRevoked
	}
	hash := tokenHash(refreshToken)
	policy := s.sessionPolicy()

	claim, ok, err := s.claimRefreshToken(ctx, hash, policy)
	if err != nil {
		return result, err
	}
	if !ok {
		adopted, adoptErr := s.adoptMirroredRefreshToken(ctx, hash)
		if adoptErr != nil {
			return result, adoptErr
		}
		if !adopted {
			return result, s.classifyRefreshFailure(ctx, hash)
		}
		claim, ok, err = s.claimRefreshToken(ctx, hash, policy)
		if err != nil {
			return result, err
		}
		if !ok {
			return result, s.classifyRefreshFailure(ctx, hash)
		}
	}
	return s.rotateSession(ctx, hash, claim, policy)
}

// claimRefreshToken consumes the token in a single statement. The row lock taken
// by the UPDATE is what serialises concurrent refreshes of the same token, so
// there is deliberately no read-modify-write: two tabs refreshing at the same
// instant are ordered by the database, and the loser is served from the grace
// window instead of being told its session is gone.
func (s *Service) claimRefreshToken(ctx context.Context, hash string, policy SessionPolicy) (refreshClaim, bool, error) {
	var claim refreshClaim
	err := s.db.QueryRowContext(ctx, `
		UPDATE user_session_tokens
		   SET consumed_at = COALESCE(consumed_at, now()),
		       grace_until = COALESCE(grace_until, now() + make_interval(secs => ?)),
		       reuse_count = CASE WHEN consumed_at IS NULL THEN 0 ELSE reuse_count + 1 END
		 WHERE token_hash = ?
		   AND kind = 'refresh'
		   AND expires_at > now()
		   AND (consumed_at IS NULL OR (grace_until > now() AND reuse_count < ?))
		   AND EXISTS (
		         SELECT 1 FROM user_sessions s
		          WHERE s.id = user_session_tokens.session_id AND s.revoked_at IS NULL
		       )
		RETURNING session_id, generation, reuse_count, grace_until
	`, policy.RefreshGrace.Seconds(), hash, policy.RefreshGraceMaxReuse).
		Scan(&claim.sessionID, &claim.generation, &claim.reuseCount, &claim.graceUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, false, nil
	}
	if err != nil {
		return claim, false, err
	}
	return claim, true, nil
}

// adoptMirroredRefreshToken is the blue-green compatibility path.
//
// This deployment starts the next slot, waits for /readyz, switches nginx and
// only then drains the old slot, so both binaries serve traffic for ~35s. The
// previous binary rotates by overwriting user_sessions.refresh_token_hash and
// never writes user_session_tokens, so a token it minted during that window is
// invisible to the new binary. Without this fallback those sessions would be
// classified as unknown tokens and signed out — the exact failure this change
// exists to remove.
//
// Adoption is deliberately not a replay: the token has no row in
// user_session_tokens at all, so it cannot have been consumed under the new
// rules. INSERT ... ON CONFLICT DO NOTHING is what distinguishes the two — if a
// row already exists, the caller falls through to normal classification.
//
// Delete this together with the user_sessions.token_hash / refresh_token_hash
// columns in the contract migration; keeping it after those columns are gone is
// dead code, removing it before they are gone re-creates the sign-out window.
func (s *Service) adoptMirroredRefreshToken(ctx context.Context, hash string) (bool, error) {
	var sessionID string
	var refreshExpiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT id, refresh_expires_at
		  FROM user_sessions
		 WHERE refresh_token_hash = ?
		   AND revoked_at IS NULL
		   AND refresh_expires_at IS NOT NULL
		   AND refresh_expires_at > now()
	`, hash).Scan(&sessionID, &refreshExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO user_session_tokens (token_hash, session_id, kind, issued_at, expires_at, generation)
		VALUES (?, ?, 'refresh', now(), ?, 0)
		ON CONFLICT (token_hash) DO NOTHING
	`, hash, sessionID, refreshExpiresAt)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	log.Infof("identity: adopted mirrored refresh token for session %s issued by a previous release", sessionID)
	return true, nil
}

// classifyRefreshFailure turns "the token could not be consumed" into the
// specific reason. The order matters: every benign explanation is checked before
// replay detection, because replay detection revokes the entire session family
// and must never fire for a token that was merely expired or already revoked.
func (s *Service) classifyRefreshFailure(ctx context.Context, hash string) error {
	var (
		sessionID  string
		tenantID   string
		expiresAt  time.Time
		consumedAt sql.NullTime
		graceUntil sql.NullTime
		reuseCount int
		revokedAt  sql.NullTime
		absoluteAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT t.session_id, s.tenant_id, t.expires_at, t.consumed_at, t.grace_until, t.reuse_count,
		       s.revoked_at, s.refresh_absolute_expires_at
		  FROM user_session_tokens t
		  JOIN user_sessions s ON s.id = t.session_id
		 WHERE t.token_hash = ? AND t.kind = 'refresh'
	`, hash).Scan(&sessionID, &tenantID, &expiresAt, &consumedAt, &graceUntil, &reuseCount, &revokedAt, &absoluteAt)
	if errors.Is(err, sql.ErrNoRows) {
		// No row anywhere: either a forged token or one issued before the last
		// family revocation. Logged because a sustained rate of these is the
		// signature of someone probing the endpoint.
		log.Warnf("identity: refresh rejected, unknown token")
		s.RecordAudit(ctx, AuditEvent{
			ActorKind:    "system",
			Action:       "auth.refresh_unknown_token",
			ResourceType: "session",
			Result:       "denied",
		})
		return ErrSessionRevoked
	}
	if err != nil {
		return err
	}
	now := time.Now()
	if revokedAt.Valid {
		return ErrSessionRevoked
	}
	if !expiresAt.After(now) {
		return ErrSessionExpired
	}
	if absoluteAt.Valid && !absoluteAt.Time.After(now) {
		if revokeErr := s.revokeSessionFamily(ctx, sessionID, "absolute_expiry"); revokeErr != nil {
			return revokeErr
		}
		log.Infof("identity: session %s reached its absolute refresh deadline", sessionID)
		s.RecordAudit(ctx, AuditEvent{
			TenantID:       tenantID,
			ActorKind:      "system",
			ActorSessionID: sessionID,
			Action:         "auth.refresh_absolute_expired",
			ResourceType:   "session",
			ResourceID:     sessionID,
			Result:         "denied",
		})
		return ErrSessionExpired
	}
	// Everything benign has been ruled out: the token was consumed and is being
	// presented again outside the grace window or past the reuse cap. Treat it as
	// theft and revoke the family, which invalidates whichever copy the attacker
	// holds and forces the legitimate user to re-authenticate.
	if revokeErr := s.revokeSessionFamily(ctx, sessionID, "refresh_reuse_detected"); revokeErr != nil {
		return revokeErr
	}
	log.Warnf("identity: refresh token reuse detected, revoked session %s (reuse_count=%d)", sessionID, reuseCount)
	s.RecordAudit(ctx, AuditEvent{
		TenantID:       tenantID,
		ActorKind:      "system",
		ActorSessionID: sessionID,
		Action:         "auth.refresh_reuse_detected",
		ResourceType:   "session",
		ResourceID:     sessionID,
		Result:         "denied",
		Changes: map[string]any{
			"reuse_count": reuseCount,
			"grace_until": nullTimeString(graceUntil),
		},
	})
	return ErrSessionRevoked
}

// rotateSession issues the successor token pair for a claimed refresh token.
func (s *Service) rotateSession(ctx context.Context, consumedHash string, claim refreshClaim, policy SessionPolicy) (LoginResult, error) {
	var result LoginResult
	state, err := s.loadSessionRotationState(ctx, claim.sessionID)
	if err != nil {
		return result, err
	}

	accessTTL, _ := s.tenantSessionTTL(ctx, state.tenantID, state.rememberMe)
	refreshTTL := s.resolveRefreshTTL(ctx, state.tenantID, state.rememberMe, state.refreshTTLSeconds)

	accessPlain, accessHash, err := randomToken()
	if err != nil {
		return result, err
	}
	refreshPlain, refreshHash, err := randomPrefixedToken(adminRefreshTokenPrefix)
	if err != nil {
		return result, err
	}
	now := time.Now().UTC()
	accessExp := now.Add(accessTTL)
	newRefreshExp := now.Add(refreshTTL)
	// The absolute deadline is a ceiling, never a target: without this min a
	// stolen token could be kept alive forever by rotating it, and a grace-window
	// replay would extend the family it was stolen from.
	if state.absoluteExpiresAt.Valid && newRefreshExp.After(state.absoluteExpiresAt.Time) {
		newRefreshExp = state.absoluteExpiresAt.Time
	}
	if !newRefreshExp.After(now) {
		if revokeErr := s.revokeSessionFamily(ctx, claim.sessionID, "absolute_expiry"); revokeErr != nil {
			return result, revokeErr
		}
		return result, ErrSessionExpired
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialise concurrent rotations of the same session before touching any token
	// row. Without it two rotations interleave as: each inserts its own access
	// token, then each tries to shorten the other's (the sibling UPDATE below
	// matches every access row except its own), which is a lock cycle Postgres
	// breaks by killing one transaction with a deadlock error. Several tabs
	// refreshing at once is the normal case this change exists to support, so that
	// error would be a routine sign-out. Claiming the session row first gives every
	// rotation the same lock order; contention is per-session and lasts one
	// statement batch.
	var locked string
	if err = tx.QueryRowContext(ctx,
		`SELECT id FROM user_sessions WHERE id = ? FOR UPDATE`, claim.sessionID).Scan(&locked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrSessionRevoked
		}
		return result, err
	}
	if err = s.insertSessionTokens(ctx, tx, sessionTokenInsert{
		SessionID:   claim.sessionID,
		AccessHash:  accessHash,
		AccessExp:   accessExp,
		RefreshHash: refreshHash,
		RefreshExp:  newRefreshExp,
		Generation:  claim.generation + 1,
		ParentHash:  consumedHash,
	}); err != nil {
		return result, err
	}
	// Shorten, do not delete, the sibling access tokens. Deleting them is what
	// makes a request that was already in flight when the refresh landed come
	// back 401 — the single most common shape of "I was working and got logged
	// out". AccessGrace is sized to outlast the panel's request timeout.
	if _, err = tx.ExecContext(ctx, `
		UPDATE user_session_tokens
		   SET expires_at = LEAST(expires_at, now() + make_interval(secs => ?))
		 WHERE session_id = ? AND kind = 'access' AND token_hash <> ?
		   AND expires_at > now() + make_interval(secs => ?)
	`, policy.AccessGrace.Seconds(), claim.sessionID, accessHash, policy.AccessGrace.Seconds()); err != nil {
		return result, err
	}
	// Mirror the new head back onto user_sessions. The revoked_at guard closes the
	// race where the session was revoked between the claim and this write.
	res, err := tx.ExecContext(ctx, `
		UPDATE user_sessions
		   SET token_hash = ?, expires_at = ?, refresh_token_hash = ?, refresh_expires_at = ?, last_seen_at = now()
		 WHERE id = ? AND revoked_at IS NULL
	`, accessHash, accessExp, refreshHash, newRefreshExp, claim.sessionID)
	if err != nil {
		return result, err
	}
	if affected, affErr := res.RowsAffected(); affErr != nil {
		return result, affErr
	} else if affected == 0 {
		return result, ErrSessionRevoked
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}

	if claim.reuseCount > 0 {
		// The only signal this design produces for a stolen refresh token: a
		// legitimate client hits the grace window at most once per rotation, so a
		// sustained rate here means either an attacker replaying or a client bug.
		log.Infof("identity: refresh served from grace window session=%s generation=%d reuse_count=%d",
			claim.sessionID, claim.generation, claim.reuseCount)
		s.RecordAudit(ctx, AuditEvent{
			TenantID:       state.tenantID,
			ActorKind:      "user_session",
			ActorUserID:    state.userID,
			ActorSessionID: claim.sessionID,
			Action:         "auth.refresh_grace_used",
			ResourceType:   "session",
			ResourceID:     claim.sessionID,
			Result:         "success",
			Changes: map[string]any{
				"session_id":  claim.sessionID,
				"generation":  claim.generation,
				"reuse_count": claim.reuseCount,
				"grace_until": nullTimeString(claim.graceUntil),
			},
		})
	}

	principal, err := s.loadPrincipal(ctx, state.userID, claim.sessionID, accessExp, state.tenantID)
	if err != nil {
		return result, err
	}
	return LoginResult{
		AccessToken: accessPlain, RefreshToken: refreshPlain, TokenType: "Bearer",
		ExpiresAt: accessExp, RefreshExpiresAt: newRefreshExp, Principal: principal,
	}, nil
}

func (s *Service) loadSessionRotationState(ctx context.Context, sessionID string) (sessionRotationState, error) {
	var state sessionRotationState
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, tenant_id, remember_me, refresh_ttl_seconds, refresh_absolute_expires_at, revoked_at
		  FROM user_sessions WHERE id = ?
	`, sessionID).Scan(&state.userID, &state.tenantID, &state.rememberMe, &state.refreshTTLSeconds,
		&state.absoluteExpiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrSessionRevoked
	}
	if err != nil {
		return state, err
	}
	if revokedAt.Valid {
		return state, ErrSessionRevoked
	}
	return state, nil
}

// revokeSessionFamily kills a session and every token descended from it. The
// DELETE is what makes reuse detection final: leaving the rows behind would let
// the same stolen token produce a fresh reuse alert on every retry.
func (s *Service) revokeSessionFamily(ctx context.Context, sessionID, reason string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = now(), revoke_reason = ? WHERE id = ? AND revoked_at IS NULL
	`, reason, sessionID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_session_tokens WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	return nil
}

func nullTimeString(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339)
}
