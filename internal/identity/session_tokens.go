package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

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

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
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

// SessionPolicy tunes how far a rotated token stays usable. Every value is a
// deliberate compromise between "a racing tab must not be logged out" and "a
// stolen token must not live forever"; see the per-field defaults below.
type SessionPolicy struct {
	// RefreshGrace keeps a consumed refresh token usable so a retry or a tab that
	// lost the rotation race can still complete. It must exceed the panel's total
	// refresh budget (lock wait + retries = 23s) or a legitimate retry lands
	// outside the window and is misread as a replay, revoking the whole session.
	RefreshGrace time.Duration
	// RefreshGraceMaxReuse bounds how many times one consumed refresh token may be
	// replayed inside the grace window, which bounds the fork count of a session
	// family to RefreshGraceMaxReuse+1.
	RefreshGraceMaxReuse int
	// AccessGrace is how long the previous access token stays valid after a
	// rotation. It must exceed the panel's request timeout (30s) so a request
	// already in flight when the refresh lands is not rejected.
	AccessGrace time.Duration
	// AbsoluteRefreshTTL is the hard ceiling on a session family measured from
	// login. Rotation never extends past it, which is what bounds how long a
	// stolen refresh token can be kept alive by rotating it.
	AbsoluteRefreshTTL time.Duration
}

const (
	defaultRefreshGrace         = 30 * time.Second
	maxRefreshGrace             = 60 * time.Second
	defaultRefreshGraceMaxReuse = 2
	defaultAccessGrace          = 60 * time.Second
	defaultAbsoluteRefreshTTL   = 60 * 24 * time.Hour
	// defaultSessionReaperInterval is the GC cadence for expired token rows and
	// sessions past their absolute deadline.
	defaultSessionReaperInterval = 60 * time.Minute
)

// normalized fills zero fields with the shipped defaults and clamps the grace
// window. The clamp is an upper bound only: a longer grace window widens the
// period in which a stolen refresh token is indistinguishable from a retry.
func (p SessionPolicy) normalized() SessionPolicy {
	if p.RefreshGrace <= 0 {
		p.RefreshGrace = defaultRefreshGrace
	}
	if p.RefreshGrace > maxRefreshGrace {
		p.RefreshGrace = maxRefreshGrace
	}
	if p.RefreshGraceMaxReuse <= 0 {
		p.RefreshGraceMaxReuse = defaultRefreshGraceMaxReuse
	}
	if p.AccessGrace <= 0 {
		p.AccessGrace = defaultAccessGrace
	}
	if p.AbsoluteRefreshTTL <= 0 {
		p.AbsoluteRefreshTTL = defaultAbsoluteRefreshTTL
	}
	return p
}

// SessionPolicyFromConfig reads remote-management.auth. Absent or non-positive
// values fall back to the defaults rather than disabling the feature: a zero
// grace window would restore the hard rotation this policy exists to remove.
func SessionPolicyFromConfig(cfg *config.Config) SessionPolicy {
	if cfg == nil {
		return SessionPolicy{}.normalized()
	}
	auth := cfg.RemoteManagement.Auth
	policy := SessionPolicy{
		RefreshGraceMaxReuse: auth.RefreshGraceMaxReuse,
	}
	if auth.RefreshGraceSeconds > 0 {
		policy.RefreshGrace = time.Duration(auth.RefreshGraceSeconds) * time.Second
	}
	if auth.AccessTokenGraceSeconds > 0 {
		policy.AccessGrace = time.Duration(auth.AccessTokenGraceSeconds) * time.Second
	}
	if auth.AbsoluteRefreshTTLHours > 0 {
		policy.AbsoluteRefreshTTL = time.Duration(auth.AbsoluteRefreshTTLHours) * time.Hour
	}
	return policy.normalized()
}

// SessionReaperIntervalFromConfig resolves the reaper cadence, defaulting when
// the operator did not configure one.
func SessionReaperIntervalFromConfig(cfg *config.Config) time.Duration {
	if cfg == nil {
		return defaultSessionReaperInterval
	}
	if minutes := cfg.RemoteManagement.Auth.SessionReaperIntervalMinutes; minutes > 0 {
		return time.Duration(minutes) * time.Minute
	}
	return defaultSessionReaperInterval
}

// SetSessionPolicy installs a policy for subsequent logins and refreshes.
// Stored behind an atomic pointer because config hot-reload runs concurrently
// with in-flight refreshes, which must never observe a half-applied policy.
func (s *Service) SetSessionPolicy(p SessionPolicy) {
	if s == nil {
		return
	}
	normalized := p.normalized()
	s.policy.Store(&normalized)
}

