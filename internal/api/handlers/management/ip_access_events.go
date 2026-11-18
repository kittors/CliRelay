package management

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authevents"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/ipaccess"
)

// summaryWindows are the aggregation ranges the panel offers. Bounded to a
// fixed set so a hand-crafted query cannot ask the database to group over the
// entire retention period.
var summaryWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

const defaultSummaryWindow = "24h"

func authEventsUnavailable(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{"code": "auth_events_unavailable", "message": "authentication attempt history requires the database backend"},
	})
}

// GetAuthAttempts lists individual authentication attempts.
func (h *Handler) GetAuthAttempts(c *gin.Context) {
	recorder := authevents.Default()
	if recorder == nil || !recorder.Available() {
		authEventsUnavailable(c)
		return
	}
	filter := authevents.ListFilter{
		IPPrefix: strings.TrimSpace(c.Query("ip")),
		Username: strings.TrimSpace(c.Query("username")),
		Outcome:  strings.TrimSpace(c.Query("outcome")),
		Surface:  strings.TrimSpace(c.Query("surface")),
	}
	filter.Page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	filter.Size, _ = strconv.Atoi(c.DefaultQuery("size", "50"))
	if window, ok := summaryWindows[strings.TrimSpace(c.Query("window"))]; ok {
		filter.Since = time.Now().Add(-window)
	}

	events, total, err := recorder.List(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "list_failed", "message": err.Error()}})
		return
	}
	if events == nil {
		events = []authevents.Event{}
	}
	c.JSON(http.StatusOK, gin.H{"items": events, "total": total, "page": filter.Page, "size": filter.Size})
}

// GetAuthAttemptSummary ranks sources by failure volume and annotates each with
// its current standing in the rule list, so "who is attacking us" and "what have
// I already done about it" are one answer instead of two screens.
func (h *Handler) GetAuthAttemptSummary(c *gin.Context) {
	recorder := authevents.Default()
	if recorder == nil || !recorder.Available() {
		authEventsUnavailable(c)
		return
	}
	windowKey := strings.TrimSpace(c.DefaultQuery("window", defaultSummaryWindow))
	window, ok := summaryWindows[windowKey]
	if !ok {
		windowKey = defaultSummaryWindow
		window = summaryWindows[defaultSummaryWindow]
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))

	summaries, err := recorder.Summarize(c.Request.Context(), time.Now().Add(-window), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "summary_failed", "message": err.Error()}})
		return
	}
	if summaries == nil {
		summaries = []authevents.SourceSummary{}
	}
	annotateWithRules(summaries)
	c.JSON(http.StatusOK, gin.H{"items": summaries, "window": windowKey})
}

// annotateWithRules fills in each source's current rule standing using the live
// matcher rather than a SQL join, so a source covered by a /16 deny is reported
// as blocked instead of appearing unhandled because no rule names it exactly.
func annotateWithRules(summaries []authevents.SourceSummary) {
	matcher := ipaccess.Default().Matcher()
	if matcher == nil {
		return
	}
	for i := range summaries {
		probe := ipaccess.ParseIPForMatch(summaries[i].SampleIP)
		if probe == nil {
			continue
		}
		decision, rule := matcher.Match(probe)
		if decision == ipaccess.DecisionNeutral {
			continue
		}
		summaries[i].RuleEffect = decision.String()
		if rule != nil {
			summaries[i].RuleID = rule.ID
			summaries[i].RuleExpires = rule.ExpiresAt
		}
	}
}

// GetIPAccessPolicy returns the current protection policy.
func (h *Handler) GetIPAccessPolicy(c *gin.Context) {
	registry := ipaccess.Default()
	if registry == nil {
		ipAccessUnavailable(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"policy":   registry.Policy(),
		"throttle": h.throttlePolicySnapshot(),
	})
}

// PutIPAccessPolicy updates the protection policy.
func (h *Handler) PutIPAccessPolicy(c *gin.Context) {
	registry := ipaccess.Default()
	if registry == nil {
		ipAccessUnavailable(c)
		return
	}
	var body ipaccess.ProtectionPolicy
	if err := c.ShouldBindJSON(&body); err != nil {
		ipAccessBadRequest(c, err)
		return
	}
	current := registry.Policy()

	// Self-lockout guard. Turning on lockdown while the requester is not on the
	// allow list locks the operator out on their next request, and the only
	// recovery is editing the database by hand. Refuse, and say what to add.
	if body.Lockdown && !current.Lockdown {
		address := ipaccess.AddressFor(c)
		if !address.Trusted && !address.Loopback() {
			ipAccessBadRequest(c, errors.New("lockdown cannot be enabled while the client address is untrusted: configure trusted-proxies first, or the rules cannot identify anyone"))
			return
		}
		if !address.Loopback() && !registry.Matcher().AllowsAddress(address.IP) {
			ipAccessBadRequest(c, errors.New("lockdown would lock you out: add an allow rule covering "+ipaccess.BanCIDR(address.IP)+" first"))
			return
		}
	}

	if err := registry.UpdatePolicy(c.Request.Context(), body); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "policy_save_failed", "message": err.Error()}})
		return
	}
	// Throttle overrides take effect on the running limiter immediately; they
	// are otherwise indistinguishable from "saved but ignored".
	h.applyThrottleOverride(registry.Policy().Throttle)
	c.JSON(http.StatusOK, gin.H{"policy": registry.Policy(), "throttle": h.throttlePolicySnapshot()})
}
