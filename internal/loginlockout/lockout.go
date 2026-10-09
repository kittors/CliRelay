// Package loginlockout is the account-level sign-in cooldown shared by the admin
// panel (internal/identity) and the end-user portal (internal/enduser).
//
// The two used to keep separate copies that claimed to mirror each other. They
// drifted: the portal moved its first rung from three failures to five and began
// reporting how long a cooldown has left, while the admin panel kept locking on
// the third mistype and answered a correct password typed during the cooldown
// with "account locked, contact your administrator" — which people read as a
// wrong password, so they kept guessing and walked up the ladder. One table
// keeps the surfaces from drifting again.
package loginlockout

import (
	"errors"
	"fmt"
	"time"
)

// FailureWindow is the observation window for counting failures. A failure more
// than this long after the previous one starts the count again; without the
// decay the counter is a lifetime total and mistypes months apart add up to a
// lockout.
const FailureWindow = 15 * time.Minute

// rungSpacing is the number of failures between two cooldown rungs. Every rung
// of Penalty sits on a multiple of it, which is what RemainingBeforeCooldown
// relies on.
const rungSpacing = 5

// Penalty maps a failure count to a staged cooldown. apply is true only at the
// threshold values, so failures inside a stage do not re-extend an active
// cooldown from zero.
//
// There is deliberately no permanent branch. A lock that only an administrator
// can clear is an attacker-triggerable denial of service (OWASP): any client
// that can submit a password could disable any account.
//
// The first rung sits at five rather than three because production access logs
// show users holding a correct generated password routinely needing two or
// three attempts to transcribe it, so the third honest mistype was already a
// lockout. Guessing protection does not rest on this ladder: the account-scoped
// throttle bucket in the API layer (five failures per fifteen minutes, with its
// own escalating backoff) is the real defence, and this ladder backstops it
// across restarts and nodes.
func Penalty(failedCount int) (stage int, wait time.Duration, apply bool) {
	switch {
	case failedCount >= 25:
		// Past the top stage, re-arm every fifth failure instead of on every
		// attempt, so a client retrying in a loop cannot ratchet its own penalty.
		return 5, 60 * time.Minute, failedCount%rungSpacing == 0
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

// RemainingBeforeCooldown reports how many more failures the account can absorb
// before the next rung arms, counting the failure that arms it: after one
// failure four remain, and the fourth of them starts a cooldown.
//
// This is what lets the sign-in form warn before the lock instead of after it.
// Without the count, the first thing a user learned about the ladder was being
// locked out by it.
func RemainingBeforeCooldown(failedCount int) int {
	if failedCount < 0 {
		failedCount = 0
	}
	return rungSpacing - failedCount%rungSpacing
}

// ErrCooldown is the sentinel every cooldown satisfies.
var ErrCooldown = errors.New("login cooldown")

// CooldownError reports how long an account-level cooldown still has to run.
//
// A bare "not now" left the sign-in form with a single "too many attempts, try
// again later" string, so a five-minute lock was indistinguishable from a
// one-minute or a one-hour one, and users kept retrying straight onto the next
// rung.
type CooldownError struct {
	RetryAfter time.Duration
}

func (e *CooldownError) Error() string {
	return fmt.Sprintf("login cooldown: retry after %s", e.RetryAfter.Round(time.Second))
}

// Is lets callers test errors.Is(err, ErrCooldown) without unwrapping.
func (e *CooldownError) Is(target error) bool { return target == ErrCooldown }

// NewCooldownError builds the error from an absolute deadline, clamping to a
// whole second so a caller never renders "retry after 0s" for a live lock.
func NewCooldownError(until, now time.Time) *CooldownError {
	remaining := until.Sub(now)
	if remaining < time.Second {
		remaining = time.Second
	}
	return &CooldownError{RetryAfter: remaining.Round(time.Second)}
}

// FailureError is a wrong password compared against an existing account.
//
// It satisfies errors.Is against the owning package's invalid-credentials
// sentinel, so the API layer keeps charging its guess budget for it. That
// includes the failure that arms a cooldown: the portal used to report that one
// as a bare cooldown, which the API layer correctly treats as "no password was
// compared", so the guess that tripped the lock was the one guess never charged.
type FailureError struct {
	// Sentinel is the owning package's ErrInvalidCredentials.
	Sentinel error
	// Remaining is RemainingBeforeCooldown for the account's new failure count.
	// Zero means unknown; it is only meaningful while Cooldown is nil.
	Remaining int
	// Cooldown is set when this failure armed a cooldown.
	Cooldown *CooldownError
}

func (e *FailureError) Error() string { return e.Sentinel.Error() }

// Is reports the failure as the owning package's invalid-credentials sentinel.
// It deliberately does not match ErrCooldown even when Cooldown is set: callers
// that only know the sentinels must keep seeing a wrong password, and the API
// layer reads Cooldown explicitly.
func (e *FailureError) Is(target error) bool { return target == e.Sentinel }

// NewFailure describes one wrong password that brought the account's failure
// count to failedCount. armedUntil is the deadline of the cooldown this failure
// armed, or the zero time when it armed none.
func NewFailure(sentinel error, failedCount int, armedUntil, now time.Time) *FailureError {
	failure := &FailureError{Sentinel: sentinel, Remaining: RemainingBeforeCooldown(failedCount)}
	if !armedUntil.IsZero() {
		failure.Cooldown = NewCooldownError(armedUntil, now)
	}
	return failure
}