func (s *Service) sessionPolicy() SessionPolicy {
	if s == nil {
		return SessionPolicy{}.normalized()
	}
	if p := s.policy.Load(); p != nil {
		return *p
	}
	return SessionPolicy{}.normalized()
}

// sessionTokenInsert describes the access/refresh pair issued by one login or
// one rotation. Both rows share a generation and a parent so a family can be
// walked back to the token it descends from during an incident review.
type sessionTokenInsert struct {
	SessionID   string
	AccessHash  string
	AccessExp   time.Time
	RefreshHash string
	RefreshExp  time.Time
	Generation  int64
	ParentHash  string
}

func (s *Service) insertSessionTokens(ctx context.Context, tx *sql.Tx, in sessionTokenInsert) error {
	var parent any
	if strings.TrimSpace(in.ParentHash) != "" {
		parent = in.ParentHash
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_session_tokens (token_hash, session_id, kind, issued_at, expires_at, generation, parent_hash)
		VALUES (?, ?, 'access', now(), ?, ?, ?)
	`, in.AccessHash, in.SessionID, in.AccessExp, in.Generation, parent); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_session_tokens (token_hash, session_id, kind, issued_at, expires_at, generation, parent_hash)
		VALUES (?, ?, 'refresh', now(), ?, ?, ?)
	`, in.RefreshHash, in.SessionID, in.RefreshExp, in.Generation, parent); err != nil {
		return err
	}
	return nil
}

// accessSession is the session context resolved from an access token.
type accessSession struct {
	sessionID string
	userID    string
	tenantID  string
	expiresAt time.Time
	revokedAt sql.NullTime
}

// lookupAccessSession resolves an access token to its session. user_session_tokens
// is the authentication source; the user_sessions mirror is only a fallback.
func (s *Service) lookupAccessSession(ctx context.Context, hash string) (accessSession, error) {
	var out accessSession
	err := s.db.QueryRowContext(ctx, `
		SELECT t.session_id, s.user_id, s.tenant_id, t.expires_at, s.revoked_at
		  FROM user_session_tokens t
		  JOIN user_sessions s ON s.id = t.session_id
		 WHERE t.token_hash = ? AND t.kind = 'access'
	`, hash).Scan(&out.sessionID, &out.userID, &out.tenantID, &out.expiresAt, &out.revokedAt)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	return s.adoptMirroredAccessSession(ctx, hash)
}

// adoptMirroredAccessSession is the blue-green compatibility path.
//
// Deployment starts the next slot, waits for /readyz, switches nginx and only
// then drains the old slot, so both binaries serve traffic for ~35s. The
// previous binary rotates by overwriting user_sessions.token_hash and never
// writes user_session_tokens, so an access token it minted during that window
// exists only in the mirror. Rejecting it would sign out every user who
// happened to refresh during the switch — the failure this change exists to
// remove. The token is backfilled so the next request takes the fast path.
//
// Delete this together with the user_sessions.token_hash / refresh_token_hash
// columns in the contract migration; keeping it after those columns are gone is
// dead code, removing it before they are gone re-creates the sign-out window.
func (s *Service) adoptMirroredAccessSession(ctx context.Context, hash string) (accessSession, error) {
	var out accessSession
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, tenant_id, expires_at, revoked_at
		  FROM user_sessions WHERE token_hash = ?
	`, hash).Scan(&out.sessionID, &out.userID, &out.tenantID, &out.expiresAt, &out.revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrSessionRevoked
	}
	if err != nil {
		return out, err
	}
	if out.revokedAt.Valid || !out.expiresAt.After(time.Now()) {
		// Do not backfill a dead token: the caller rejects it anyway, and the row
		// would only be garbage for the reaper to collect.
		return out, nil
	}
	if _, err = s.db.ExecContext(ctx, `
		INSERT INTO user_session_tokens (token_hash, session_id, kind, issued_at, expires_at, generation)
		VALUES (?, ?, 'access', now(), ?, 0)
		ON CONFLICT (token_hash) DO NOTHING
	`, hash, out.sessionID, out.expiresAt); err != nil {
		return out, err
	}
	return out, nil
}

// resolveRefreshTTL reproduces the refresh lifetime the session was created
// with. The stored value wins over the tenant configuration because a
// "remember me" login is a property of that session, not of the tenant: reading
// the tenant default at refresh time is what silently downgraded every 30-day
// session to the tenant's (much shorter) TTL on its first rotation.
func (s *Service) resolveRefreshTTL(ctx context.Context, tenantID string, remember bool, storedSeconds int) time.Duration {
	if storedSeconds > 0 {
		return time.Duration(storedSeconds) * time.Second
	}
	_, refresh := s.tenantSessionTTL(ctx, tenantID, remember)
	return refresh
}
