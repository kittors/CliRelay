package enduser

import "time"

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
func lockPenalty(failedCount int) (stage int, wait time.Duration, apply bool) {
	switch {
	case failedCount >= 20:
		// Past the top stage, re-arm every fifth failure instead of on every
		// attempt, so a client retrying in a loop cannot ratchet its own penalty.
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
