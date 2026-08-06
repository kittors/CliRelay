package management

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

// credentialKind separates "a secret was supplied and compared" from "no secret
// was supplied at all". Only the former carries any evidence about the secret.
type credentialKind uint8

const (
	credentialMissing credentialKind = iota
	credentialProvided
)

// managementAuthMiddleware enforces access control for management endpoints.
// All requests (local and remote) require a valid management key; remote access
// additionally requires allow-remote-management=true.
//
// The step order below is load-bearing and must not be reordered:
//   - the ban check (step 5) precedes every secret comparison, so a banned client
//     cannot spend a bcrypt compare (~70ms of one core) per request;
//   - credential-less requests (step 8) are charged to their own bucket and can
//     only ever be soft-limited, so crawlers and SPA races cannot ban a whole
//     office behind one egress IP.
func (h *Handler) managementAuthMiddleware(c *gin.Context) {
	// 1) Version headers stay ahead of authentication: the panel reads them on the
	// 401 path to detect a backend/UI version skew.
	c.Header("X-CPA-VERSION", buildinfo.Version)
	c.Header("X-CPA-COMMIT", buildinfo.Commit)
	c.Header("X-CPA-BUILD-DATE", buildinfo.BuildDate)
	currentUIVersion, currentUICommit := h.currentFrontendState()
	c.Header("X-CPA-UI-VERSION", currentUIVersion)
	c.Header("X-CPA-UI-COMMIT", currentUICommit)

	// 2) Session tokens (cps_*) come from Authorization: Bearer on normal HTTP, or from
	// ?token= on the system-stats WebSocket handshake (browsers cannot set WS headers).
	// Resolve the session token before falling through to management-key auth so a
	// valid user session is never bcrypt-compared against the remote management secret.
	//
	// Known design boundary (not fixable in isolation): the whole cps_ session system
	// is outside the allow-remote gate, because /v0/auth/login does not pass through
	// this middleware at all. Gating only this branch would produce accounts that can
	// log in and then get 403 on every request, which is a worse outage than the gap.
	if token := resolveSessionToken(c); strings.HasPrefix(token, "cps_") {
		_ = h.authenticateSessionToken(c, token)
		return
	}

	// 3) A loopback ClientIP alone does not prove local origin. With trusted-proxies
	// unset (the default) gin falls back to RemoteAddr, so behind a same-host
	// reverse proxy every external request would report 127.0.0.1 and thereby skip
	// the allow-remote gate, unlock the local-password path, and bypass the per-IP
	// login throttle entirely. Require the direct peer to be loopback and no relay
	// headers to be present as well.
	peer := resolveThrottlePeer(c)
	localClient := (peer == "127.0.0.1" || peer == "::1") && util.IsLocalOriginRequest(c.Request)

	cfg := h.cfg
	var (
		allowRemote bool
		secretHash  string
	)
	if cfg != nil {
		allowRemote = cfg.RemoteManagement.AllowRemote
		secretHash = cfg.RemoteManagement.SecretKey
	}
	if h.allowRemoteOverride {
		allowRemote = true
	}
	envSecret := h.envSecret

	now := time.Now()
	var mgmtKey throttleKey
	if !localClient {
		// 4) Remote access gate.
		if !allowRemote {
			// Requests relayed by a local reverse proxy land here even though the TCP
			// peer is loopback. allow-remote is the only advice that actually grants
			// access: a relayed request is never treated as local, so trusted-proxies
			// does not help on its own — it recovers the real client IP for the per-IP
			// throttle, which is why it is named as an additional step rather than an
			// alternative.
			message := "remote management disabled"
			if relayHeader := util.RelayIndicationHeader(c.Request); relayHeader != "" {
				message = "remote management disabled: request was relayed (" + relayHeader + " header present), so it is not treated as local. Set remote-management.allow-remote=true to allow it, and list the proxy in trusted-proxies so per-IP throttling sees the real client."
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": message})
			return
		}

		// 5) Ban pre-check. This must stay the first thing that touches a remote
		// request's credentials budget: everything below it either hashes a password
		// or compares a stored secret, and an already-banned client must not be able
		// to make the server pay for that.
		mgmtKey = h.clientThrottleKey(c, scopeManagementKey)
		if d := h.loginThrottle.evaluate(mgmtKey, now); d.Outcome != outcomeAllow {
			h.logAuthFailure(c, mgmtKey, d)
			abortThrottled(c, d)
			return
		}
	}

	// 6) Nothing to compare against at all.
	localPasswordConfigured := localClient && h.localPassword != ""
	if secretHash == "" && envSecret == "" && !localPasswordConfigured {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "remote management key not set"})
		return
	}

	// 7) Extract the credential.
	provided, kind := classifyManagementCredential(c)

	// 8) No credential was supplied. This proves nothing about the secret, so it is
	// charged to the credential-less bucket, which can only ever rate-limit.
	if kind == credentialMissing {
		if !localClient {
			key := h.clientThrottleKey(c, scopeUnauthenticated)
			if d := h.loginThrottle.recordFailure(key, now); d.Outcome != outcomeAllow {
				h.logAuthFailure(c, key, d)
				abortThrottled(c, d)
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing management key"})
		return
	}

	// 9) A credential was supplied: compare it against every configured secret.
	if localClient {
		if lp := h.localPassword; lp != "" {
			if util.ConstantTimeStringEqual(provided, lp) {
				h.setServicePrincipal(c)
				h.nextWithManagementAudit(c)
				return
			}
		}
	}

	if envSecret != "" && util.ConstantTimeStringEqual(provided, envSecret) {
		h.loginThrottle.recordSuccess(mgmtKey)
		h.setServicePrincipal(c)
		h.nextWithManagementAudit(c)
		return
	}

	if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
		if !localClient {
			d := h.loginThrottle.recordFailure(mgmtKey, now)
			h.logAuthFailure(c, mgmtKey, d)
			if d.Outcome != outcomeAllow {
				abortThrottled(c, d)
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid management key"})
		return
	}

	h.loginThrottle.recordSuccess(mgmtKey)
	h.setServicePrincipal(c)
	h.nextWithManagementAudit(c)
}

// classifyManagementCredential draws the line at "was a stored secret compared".
// Missing or unparseable credentials are format errors produced by crawlers,
// health checks, prefetch and SPA races — never evidence of guessing.
func classifyManagementCredential(c *gin.Context) (string, credentialKind) {
	// Accept either Authorization: Bearer <key> or X-Management-Key.
	var provided string
	if ah := c.GetHeader("Authorization"); ah != "" {
		parts := strings.SplitN(ah, " ", 2)
		if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
			provided = parts[1]
		} else {
			provided = ah
		}
	}
	if provided == "" {
		provided = c.GetHeader("X-Management-Key")
	}
	// Fallback: ?token= query param is accepted only for WebSocket handshakes;
	// normal HTTP requests must not carry credentials in URLs.
	// Session tokens (cps_*) were already handled by the caller; remaining query
	// tokens are treated as management keys (legacy panel / service credential).
	if provided == "" && shouldReadManagementTokenFromQuery(c) {
		if queryToken := strings.TrimSpace(c.Query("token")); !strings.HasPrefix(queryToken, "cps_") {
			provided = queryToken
		}
	}
	if provided == "" {
		return "", credentialMissing
	}
	return provided, credentialProvided
}

// abortThrottled writes the throttle rejection. The two shapes are a cross-repo
// contract with the admin panel and must not drift.
func abortThrottled(c *gin.Context, d throttleDecision) {
	retryAfter := d.RetryAfter.Round(time.Second)
	c.Header("Retry-After", retryAfterSecondsHeader(retryAfter))
	if d.Outcome == outcomeBlocked {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": fmt.Sprintf("IP banned due to too many failed attempts. Try again in %s", retryAfter),
		})
		return
	}
	// The 429 message must never contain "IP banned": the panel's shouldSuspendAuth
	// regex-matches that string and forces a logout, and a soft rate limit is
	// precisely the case where the session is still valid and must survive.
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"code":    "too_many_requests",
			"message": fmt.Sprintf("too many requests, retry in %s", retryAfter),
		},
	})
}

