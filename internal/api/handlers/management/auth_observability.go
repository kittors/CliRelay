package management

import (
	"context"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authevents"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/ipaccess"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// autoBanWriteTimeout bounds the database write that arms an automatic ban. It
// uses its own context rather than the request's: the client being rejected is
// often the one that disconnects, and a ban that vanished because the attacker
// hung up would be worse than no ban at all.
const autoBanWriteTimeout = 5 * time.Second

// surfaceForScope maps a throttle bucket to the door it guards, so callers never
// pass a surface string that could drift from the scope it describes.
func surfaceForScope(scope throttleScope) string {
	switch scope {
	case scopeManagementKey:
		return authevents.SurfaceManagementKey
	case scopeUserPassword, scopeUserAccount:
		return authevents.SurfaceAdminLogin
	case scopePortalPassword, scopePortalAccount:
		return authevents.SurfacePortalLogin
	case scopeRefresh:
		return authevents.SurfaceRefresh
	default:
		return authevents.SurfaceRequest
	}
}

// throttleEvaluate reports a bucket's standing, honouring allow-list exemption.
//
// An allow-listed source skips the throttle entirely rather than getting a
// larger budget: the operator has asserted the address is theirs, and the point
// of the exemption is that an office NAT behind one address cannot lock itself
// out by collective typo.
func (h *Handler) throttleEvaluate(c *gin.Context, key throttleKey, now time.Time) throttleDecision {
	if ipaccess.ExemptFromThrottle(c) {
		return throttleDecision{Scope: key.Scope}
	}
	return h.loginThrottle.evaluate(key, now)
}

// throttleCharge charges one failure against a bucket, honouring exemption.
//
// Charging and observability are separate calls because a single failed login
// charges two buckets (per-IP and per-account) but is still one attempt: folding
// the event write into the charge would double every row in the attempt log and
// halve the effective auto-ban threshold.
func (h *Handler) throttleCharge(c *gin.Context, key throttleKey, now time.Time) throttleDecision {
	if ipaccess.ExemptFromThrottle(c) {
		return throttleDecision{Scope: key.Scope}
	}
	return h.loginThrottle.recordFailure(key, now)
}

// noteCredentialFailure records one real credential failure: exactly one attempt
// row and one auto-ban charge. Call it once per attempt, never once per bucket.
func (h *Handler) noteCredentialFailure(c *gin.Context, key throttleKey, decision throttleDecision, username, reason string) {
	if ipaccess.ExemptFromThrottle(c) {
		return
	}
	outcome := authevents.OutcomeFailure
	if decision.Outcome != outcomeAllow {
		outcome = authevents.OutcomeThrottled
	}
	h.recordAuthAttempt(c, key, outcome, username, reason)
	h.feedAutoBan(c, key, username, reason)
}

// noteNonCredentialFailure records a rejection that proves nothing about any
// secret — a missing credential, a malformed refresh token — so it is logged for
// visibility but never counted towards a ban.
func (h *Handler) noteNonCredentialFailure(c *gin.Context, key throttleKey, decision throttleDecision, reason string) {
	if ipaccess.ExemptFromThrottle(c) {
		return
	}
	outcome := authevents.OutcomeFailure
	if decision.Outcome != outcomeAllow {
		outcome = authevents.OutcomeThrottled
	}
	h.recordAuthAttempt(c, key, outcome, "", reason)
}

// noteThrottledAttempt records an attempt turned away by an already-armed block.
// The throttle deliberately does not count these, but they are the bulk of an
// attack's traffic and the only evidence of how long it kept going.
func (h *Handler) noteThrottledAttempt(c *gin.Context, key throttleKey, username string) {
	if ipaccess.ExemptFromThrottle(c) {
		return
	}
	h.recordAuthAttempt(c, key, authevents.OutcomeThrottled, username, "rejected by an armed throttle")
}

// noteAuthSuccess records a successful authentication.
//
// Only interactive logins are recorded, not every management-key request: the
// key is verified on every management API call, so logging those successes would
// bury the attempt log under ordinary panel traffic. "This source failed ninety
// times and then succeeded" is the line worth keeping.
func (h *Handler) noteAuthSuccess(c *gin.Context, key throttleKey, username string) {
	h.recordAuthAttempt(c, key, authevents.OutcomeSuccess, username, "")
}

func (h *Handler) recordAuthAttempt(c *gin.Context, key throttleKey, outcome, username, reason string) {
	recorder := authevents.Default()
	if recorder == nil {
		return
	}
	address := ipaccess.AddressFor(c)
	if username == "" && (key.Scope == scopeUserAccount || key.Scope == scopePortalAccount) {
		username = key.Value
	}
	event := authevents.Event{
		IP:       address.Raw,
		IPPrefix: ipaccess.BanCIDR(address.IP),
		Trusted:  address.Trusted,
		Scope:    key.Scope.String(),
		Surface:  surfaceForScope(key.Scope),
		Username: username,
		Outcome:  outcome,
		Reason:   reason,
	}
	if c != nil && c.Request != nil {
		event.RequestPath = c.Request.URL.Path
		event.UserAgent = c.Request.UserAgent()
		event.RequestID = logging.GetGinRequestID(c)
	}
	recorder.Record(event)
}

func (h *Handler) feedAutoBan(c *gin.Context, key throttleKey, username, reason string) {
	registry := ipaccess.Default()
	if registry == nil {
		return
	}
	address := ipaccess.AddressFor(c)
	if address.IP == nil {
		return
	}
	if reason == "" {
		reason = "repeated authentication failures (" + key.Scope.String() + ")"
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoBanWriteTimeout)
	defer cancel()
	outcome := registry.AutoBan().RecordFailure(ctx, address, reason)
	if !outcome.Triggered {
		return
	}
	result := authevents.OutcomeWouldBan
	if outcome.Enforced {
		result = authevents.OutcomeAutoBanned
	}
	h.recordAuthAttempt(c, key, result, username,
		fmt.Sprintf("auto-ban rule matched %s after %d failures", outcome.CIDR, outcome.Failures))
}
