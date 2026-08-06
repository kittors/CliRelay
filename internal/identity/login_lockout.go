package identity

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// loginFailureWindow is the observation window for counting login failures.
// Without it the counter is a lifetime total, so any account eventually crosses
// every threshold and stays locked for reasons that happened months apart.
const loginFailureWindow = 15 * time.Minute

// lockPenalty mirrors internal/enduser/login_lockout.go — staged cooldown, never
// a permanent lock. OWASP: permanent lockout is an attacker-triggerable DoS
// (96 requests/day permanently locks any account when the counter never decays).
//
// apply is true only when the count crosses a threshold, so failures inside a
// stage do not keep re-extending the cooldown from zero.
func lockPenalty(failedCount int) (stage int, wait time.Duration, apply bool) {
	switch {
	case failedCount >= 20:
		return 5, 60 * time.Minute, failedCount == 20 || failedCount%5 == 0
	case failedCount >= 15:
		return 4, 30 * time.Minute, failedCount == 15
	case failedCount >= 10:
		return 3, 15 * time.Minute, failedCount == 10
	case failedCount >= 5:
		return 2, 5 * time.Minute, failedCount == 5
	case failedCount >= 3:
		return 1, 1 * time.Minute, failedCount == 3
	default:
		return 0, 0, false
	}
}

// registerLoginFailure charges one failure against the account and arms the
// staged cooldown when a threshold is crossed.
//
// It deliberately never writes users.status: an automatic lock that flips status
// is indistinguishable from an administrative lock, and the historical
// "status='locked' with locked_until IS NULL" combination locked accounts out
// permanently with no way back except a manual database edit.
func (s *Service) registerLoginFailure(ctx context.Context, tenantID, userID string) (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
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
	`, loginFailureWindow.Seconds(), userID).Scan(&count); err != nil {
		return 0, err
	}
	log.Debugf("identity: login failure user=%s count=%d window=%s", userID, count, loginFailureWindow)

	stage, wait, apply := lockPenalty(count)
	if !apply || wait <= 0 {
		return count, nil
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE users SET locked_until = now() + make_interval(secs => ?), lock_stage = ?, updated_at = now()
		 WHERE id = ?
	`, wait.Seconds(), stage, userID); err != nil {
		return count, err
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
	return count, nil
}
