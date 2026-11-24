package management

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/authevents"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/ipaccess"
)

func ipAccessUnavailable(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{"code": "ip_access_unavailable", "message": "ip access control requires the database backend"},
	})
}

func ipAccessBadRequest(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{"code": "invalid_rule", "message": err.Error()},
	})
}

// GetIPAccessRules lists allow/deny rules.
func (h *Handler) GetIPAccessRules(c *gin.Context) {
	registry := ipaccess.Default()
	if registry == nil {
		ipAccessUnavailable(c)
		return
	}
	filter := ipaccess.ListFilter{
		Effect: c.Query("effect"),
		Source: c.Query("source"),
		Search: c.Query("search"),
	}
	if raw := strings.TrimSpace(c.Query("enabled")); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			filter.Enabled = &enabled
		}
	}
	filter.Page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	filter.Size, _ = strconv.Atoi(c.DefaultQuery("size", "50"))

	rules, total, err := registry.Store().List(c.Request.Context(), filter)
	if err != nil {
		writeInternalErrorWithCode(c, "list_failed", "查询 IP 访问控制规则", err)
		return
	}
	if rules == nil {
		rules = []ipaccess.Rule{}
	}
	c.JSON(http.StatusOK, gin.H{"items": rules, "total": total, "page": filter.Page, "size": filter.Size})
}

