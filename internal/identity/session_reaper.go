package identity

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// StartSessionReaper garbage-collects expired token rows and dead sessions.
//
// It exists because rotation now appends a row per issued token instead of
// overwriting one: without a reaper, user_session_tokens grows for the lifetime
// of the deployment. The returned stop function is idempotent and must be wired
// into the process shutdown chain so a restart does not leak the goroutine.
func (s *Service) StartSessionReaper(ctx context.Context, interval time.Duration) (stop func()) {
	if s == nil || s.db == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = defaultSessionReaperInterval
	}
	reaperCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-reaperCtx.Done():
				return
			case <-ticker.C:
				tokens, sessions, err := s.reapOnce(reaperCtx)
				if err != nil {
					log.WithError(err).Debug("identity: session reaper pass failed")
					continue
				}
				if tokens > 0 || sessions > 0 {
					log.Infof("identity: session reaper removed %d tokens, %d sessions", tokens, sessions)
					continue
				}
				log.Debugf("identity: session reaper removed %d tokens, %d sessions", tokens, sessions)
			}
		}
	}()
	return cancel
}

// reapOnce runs a single garbage-collection pass.
//
// Expired token rows are kept for a day rather than deleted immediately so an
// operator investigating "why was I signed out" can still see which token
// expired when. Revoked sessions are kept for 30 days because
// audit_logs.actor_session_id points at them; that foreign key is ON DELETE SET
// NULL, so the final delete degrades the audit trail rather than failing.
func (s *Service) reapOnce(ctx context.Context) (tokens, sessions int64, err error) {
	if s == nil || s.db == nil {
		return 0, 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_session_tokens WHERE expires_at < now() - interval '1 day'`)
	if err != nil {
		return 0, 0, err
	}
	tokens, err = res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if _, err = s.db.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = now(), revoke_reason = 'absolute_expiry'
		 WHERE revoked_at IS NULL AND refresh_absolute_expires_at IS NOT NULL
		   AND refresh_absolute_expires_at < now()
	`); err != nil {
		return tokens, 0, err
	}
	res, err = s.db.ExecContext(ctx, `
		DELETE FROM user_sessions WHERE revoked_at IS NOT NULL AND revoked_at < now() - interval '30 days'
	`)
	if err != nil {
		return tokens, 0, err
	}
	sessions, err = res.RowsAffected()
	if err != nil {
		return tokens, 0, err
	}
	return tokens, sessions, nil
}
