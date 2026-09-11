package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
)

// Portal auth + key management

const portalPrincipalKey = "portalEndUser"
const portalSessionKey = "portalSessionID"

// isPortalGuessFailure reports whether a portal login error means a stored
// password was actually compared and did not match.
//
// The throttle used to infer this from the HTTP status instead, charging the
// guess budget for any 401 the route produced. That swept in the refresh
// endpoint returning 401 for a session the server itself had just revoked: a
// user whose password an admin had reset arrived at the login form having
// already spent one attempt on a failure that was not theirs. A cooldown, a
// disabled account and an infrastructure error carry no information about the
// password either, so none of them may consume the budget.
func isPortalGuessFailure(err error) bool {
	return errors.Is(err, enduser.ErrInvalidCredentials)
}

func (h *Handler) PostPortalLogin(c *gin.Context) {
	now := time.Now()
	// Evaluated before the body is even parsed: Login runs an unconditional
	// bcrypt compare (cost 10) for every request, including ones naming a user
	// that does not exist, so a throttled caller must never reach it.
	ipKey := h.clientThrottleKey(c, scopePortalPassword)
	if d := h.throttleEvaluate(c, ipKey, now); d.Outcome != outcomeAllow {
		h.noteThrottledAttempt(c, ipKey, "")
		abortLoginThrottled(c, d)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Username) == "" || body.Password == "" {
		endUserError(c, enduser.ErrInvalidCredentials)
		return
	}
	// The account bucket is the layer that actually stops guessing: it survives
	// an attacker rotating source addresses, which the per-IP bucket cannot.
	// Behind a reverse proxy with no trusted-proxies configured every client
	// shares one IP bucket, which makes this the only bucket that discriminates.
	acctKey := accountThrottleKey(scopePortalAccount, enduser.NormalizeUsername(body.Username))
	if d := h.throttleEvaluate(c, acctKey, now); d.Outcome != outcomeAllow {
		h.noteThrottledAttempt(c, acctKey, body.Username)
		abortLoginThrottled(c, d)
		return
	}
	result, err := svc.Login(c.Request.Context(), body.Username, body.Password, c.GetHeader("User-Agent"))
	if err != nil {
		if isPortalGuessFailure(err) {
			h.throttleCharge(c, ipKey, now)
			d := h.throttleCharge(c, acctKey, now)
			h.logAuthFailure(c, acctKey, d)
			// One attempt, one record: the two charges above are two views of
			// the same failure. The username is passed through so an operator
			// can answer "why can this person not log in" from the attempt log
			// instead of correlating web-server access logs by timestamp.
			h.noteCredentialFailure(c, acctKey, d, body.Username, "invalid portal credentials")
		}
		endUserError(c, err)
		return
	}
	// NIST SP 800-63B 5.2.2: a success clears that account's failures, never the
	// per-IP quota.
	h.loginThrottle.recordSuccess(acctKey)
	h.noteAuthSuccess(c, acctKey, body.Username)
	c.JSON(http.StatusOK, result)
}

func (h *Handler) PostPortalRefresh(c *gin.Context) {
	// Its own bucket, never the password bucket. Refresh tokens are 256-bit
	// random values, so a failure here is almost always an expired or revoked
	// session rather than a guess; charging it against the login budget meant
	// routine session expiry ate into the attempts a user needed to sign in.
	refreshKey := h.clientThrottleKey(c, scopeRefresh)
	if d := h.throttleCharge(c, refreshKey, time.Now()); d.Outcome != outcomeAllow {
		h.noteNonCredentialFailure(c, refreshKey, d, "portal refresh endpoint rate limit")
		abortThrottled(c, d)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.RefreshToken) == "" {
		endUserError(c, enduser.ErrSessionRevoked)
		return
	}
	result, err := svc.Refresh(c.Request.Context(), body.RefreshToken, c.GetHeader("User-Agent"))
	if err != nil {
		endUserError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) authenticatePortal(c *gin.Context) (enduser.User, string, bool) {
	svc := h.endUserService()
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "end user service unavailable"})
		return enduser.User{}, "", false
	}
	token := bearerToken(c)
	user, sessionID, err := svc.Authenticate(c.Request.Context(), token)
	if err != nil {
		endUserError(c, err)
		return enduser.User{}, "", false
	}
	c.Set(portalPrincipalKey, user)
	c.Set(portalSessionKey, sessionID)
	return user, sessionID, true
}

// portalKeyAccess requires an active portal session that is allowed to manage keys.
func (h *Handler) portalKeyAccess(c *gin.Context) (enduser.User, bool) {
	user, _, ok := h.authenticatePortal(c)
	if !ok {
		return enduser.User{}, false
	}
	if user.MustChangePassword {
		endUserError(c, enduser.ErrMustChangePassword)
		return enduser.User{}, false
	}
	return user, true
}

func (h *Handler) PostPortalLogout(c *gin.Context) {
	_, sessionID, ok := h.authenticatePortal(c)
	if !ok {
		return
	}
	_ = h.endUserService().Logout(c.Request.Context(), sessionID)
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetPortalMe(c *gin.Context) {
	user, _, ok := h.authenticatePortal(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": user})
}

func (h *Handler) PutPortalPassword(c *gin.Context) {
	user, sessionID, ok := h.authenticatePortal(c)
	if !ok {
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := h.endUserService().ChangePassword(c.Request.Context(), user, sessionID, body.CurrentPassword, body.NewPassword); err != nil {
		endUserError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
