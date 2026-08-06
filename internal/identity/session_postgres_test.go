package identity

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

const sessionTestPassword = "Bootstrap-Password-123!"

// newSessionTestService brings up a clean identity schema on the shared test
// database and returns a bootstrapped service plus the admin user id.
func newSessionTestService(t *testing.T) (*Service, *sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	ctx := context.Background()
	db, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err = db.Exec(`TRUNCATE audit_logs,user_session_tokens,user_sessions,user_roles,role_permissions,menus,users,roles,permissions,tenants CASCADE`); err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	if err = service.Bootstrap(ctx, sessionTestPassword); err != nil {
		t.Fatal(err)
	}
	return service, db, SystemUserID
}

func mustLogin(t *testing.T, s *Service, remember bool) LoginResult {
	t.Helper()
	result, err := s.Login(context.Background(), "admin", sessionTestPassword, remember, "test-agent")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return result
}

// TestPostgresRefreshKeepsPreviousAccessTokenDuringGrace is the primary anchor for
// the reported "signed out while using the panel" failure. Rotation used to
// invalidate the previous access token the instant a refresh landed, so any
// request already in flight came back 401 and the panel treated it as a dead
// session.
func TestPostgresRefreshKeepsPreviousAccessTokenDuringGrace(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	service.SetSessionPolicy(SessionPolicy{AccessGrace: 60 * time.Second})
	ctx := context.Background()

	login := mustLogin(t, service, false)
	if _, err := service.RefreshSession(ctx, login.RefreshToken); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if _, err := service.Authenticate(ctx, login.AccessToken, ""); err != nil {
		t.Fatalf("previous access token rejected inside the grace window: %v", err)
	}

	// Expire the grace window without sleeping through it.
	if _, err := db.ExecContext(ctx,
		`UPDATE user_session_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = ?`,
		tokenHash(login.AccessToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, login.AccessToken, ""); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("previous access token after grace = %v, want ErrSessionExpired", err)
	}
}

// TestPostgresRefreshGraceAllowsPreviousRefreshToken covers the second half of the
// same failure: a tab that retries a refresh, or a second tab that raced it, must
// not lose the session.
func TestPostgresRefreshGraceAllowsPreviousRefreshToken(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	service.SetSessionPolicy(SessionPolicy{RefreshGrace: 30 * time.Second, RefreshGraceMaxReuse: 2})
	ctx := context.Background()

	login := mustLogin(t, service, false)
	first, err := service.RefreshSession(ctx, login.RefreshToken)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	second, err := service.RefreshSession(ctx, login.RefreshToken)
	if err != nil {
		t.Fatalf("grace replay of the consumed refresh token: %v", err)
	}
	if second.AccessToken == first.AccessToken {
		t.Fatal("grace replay returned the same access token as the first rotation")
	}
	// Both branches must authenticate: forks are legal inside the grace window.
	for name, token := range map[string]string{"first": first.AccessToken, "second": second.AccessToken} {
		if _, err = service.Authenticate(ctx, token, ""); err != nil {
			t.Fatalf("%s rotation's access token rejected: %v", name, err)
		}
	}

	var reuseCount int
	if err = db.QueryRowContext(ctx,
		`SELECT reuse_count FROM user_session_tokens WHERE token_hash = ?`,
		tokenHash(login.RefreshToken)).Scan(&reuseCount); err != nil {
		t.Fatal(err)
	}
	if reuseCount != 1 {
		t.Fatalf("reuse_count = %d, want 1", reuseCount)
	}
}

