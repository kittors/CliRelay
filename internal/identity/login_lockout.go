package identity

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/loginlockout"
	log "github.com/sirupsen/logrus"
)

// registerLoginFailure charges one failure against the account and arms the
// staged cooldown (loginlockout.Penalty) when a threshold is crossed.
//
// It returns the failure as a *loginlockout.FailureError so the sign-in form
// can warn how many attempts are left before the lock, and report the lock and
// its length on the failure that arms it, instead of the user discovering both
// by hitting the lock.
//
// It deliberately never writes users.status: an automatic lock that flips status
// is indistinguishable from an administrative lock, and the historical
// "status='locked' with locked_until IS NULL" combination locked accounts out
// permanently with no way back except a manual database edit.
func (s *Service) registerLoginFailure(ctx context.Context, tenantID, userID string) (*loginlockout.FailureError, error) {
	if s == nil || s.db == nil {
		return &loginlockout.FailureError{Sentinel: ErrInvalidCredentials}, nil
	}
	var count int
	// Counting and decay happen in one statement so concurrent failures cannot
	// lose an update, and so the window boundary is evaluated by the database
	// clock rather than by whichever replica happened to serve the request.
	if err := s.db.QueryRowContext(ctx, `
		UPDATE users
		   SET failed_login_count = CASE
		         WHEN last_failed_login_at IS NULL
		           OR last_failed_login_at < now() - make_interval(secs => ?)
		         THEN 1 ELSE failed_login_count + 1 END,
		       last_failed_login_at = now(),
		       updated_at = now()
		 WHERE id = ?
		RETURNING failed_login_count
	`, loginlockout.FailureWindow.Seconds(), userID).Scan(&count); err != nil {
		return nil, err
	}
	log.Debugf("identity: login failure user=%s count=%d window=%s", userID, count, loginlockout.FailureWindow)

	now := time.Now()
	stage, wait, apply := loginlockout.Penalty(count)
	if !apply || wait <= 0 {
		return loginlockout.NewFailure(ErrInvalidCredentials, count, time.Time{}, now), nil
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE users SET locked_until = now() + make_interval(secs => ?), lock_stage = ?, updated_at = now()
		 WHERE id = ?
	`, wait.Seconds(), stage, userID); err != nil {
		return nil, err
	}
	log.Warnf("identity: login cooldown armed user=%s stage=%d wait=%s failed_count=%d", userID, stage, wait, count)
	s.RecordAudit(ctx, AuditEvent{
		TenantID:     tenantID,
		ActorKind:    "system",
		Action:       "auth.login_locked",
		ResourceType: "user",
		ResourceID:   userID,
		Result:       "denied",
		Changes: map[string]any{
			"stage":        stage,
			"wait_seconds": int(wait.Seconds()),
			"failed_count": count,
		},
	})
	return loginlockout.NewFailure(ErrInvalidCredentials, count, now.Add(wait), now), nil
}
