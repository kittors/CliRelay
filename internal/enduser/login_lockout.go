package enduser

import (
	"fmt"
	"time"
)

// endUserFailureWindow is the sliding window for counting portal login failures.
//
// Without it failed_login_count only ever grew: a counter that never decays turns
// "20 wrong passwords" from a burst into a lifetime total, so an account could be
// locked by mistypes spread across months.
const endUserFailureWindow = 15 * time.Minute

// lockPenalty maps a failure count to a staged cooldown. It applies only at the
// threshold values so intermediate failures inside a stage do not re-extend an
// active cooldown.
//
// There is deliberately no permanent branch. The previous version locked the
// account outright at 20 failures (status='locked', locked_until=NULL), which
// only an administrator could clear — and because the counter never decayed,
// any attacker could permanently disable any portal account with 20 requests.
// OWASP treats attacker-triggerable permanent lockout as a denial-of-service
// vector; a rising cooldown stops guessing without handing out that lever.
//
// The first rung sits at 5 rather than 3 because production access logs show
// what the old ladder cost: users holding a *correct* generated password
// routinely needed two or three attempts to transcribe it, so the third mistype
// — still an honest one — was already a lockout. Guessing protection is not
// weakened by the change: the real defence is the account-scoped throttle
// bucket (scopePortalAccount, 5 failures per 15 minutes with its own escalating
// backoff), which this ladder only backstops.
func lockPenalty(failedCount int) (stage int, wait time.Duration, apply bool) {
	switch {
	case failedCount >= 25:
		// Past the top stage, re-arm every fifth failure instead of on every
		// attempt, so a client retrying in a loop cannot ratchet its own penalty.
		return 5, 60 * time.Minute, failedCount == 25 || failedCount%5 == 0
	case failedCount >= 20:
		return 4, 30 * time.Minute, failedCount == 20
	case failedCount >= 15:
		return 3, 15 * time.Minute, failedCount == 15
	case failedCount >= 10:
		return 2, 5 * time.Minute, failedCount == 10
	case failedCount >= 5:
		return 1, 1 * time.Minute, failedCount == 5
	default:
		return 0, 0, false
	}
}

// CooldownError reports how long an account-level cooldown still has to run.
//
// The bare ErrLoginCooldowned sentinel told the caller only "not now". The panel
// rendered that as "too many attempts, try again later" with no duration, so a
// user who had just been locked for five minutes had no way to tell it from a
// one-minute or a one-hour lock, and kept retrying — which, once the cooldown
// lapsed, walked them straight up to the next rung.
type CooldownError struct {
	RetryAfter time.Duration
}

func (e *CooldownError) Error() string {
	return fmt.Sprintf("login cooldown: retry after %s", e.RetryAfter.Round(time.Second))
}

// Is keeps existing errors.Is(err, ErrLoginCooldowned) call sites working.
func (e *CooldownError) Is(target error) bool { return target == ErrLoginCooldowned }

// newCooldownError builds the error from an absolute deadline, clamping to a
// whole second so a caller never renders "retry after 0s" for a live lock.
func newCooldownError(until time.Time, now time.Time) *CooldownError {
	remaining := until.Sub(now)
	if remaining < time.Second {
		remaining = time.Second
	}
	return &CooldownError{RetryAfter: remaining.Round(time.Second)}
}