// TestPostgresRefreshGraceReuseCapRevokesSession proves the grace window did not
// become an unbounded replay window: past the cap the token is treated as stolen.
func TestPostgresRefreshGraceReuseCapRevokesSession(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	service.SetSessionPolicy(SessionPolicy{RefreshGrace: 30 * time.Second, RefreshGraceMaxReuse: 2})
	ctx := context.Background()

	login := mustLogin(t, service, false)
	for i := 0; i <= 2; i++ {
		if _, err := service.RefreshSession(ctx, login.RefreshToken); err != nil {
			t.Fatalf("refresh %d inside the reuse budget: %v", i, err)
		}
	}
	if _, err := service.RefreshSession(ctx, login.RefreshToken); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("refresh past the reuse cap = %v, want ErrSessionRevoked", err)
	}

	var revokeReason string
	if err := db.QueryRowContext(ctx,
		`SELECT revoke_reason FROM user_sessions WHERE id = ?`, login.Principal.SessionID).Scan(&revokeReason); err != nil {
		t.Fatal(err)
	}
	if revokeReason == "" {
		t.Fatal("session was not revoked after reuse detection")
	}

	var remaining int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_session_tokens WHERE session_id = ?`, login.Principal.SessionID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("token rows after family revocation = %d, want 0", remaining)
	}
}

// TestPostgresRefreshOutsideGraceRevokesFamily checks that a token replayed long
// after rotation is still treated as a compromise, not as a slow retry.
func TestPostgresRefreshOutsideGraceRevokesFamily(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	service.SetSessionPolicy(SessionPolicy{RefreshGrace: 30 * time.Second, RefreshGraceMaxReuse: 2})
	ctx := context.Background()

	login := mustLogin(t, service, false)
	if _, err := service.RefreshSession(ctx, login.RefreshToken); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE user_session_tokens SET grace_until = now() - interval '1 second' WHERE token_hash = ?`,
		tokenHash(login.RefreshToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshSession(ctx, login.RefreshToken); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("replay outside the grace window = %v, want ErrSessionRevoked", err)
	}
}

// TestPostgresRefreshConcurrentRotationAllSucceed is the direct regression for the
// multi-tab report: several tabs hitting 401 at once all refresh with the same
// stored token, and every one of them must come away with a working session.
func TestPostgresRefreshConcurrentRotationAllSucceed(t *testing.T) {
	service, _, _ := newSessionTestService(t)
	service.SetSessionPolicy(SessionPolicy{RefreshGrace: 30 * time.Second, RefreshGraceMaxReuse: 8})
	ctx := context.Background()

	login := mustLogin(t, service, false)

	const concurrency = 8
	var wg sync.WaitGroup
	results := make([]LoginResult, concurrency)
	errs := make([]error, concurrency)
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = service.RefreshSession(ctx, login.RefreshToken)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent refresh %d failed: %v", i, err)
		}
	}
	for i, res := range results {
		if _, err := service.Authenticate(ctx, res.AccessToken, ""); err != nil {
			t.Fatalf("access token from concurrent refresh %d rejected: %v", i, err)
		}
	}
}