// logAuthFailure emits the operator-facing record of a throttle event.
//
// This is the one place in the management package that uses structured fields
// instead of the package-wide printf style: the fields are consumed by log
// search when investigating a lockout, where "which bucket, which key, how far
// from the limit" has to be filterable rather than embedded in a sentence.
//
// Level is Debug by default and Warn only on the transition into a block, so a
// brute-force run produces one Warn instead of one line per rejected request.
func (h *Handler) logAuthFailure(c *gin.Context, key throttleKey, d throttleDecision) {
	shortLimit, longLimit := h.loginThrottle.limitsFor(key)
	fields := log.Fields{
		"scope":       key.Scope.String(),
		"key":         maskThrottleKey(key),
		"shared":      key.Shared,
		"short_count": d.ShortCount,
		"long_count":  d.LongCount,
		"limit":       shortLimit,
		"long_limit":  longLimit,
		"outcome":     d.Outcome.String(),
	}
	if c != nil && c.Request != nil {
		userAgent := c.Request.UserAgent()
		if len(userAgent) > 120 {
			userAgent = userAgent[:120]
		}
		fields["path"] = c.Request.URL.Path
		fields["method"] = c.Request.Method
		fields["ua"] = userAgent
		fields["relay_header"] = util.RelayIndicationHeader(c.Request)
		fields["request_id"] = throttleRequestID(c)
	}
	if d.NewlyArmed {
		fields["block_for"] = d.RetryAfter.Round(time.Second).String()
		fields["generation"] = d.Generation
		log.WithFields(fields).Warn("auth-throttle: credential failure armed a block")
		return
	}
	log.WithFields(fields).Debug("auth-throttle: credential failure")
}

// throttleRequestID reads the correlation id assigned upstream. It deliberately
// does not mint one when none exists: an id that appears in a single log line
// and in no response header or audit row would only look correlatable.
func throttleRequestID(c *gin.Context) string {
	if id := logging.GetGinRequestID(c); id != "" {
		return id
	}
	if c.Request != nil {
		return logging.GetRequestID(c.Request.Context())
	}
	return ""
}