// PostIPAccessRule creates a rule.
func (h *Handler) PostIPAccessRule(c *gin.Context) {
	registry := ipaccess.Default()
	if registry == nil {
		ipAccessUnavailable(c)
		return
	}
	var body struct {
		CIDR      string `json:"cidr"`
		Effect    string `json:"effect"`
		Note      string `json:"note"`
		Reason    string `json:"reason"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		ipAccessBadRequest(c, err)
		return
	}
	effect, err := ipaccess.ParseEffect(body.Effect)
	if err != nil {
		ipAccessBadRequest(c, err)
		return
	}
	input := ipaccess.CreateInput{
		CIDR:   body.CIDR,
		Effect: effect,
		Source: ipaccess.SourceManual,
		Reason: body.Reason,
		Note:   body.Note,
	}
	if trimmed := strings.TrimSpace(body.ExpiresAt); trimmed != "" {
		expires, parseErr := time.Parse(time.RFC3339, trimmed)
		if parseErr != nil {
			ipAccessBadRequest(c, parseErr)
			return
		}
		input.ExpiresAt = &expires
	}
	if principal, ok := principalFromContext(c); ok {
		input.CreatedBy = principal.User.ID
	}

	// Refuse to deny an address the deployment depends on. These are exactly the
	// rules an operator writes by accident while under attack, and each of them
	// takes the service down rather than stopping anyone.
	if effect == ipaccess.EffectDeny {
		if entry, covered := coversProtectedAddress(input.CIDR); covered {
			ipAccessBadRequest(c, fmt.Errorf("this rule would cover %s, which is protected (%s) and cannot be denied", entry.CIDR, entry.Reason))
			return
		}
		if blocked, checkErr := selfDenyCheck(c, input.CIDR); checkErr != nil {
			ipAccessBadRequest(c, checkErr)
			return
		} else if blocked {
			ipAccessBadRequest(c, errors.New("this rule would deny your own address; add it as an allow rule first or use a narrower range"))
			return
		}
	}

	rule, err := registry.Store().Create(c.Request.Context(), input)
	if err != nil {
		if errors.Is(err, ipaccess.ErrDuplicateRule) {
			c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "duplicate_rule", "message": err.Error()}})
			return
		}
		ipAccessBadRequest(c, err)
		return
	}
	if refreshErr := registry.Refresh(c.Request.Context()); refreshErr != nil {
		// The row is written; the snapshot will catch up on the next tick.
		c.JSON(http.StatusCreated, gin.H{"rule": rule, "warning": "rule saved but the in-memory snapshot could not be refreshed immediately"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"rule": rule})
}

// coversProtectedAddress reports whether a proposed CIDR would swallow any
// address the process must never reject.
func coversProtectedAddress(cidr string) (ipaccess.ProtectedEntry, bool) {
	normalized, _, err := ipaccess.NormalizeCIDR(cidr)
	if err != nil {
		return ipaccess.ProtectedEntry{}, false
	}
	_, network, parseErr := net.ParseCIDR(normalized)
	if parseErr != nil || network == nil {
		return ipaccess.ProtectedEntry{}, false
	}
	registry := ipaccess.Default()
	for _, entry := range registry.ProtectedEntries() {
		probe := ipaccess.ParseIPForMatch(entry.CIDR[:strings.Index(entry.CIDR, "/")])
		if probe != nil && network.Contains(probe) {
			return entry, true
		}
	}
	return ipaccess.ProtectedEntry{}, false
}

// selfDenyCheck reports whether a proposed deny rule would cover the requester.
func selfDenyCheck(c *gin.Context, cidr string) (bool, error) {
	normalized, _, err := ipaccess.NormalizeCIDR(cidr)
	if err != nil {
		return false, err
	}
	address := ipaccess.AddressFor(c)
	if address.IP == nil || !address.Trusted {
		// An untrusted address cannot be compared meaningfully, and rules are not
		// enforced in that state anyway.
		return false, nil
	}
	candidate := ipaccess.Rule{CIDR: normalized, Effect: ipaccess.EffectDeny, Enabled: true}
	probe := ipaccess.NewMatcher([]ipaccess.Rule{candidate}, false, time.Now())
	decision, _ := probe.Match(address.IP)
	return decision == ipaccess.DecisionDeny, nil
}

// PatchIPAccessRule updates the mutable fields of a rule.
func (h *Handler) PatchIPAccessRule(c *gin.Context) {
	registry := ipaccess.Default()
	if registry == nil {
		ipAccessUnavailable(c)
		return
	}
	var body struct {
		Enabled   *bool   `json:"enabled"`
		Note      *string `json:"note"`
		ExpiresAt *string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		ipAccessBadRequest(c, err)
		return
	}
	input := ipaccess.UpdateInput{Enabled: body.Enabled, Note: body.Note}
	if body.ExpiresAt != nil {
		if strings.TrimSpace(*body.ExpiresAt) == "" {
			input.ClearExpires = true
		} else {
			expires, err := time.Parse(time.RFC3339, *body.ExpiresAt)
			if err != nil {
				ipAccessBadRequest(c, err)
				return
			}
			pointer := &expires
			input.ExpiresAt = &pointer
		}
	}
	rule, err := registry.Store().Update(c.Request.Context(), c.Param("id"), input)
	if err != nil {
		if errors.Is(err, ipaccess.ErrRuleNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "rule_not_found", "message": err.Error()}})
			return
		}
		writeInternalErrorWithCode(c, "update_failed", "更新 IP 访问控制规则", err)
		return
	}
	_ = registry.Refresh(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"rule": rule})
}

// DeleteIPAccessRule removes a rule.
func (h *Handler) DeleteIPAccessRule(c *gin.Context) {
	registry := ipaccess.Default()
	if registry == nil {
		ipAccessUnavailable(c)
		return
	}
	if err := registry.Store().Delete(c.Request.Context(), c.Param("id")); err != nil {
		if errors.Is(err, ipaccess.ErrRuleNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "rule_not_found", "message": err.Error()}})
			return
		}
		writeInternalErrorWithCode(c, "delete_failed", "删除 IP 访问控制规则", err)
		return
	}
	_ = registry.Refresh(c.Request.Context())
	c.Status(http.StatusNoContent)
}

// GetIPAccessStatus reports whether the list is actually in force.
//
// This endpoint exists because the feature has a silent failure mode: behind an
// unconfigured reverse proxy every rule is stored, listed and displayed, and
// none of it is enforced. The panel renders this response as a banner so that
// state is impossible to miss rather than something an operator discovers after
// an incident.
func (h *Handler) GetIPAccessStatus(c *gin.Context) {
	registry := ipaccess.Default()
	if registry == nil {
		ipAccessUnavailable(c)
		return
	}
	address := ipaccess.AddressFor(c)
	matcher := registry.Matcher()
	policy := registry.Policy()
	trustedProxies, proxySource := registry.TrustedProxies()

	response := gin.H{
		"client_ip":                  address.Raw,
		"trusted":                    address.Trusted,
		"relay_header":               address.RelayHeader,
		"trusted_proxies_configured": registry.ProxyTrusted(),
		"trusted_proxies":            trustedProxies,
		// Where the live list came from: "database" (panel-managed), "config"
		// (config.yaml fallback) or "none". The panel needs it to explain why an
		// edit here may not be what is in force.
		"trusted_proxies_source": proxySource,
		// Diagnostics: an operator cannot declare the right proxies without
		// seeing what the chain actually contains. Guessing produced a
		// half-configured chain that resolved to loopback and quietly exempted
		// every external client.
		"peer":              address.Peer,
		"forwarded_chain":   address.Chain,
		"direct_peer":       address.DirectPeer,
		"enforced":          address.Trusted || address.Loopback(),
		"lockdown":          policy.Lockdown,
		"active_rules":      matcher.Count(),
		"storage_available": registry.Store().Available(),
		"auto_ban_mode":     policy.AutoBan.Mode,
	}
	if !address.Trusted {
		// Suggest the hop the chain actually stopped at: that is the one still to
		// be declared. With a multi-hop chain this converges one hop per click,
		// and the diagnostics above show how many remain.
		if candidate := address.Raw; candidate != "" {
			response["suggested_trusted_proxies"] = []string{candidate + "/32"}
		} else if address.Peer != "" {
			response["suggested_trusted_proxies"] = []string{address.Peer + "/32"}
		}
	}
	if recorder := authevents.Default(); recorder != nil {
		response["dropped_events"] = recorder.Dropped()
	}
	response["protected"] = registry.ProtectedEntries()
	if address.IP != nil {
		response["self_allowed"] = matcher.AllowsAddress(address.IP)
		response["suggested_self_rule"] = ipaccess.BanCIDR(address.IP)
	}
	c.JSON(http.StatusOK, response)
}