// TestPostgresRefreshPreservesRememberMeTTL is the B8 anchor: rotation used to
// recompute the refresh lifetime with remember=false, silently downgrading a
// 30-day "remember me" session on its first refresh.
func TestPostgresRefreshPreservesRememberMeTTL(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	ctx := context.Background()

	// A short tenant default is what exposes the bug: without the stored
	// provenance, rotation would fall back to this value.
	if _, err := db.ExecContext(ctx,
		`UPDATE tenants SET refresh_token_ttl_seconds = 86400 WHERE id = ?`, SystemTenantID); err != nil {
		t.Fatal(err)
	}

	login := mustLogin(t, service, true)
	if got := time.Until(login.RefreshExpiresAt); got < 29*24*time.Hour {
		t.Fatalf("remember-me login refresh TTL = %v, want >= 29 days", got)
	}
	rotated, err := service.RefreshSession(ctx, login.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := time.Until(rotated.RefreshExpiresAt); got < 29*24*time.Hour {
		t.Fatalf("refresh TTL after rotation = %v, want >= 29 days (remember-me must survive rotation)", got)
	}
}

// TestPostgresRefreshNeverExceedsAbsoluteDeadline guards the ceiling that keeps a
// stolen token from being renewed forever.
func TestPostgresRefreshNeverExceedsAbsoluteDeadline(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	ctx := context.Background()

	login := mustLogin(t, service, true)
	if _, err := db.ExecContext(ctx,
		`UPDATE user_sessions SET refresh_absolute_expires_at = now() + interval '1 hour' WHERE id = ?`,
		login.Principal.SessionID); err != nil {
		t.Fatal(err)
	}
	rotated, err := service.RefreshSession(ctx, login.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if time.Until(rotated.RefreshExpiresAt) > time.Hour+time.Minute {
		t.Fatalf("rotation extended past the absolute deadline: %v", rotated.RefreshExpiresAt)
	}
}

func TestPostgresRefreshOnRevokedSessionFails(t *testing.T) {
	service, _, _ := newSessionTestService(t)
	ctx := context.Background()

	login := mustLogin(t, service, false)
	if err := service.Logout(ctx, login.Principal.SessionID); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := service.RefreshSession(ctx, login.RefreshToken); err == nil {
		t.Fatal("refresh on a revoked session succeeded")
	}
}

// TestPostgresAuthenticateAdoptsMirroredAccessToken covers the blue-green window:
// the previous binary rotates by writing only user_sessions.token_hash, and the
// new binary must accept those tokens instead of signing the user out mid-deploy.
func TestPostgresAuthenticateAdoptsMirroredAccessToken(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	ctx := context.Background()

	login := mustLogin(t, service, false)
	// Simulate a rotation performed by the old binary: the mirror moves, the
	// per-token table does not.
	legacyAccess, legacyHash, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE user_sessions SET token_hash = ?, expires_at = now() + interval '1 hour' WHERE id = ?`,
		legacyHash, login.Principal.SessionID); err != nil {
		t.Fatal(err)
	}

	if _, err = service.Authenticate(ctx, legacyAccess, ""); err != nil {
		t.Fatalf("access token minted by the previous release was rejected: %v", err)
	}

	var backfilled int
	if err = db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_session_tokens WHERE token_hash = ? AND kind = 'access'`,
		legacyHash).Scan(&backfilled); err != nil {
		t.Fatal(err)
	}
	if backfilled != 1 {
		t.Fatalf("adopted token rows = %d, want 1 (the fallback must backfill)", backfilled)
	}
}

// TestPostgresRefreshAdoptsMirroredRefreshToken is the refresh-side counterpart.
func TestPostgresRefreshAdoptsMirroredRefreshToken(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	ctx := context.Background()

	login := mustLogin(t, service, false)
	legacyRefresh, legacyHash, err := randomPrefixedToken("cpr_adm_")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE user_sessions SET refresh_token_hash = ?, refresh_expires_at = now() + interval '24 hours' WHERE id = ?`,
		legacyHash, login.Principal.SessionID); err != nil {
		t.Fatal(err)
	}

	rotated, err := service.RefreshSession(ctx, legacyRefresh)
	if err != nil {
		t.Fatalf("refresh token minted by the previous release was rejected: %v", err)
	}
	if _, err = service.Authenticate(ctx, rotated.AccessToken, ""); err != nil {
		t.Fatalf("access token from adopted refresh rejected: %v", err)
	}

	// The adoption path must not look like a replay: the session stays alive.
	var revokedAt sql.NullTime
	if err = db.QueryRowContext(ctx,
		`SELECT revoked_at FROM user_sessions WHERE id = ?`, login.Principal.SessionID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt.Valid {
		t.Fatal("adopting a mirrored refresh token revoked the session")
	}
}

// TestPostgresLoginFailureCountDecaysOutsideWindow is the B6 anchor: the counter
// never decayed, so mistypes months apart accumulated into a lockout.
func TestPostgresLoginFailureCountDecaysOutsideWindow(t *testing.T) {
	service, db, userID := newSessionTestService(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := service.Login(ctx, "admin", "wrong-password", false, "test-agent"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failed login %d = %v, want ErrInvalidCredentials", i, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET last_failed_login_at = now() - interval '1 hour' WHERE id = ?`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, "admin", "wrong-password", false, "test-agent"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("failed login after the window = %v, want ErrInvalidCredentials", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT failed_login_count FROM users WHERE id = ?`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("failed_login_count = %d, want 1 (the window must restart the count)", count)
	}
}

// TestPostgresLoginLockoutIsTemporaryAndNeverFlipsStatus proves an automatic
// cooldown stays distinguishable from an administrative lock, and clears itself.
func TestPostgresLoginLockoutIsTemporaryAndNeverFlipsStatus(t *testing.T) {
	service, db, userID := newSessionTestService(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := service.Login(ctx, "admin", "wrong-password", false, "test-agent"); err == nil {
			t.Fatalf("failed login %d unexpectedly succeeded", i)
		}
	}

	var status string
	var lockedUntil sql.NullTime
	var lockStage int
	if err := db.QueryRowContext(ctx,
		`SELECT status, locked_until, lock_stage FROM users WHERE id = ?`, userID).Scan(&status, &lockedUntil, &lockStage); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("status = %q after an automatic cooldown, want active (only an administrator may lock)", status)
	}
	if !lockedUntil.Valid || !lockedUntil.Time.After(time.Now()) {
		t.Fatalf("locked_until = %v, want a future cooldown", lockedUntil)
	}
	if lockStage != 1 {
		t.Fatalf("lock_stage = %d, want 1", lockStage)
	}

	if _, err := service.Login(ctx, "admin", sessionTestPassword, false, "test-agent"); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("login during cooldown = %v, want ErrAccountLocked", err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE users SET locked_until = now() - interval '1 second' WHERE id = ?`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, "admin", sessionTestPassword, false, "test-agent"); err != nil {
		t.Fatalf("login after the cooldown expired: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT lock_stage FROM users WHERE id = ?`, userID).Scan(&lockStage); err != nil {
		t.Fatal(err)
	}
	if lockStage != 0 {
		t.Fatalf("lock_stage = %d after a successful login, want 0", lockStage)
	}
}

// TestPostgresLoginPreservesAdministrativeLock makes sure the self-healing path
// added for cooldowns cannot resurrect an account an administrator disabled.
func TestPostgresLoginPreservesAdministrativeLock(t *testing.T) {
	service, db, userID := newSessionTestService(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`UPDATE users SET status = 'locked', locked_until = NULL WHERE id = ?`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, "admin", sessionTestPassword, false, "test-agent"); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("login on an administratively locked account = %v, want ErrAccountLocked", err)
	}

	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, userID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "locked" {
		t.Fatalf("status = %q, want locked (a correct password must not clear an administrative lock)", status)
	}
}

func TestPostgresSessionReaperRemovesExpiredTokensAndSessions(t *testing.T) {
	service, db, _ := newSessionTestService(t)
	ctx := context.Background()

	live := mustLogin(t, service, false)
	stale := mustLogin(t, service, false)

	if _, err := db.ExecContext(ctx,
		`UPDATE user_session_tokens SET expires_at = now() - interval '8 days' WHERE session_id = ?`,
		stale.Principal.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE user_sessions SET revoked_at = now() - interval '60 days', revoke_reason = 'test' WHERE id = ?`,
		stale.Principal.SessionID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := service.reapOnce(ctx); err != nil {
		t.Fatalf("reapOnce: %v", err)
	}

	var staleSessions int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_sessions WHERE id = ?`, stale.Principal.SessionID).Scan(&staleSessions); err != nil {
		t.Fatal(err)
	}
	if staleSessions != 0 {
		t.Fatalf("stale session rows = %d, want 0", staleSessions)
	}
	if _, err := service.Authenticate(ctx, live.AccessToken, ""); err != nil {
		t.Fatalf("reaper removed a live session: %v", err)
	}
}
