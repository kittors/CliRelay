package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

func (h *Handler) PostLogin(c *gin.Context) {
	now := time.Now()
	// Password entry gets its own per-IP bucket, separate from the management-key
	// bucket: a wrong management key must never cost users their ability to log in.
	ipKey := h.clientThrottleKey(c, scopeUserPassword)
	if d := h.throttleEvaluate(c, ipKey, now); d.Outcome != outcomeAllow {
		h.noteThrottledAttempt(c, ipKey, "")
		abortLoginThrottled(c, d)
		return
	}

	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		RememberMe bool   `json:"remember_me"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Username) == "" || body.Password == "" {
		identityError(c, identity.ErrInvalidCredentials)
		return
	}
	// The account bucket is the layer that actually stops guessing: it survives an
	// attacker rotating source addresses, which the per-IP bucket cannot.
	acctKey := accountThrottleKey(scopeUserAccount, identity.NormalizeUsername(body.Username))
	if d := h.throttleEvaluate(c, acctKey, now); d.Outcome != outcomeAllow {
		h.noteThrottledAttempt(c, acctKey, body.Username)
		abortLoginThrottled(c, d)
		return
	}
	service := h.identity()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "identity_unavailable", "message": "identity service unavailable"}})
		return
	}
	result, err := service.Login(c.Request.Context(), body.Username, body.Password, body.RememberMe, c.GetHeader("User-Agent"))
	if err != nil {
		if isCredentialGuessFailure(err) {
			ipDecision := h.throttleCharge(c, ipKey, now)
			d := h.throttleCharge(c, acctKey, now)
			h.logAuthFailure(c, acctKey, d)
			// One attempt, one record: the two charges above are two views of the
			// same failure.
			h.noteCredentialFailure(c, acctKey, d, body.Username, "invalid password")
			h.respondCredentialFailure(c, err, acctKey, ipDecision, d)
			return
		}
		identityError(c, err)
		return
	}
	// NIST SP 800-63B §5.2.2: a success clears that account's failures, never the
	// per-IP quota — an attacker holding one valid account of their own would
	// otherwise reset their guessing budget between stuffing rounds.
	h.loginThrottle.recordSuccess(acctKey)
	h.noteAuthSuccess(c, acctKey, body.Username)
	c.JSON(http.StatusOK, result)
}

// isCredentialGuessFailure reports whether a login error means a secret was
// compared and did not match — including the failure that arms the account's
// cooldown, which carries it as a *loginlockout.FailureError. Anything else must
// not consume the guess budget: a cooldown, a suspended tenant, a disabled
// account and an infrastructure error all carry zero information about the
// password, so counting them only produces lockouts that the user cannot clear
// by typing the right password.
func isCredentialGuessFailure(err error) bool {
	return errors.Is(err, identity.ErrInvalidCredentials)
}

// abortLoginThrottled rejects a login attempt an armed throttle bucket turned
// away before any password was compared. The "login_rate_limited" code and
// details.retry_after_seconds are a cross-repo contract: the panel's sign-in
// forms map exactly that code to their lock copy and count down from the
// seconds; anything else surfaces as a raw error.
func abortLoginThrottled(c *gin.Context, d throttleDecision) {
	abortLoginLocked(c, loginRateLimitedCode, loginRateLimitedMessage, d.RetryAfter)
}

// adminRefreshTokenLength is len("cpr_adm_") + base64.RawURLEncoding(32 bytes).
const adminRefreshTokenLength = 8 + 43

// isWellFormedAdminRefreshToken rejects anything that cannot be a token this
// server issued. It exists so the unauthenticated refresh endpoint does not turn
// arbitrary request bodies into database lookups.
func isWellFormedAdminRefreshToken(token string) bool {
	t := strings.TrimSpace(token)
	return len(t) == adminRefreshTokenLength && strings.HasPrefix(t, "cpr_adm_")
}

func (h *Handler) PostRefresh(c *gin.Context) {
	// Unauthenticated and one database lookup per call: charge every attempt, not
	// just the failures, because a caller that can already refresh successfully
	// has no reason to do so sixty times in five minutes.
	refreshKey := h.clientThrottleKey(c, scopeRefresh)
	if d := h.throttleCharge(c, refreshKey, time.Now()); d.Outcome != outcomeAllow {
		h.noteNonCredentialFailure(c, refreshKey, d, "refresh endpoint rate limit")
		abortThrottled(c, d)
		return
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.RefreshToken) == "" {
		identityError(c, identity.ErrSessionRevoked)
		return
	}
	if !isWellFormedAdminRefreshToken(body.RefreshToken) {
		identityError(c, identity.ErrSessionRevoked)
		return
	}
	service := h.identity()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "identity_unavailable", "message": "identity service unavailable"}})
		return
	}
	result, err := service.RefreshSession(c.Request.Context(), body.RefreshToken)
	if err != nil {
		identityError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetMe(c *gin.Context) {
	principal, ok := h.authenticateUserRequest(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"principal": principal})
}

func (h *Handler) PostLogout(c *gin.Context) {
	principal, ok := h.authenticateUserRequest(c)
	if !ok {
		return
	}
	if err := h.identity().Logout(c.Request.Context(), principal.SessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "logout_failed", "message": err.Error()}})
		return
	}
	h.identity().RecordAudit(c.Request.Context(), identity.AuditEvent{
		TenantID:       principal.HomeTenant.ID,
		ActorKind:      principal.Kind,
		ActorUserID:    principal.User.ID,
		ActorSessionID: principal.SessionID,
		Action:         "auth.logout",
		ResourceType:   "session",
		ResourceID:     principal.SessionID,
		Result:         "success",
	})
	c.Status(http.StatusNoContent)
}
