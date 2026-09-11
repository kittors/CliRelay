package management

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
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

// abortPortalCooldown rejects a login that hit the per-account cooldown, telling
// the caller how long it has to wait.
//
// Without the duration the panel could only say "too many attempts, try again
// later", so a user locked out for five minutes could not distinguish it from a
// one-minute or a one-hour lock and kept retrying — which, once the cooldown
// lapsed, walked them onto the next rung of the ladder.
func abortPortalCooldown(c *gin.Context, err error) {
	body := gin.H{"code": "login_cooldown", "message": err.Error()}
	var cooldown *enduser.CooldownError
	if errors.As(err, &cooldown) {
		retryAfter := cooldown.RetryAfter.Round(time.Second)
		c.Header("Retry-After", retryAfterSecondsHeader(retryAfter))
		body["details"] = gin.H{"retry_after_seconds": int(retryAfter.Seconds())}
	}
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": body})
}
