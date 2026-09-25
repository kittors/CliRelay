package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/authevents"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/ipaccess"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	log "github.com/sirupsen/logrus"
)

// healthProbePaths are never subject to the deny list.
//
// A blocked health probe does not just hide a problem, it creates one: the
// blue/green deploy script gates cutover on /readyz, so a deny rule that
// happened to cover the load balancer would turn every future deployment into a
// rollback. Likewise the arbiter's DNS watcher reads a refused /readyz/egress
// as broken egress and pulls the node from DNS.
var healthProbePaths = map[string]struct{}{
	"/healthz":       {},
	"/readyz":        {},
	"/readyz/egress": {},
}

// untrustedAdmissionWarn deduplicates the "list not enforced" warning, which
// otherwise fires on literally every request of a misconfigured deployment.
var untrustedAdmissionWarn struct {
	mu   sync.Mutex
	last time.Time
}

const untrustedWarnInterval = 10 * time.Minute

// ipAccessMiddleware enforces the IP allow/deny list ahead of routing.
//
// It runs before authentication on purpose: a source an operator has banned
// should not be able to spend the server's CPU on credential comparison, and a
// ban that only covered the login endpoint would leave the business API open to
// the same source.
func ipAccessMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		registry := ipaccess.Default()
		if registry == nil {
			c.Next()
			return
		}
		if c.Request != nil {
			if _, exempt := healthProbePaths[c.Request.URL.Path]; exempt {
				c.Next()
				return
			}
		}

		address := registry.ResolveAddress(c.Request, c.ClientIP())
		verdict := registry.Evaluate(address)
		ipaccess.StoreContext(c, ipaccess.RequestContext{Address: address, Verdict: verdict})
		if c.Request != nil {
			// Also on the request context so audit records downstream can read the
			// source address without every call site passing it along.
			c.Request = c.Request.WithContext(ipaccess.WithAddress(c.Request.Context(), address))
		}

		if !verdict.Enforced && verdict.Reason != "" && registry.Matcher().Count() > 0 {
			warnListNotEnforced(verdict.Reason, address)
		}
		if verdict.Allowed() {
			c.Next()
			return
		}

		recordBlocked(c, address, verdict)
		// The body deliberately does not say which rule matched or when it
		// expires: that is operator information, and echoing it back would let a
		// banned source measure the ban and plan around it.
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"code":    "ip_forbidden",
				"message": "access from this address is not permitted",
			},
		})
	}
}

func recordBlocked(c *gin.Context, address ipaccess.ClientAddress, verdict ipaccess.Verdict) {
	reason := "deny rule"
	if verdict.Rule == nil {
		reason = "lockdown: address not on the allow list"
	} else if verdict.Rule.Reason != "" {
		reason = verdict.Rule.Reason
	}
	event := authevents.Event{
		IP:      address.Raw,
		Trusted: address.Trusted,
		Surface: authevents.SurfaceRequest,
		Outcome: authevents.OutcomeBlocked,
		Reason:  reason,
	}
	if verdict.Rule != nil {
		event.IPPrefix = verdict.Rule.CIDR
	} else {
		event.IPPrefix = ipaccess.BanCIDR(address.IP)
	}
	if c.Request != nil {
		event.RequestPath = c.Request.URL.Path
		event.UserAgent = c.Request.UserAgent()
		event.RequestID = logging.GetGinRequestID(c)
	}
	authevents.Record(event)
}

func warnListNotEnforced(reason string, address ipaccess.ClientAddress) {
	now := time.Now()
	untrustedAdmissionWarn.mu.Lock()
	if !untrustedAdmissionWarn.last.IsZero() && now.Sub(untrustedAdmissionWarn.last) < untrustedWarnInterval {
		untrustedAdmissionWarn.mu.Unlock()
		return
	}
	untrustedAdmissionWarn.last = now
	untrustedAdmissionWarn.mu.Unlock()

	// The remediation is spelled out because the failure is silent by nature:
	// rules exist, the panel lists them, and nothing enforces them.
	hint := address.Raw
	if hint == "" {
		hint = "the proxy address"
	}
	log.Warnf("ip-access: rules are configured but NOT enforced because %s. Every client currently reports %s. Fix: set trusted-proxies to your reverse proxy's CIDR (for example [\"%s/32\"]) so the real client address is resolved.",
		reason, hint, hint)
}
