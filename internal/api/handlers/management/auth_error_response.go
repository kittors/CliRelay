package management

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/ipaccess"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loginlockout"
)

// writePasswordPolicyError emits a per-rule error code when the failure is a
// password policy violation, and reports whether it handled the error.
//
// Both identity and end-user surfaces route through here so a rejected password
// always arrives at the browser as a translatable code. Previously the only
// machine-readable part was "validation_failed", which left the panel with
// nothing to key a translation off: it fell back to printing the raw English
// sentence in a toast, and that is exactly what an operator saw when a tenant
// admin password missed the uppercase rule.
func writePasswordPolicyError(c *gin.Context, err error) bool {
	var policy *identity.PasswordPolicyError
	if !errors.As(err, &policy) {
		return false
	}
	body := gin.H{"code": string(policy.Code), "message": policy.Error()}
	if policy.Limit > 0 {
		// The panel renders "at least N characters" from this rather than
		// hardcoding the bound a second time and drifting from the server.
		body["details"] = gin.H{"limit": policy.Limit}
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": body})
	return true
}

// Sign-in lock codes. Both are a cross-repo contract: the panel's sign-in forms
// map exactly these strings to their "locked, try again in …" copy and count
// down from details.retry_after_seconds.
const (
	// loginRateLimitedCode is a throttle bucket, per IP or per account.
	loginRateLimitedCode    = "login_rate_limited"
	loginRateLimitedMessage = "too many login attempts"
	// loginCooldownCode is the account's own cooldown ladder (loginlockout).
	loginCooldownCode = "login_cooldown"
)

// abortLoginLocked rejects a sign-in that a lock is holding shut and says how
// long the lock has left, in the Retry-After header and in
// details.retry_after_seconds.
//
// The header alone was not enough: the panel reads error bodies, not headers,
// so the admin sign-in rendered every lock as "too many attempts, try again
// later". A user locked for five minutes could not tell it from a one-minute or
// a one-hour lock, and kept retrying.
func abortLoginLocked(c *gin.Context, code, message string, wait time.Duration) {
	seconds := retryAfterSeconds(wait)
	c.Header("Retry-After", strconv.Itoa(seconds))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{
		"code":    code,
		"message": message,
		"details": gin.H{"retry_after_seconds": seconds},
	}})
}

// isLoginCooldown reports whether err is an account cooldown from either sign-in
// surface.
func isLoginCooldown(err error) bool {
	return errors.Is(err, loginlockout.ErrCooldown)
}

// abortLoginCooldown rejects a sign-in that hit the account's cooldown ladder.
func abortLoginCooldown(c *gin.Context, err error) {
	var cooldown *loginlockout.CooldownError
	if errors.As(err, &cooldown) {
		abortLoginLocked(c, loginCooldownCode, err.Error(), cooldown.RetryAfter)
		return
	}
	// A bare sentinel carries no deadline: still a lock, it just cannot say how long.
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": loginCooldownCode, "message": err.Error()}})
}

// respondCredentialFailure answers a sign-in whose password was compared and did
// not match, once the caller has charged the per-IP and per-account buckets.
//
// Two answers used to be missing. The failure that tripped a lock was reported
// as a plain "invalid credentials", so users learned about the lock only by
// walking into it on the next attempt, with no idea how long it would last. And
// no failure said how many attempts were left, so nobody saw the lock coming.
//
// Now the failure that trips any lock — either bucket or the account's own
// ladder — reports the longest of them, since the user can sign in only once
// every one has lapsed. Every other failure reports the attempts left.
func (h *Handler) respondCredentialFailure(c *gin.Context, err error, acctKey throttleKey, ipDecision, acctDecision throttleDecision) {
	// Stays nil for an unknown username: only an existing account has a ladder.
	var failure *loginlockout.FailureError
	_ = errors.As(err, &failure)

	code, message, wait := "", "", time.Duration(0)
	for _, d := range []throttleDecision{ipDecision, acctDecision} {
		if d.Outcome != outcomeAllow && d.RetryAfter > wait {
			code, message, wait = loginRateLimitedCode, loginRateLimitedMessage, d.RetryAfter
		}
	}
	if failure != nil && failure.Cooldown != nil && failure.Cooldown.RetryAfter >= wait {
		code, message, wait = loginCooldownCode, failure.Cooldown.Error(), failure.Cooldown.RetryAfter
	}
	if wait > 0 {
		abortLoginLocked(c, code, message, wait)
		return
	}

	body := gin.H{"code": "invalid_credentials", "message": err.Error()}
	if remaining := h.remainingLoginAttempts(c, acctKey, acctDecision, failure); remaining > 0 {
		body["details"] = gin.H{"remaining_attempts": remaining}
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": body})
}

// remainingLoginAttempts is how many more wrong passwords the account absorbs
// before the next lock, counting the one that trips it, or 0 when unknown.
//
// It is the smaller of the account bucket's headroom and the account ladder's.
// The bucket is keyed by the typed username whether or not such an account
// exists, and both start from zero after a success or a quiet spell, so for an
// ordinary run of mistypes the number is the same for a real account and an
// invented one: the warning does not reveal which usernames exist. The per-IP
// bucket is left out on purpose; it is shared with everyone behind the same
// address, so its headroom says nothing about this account.
func (h *Handler) remainingLoginAttempts(c *gin.Context, acctKey throttleKey, acctDecision throttleDecision, failure *loginlockout.FailureError) int {
	remaining := 0
	consider := func(n int) {
		if n > 0 && (remaining == 0 || n < remaining) {
			remaining = n
		}
	}
	// An allow-listed source skips the throttle entirely, so the bucket counted
	// nothing and its headroom would be a promise it does not keep.
	if !ipaccess.ExemptFromThrottle(c) {
		short, long := h.loginThrottle.limitsFor(acctKey)
		if short > 0 {
			consider(short - acctDecision.ShortCount)
		}
		if long > 0 {
			consider(long - acctDecision.LongCount)
		}
	}
	if failure != nil {
		consider(failure.Remaining)
	}
	return remaining
}
