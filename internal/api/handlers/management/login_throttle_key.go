package management

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

type throttleKey struct {
	Scope throttleScope
	Value string
	// Shared marks a key that cannot be trusted to identify a single client, so
	// the penalty must stay soft: hard-banning it takes down every user behind
	// the same proxy or NAT.
	Shared bool
}

// unknownThrottleValue is the catch-all bucket for peers whose address cannot be
// parsed. It is always shared so it can never produce a hard ban.
const unknownThrottleValue = "unknown"

// clientThrottleKey derives the bucket key from the framework's trusted-proxy
// aware client IP, then normalises it to a prefix so an attacker holding an
// IPv6 /64 (free with most VPS) cannot mint 2^64 distinct buckets and OOM the
// process. Forwarding headers are NEVER used as the key directly: they are
// attacker-controlled, so an attacker could both evade their own ban and fill
// an arbitrary victim's bucket.
func (h *Handler) clientThrottleKey(c *gin.Context, scope throttleScope) throttleKey {
	peer := resolveThrottlePeer(c)
	value, shared := normalizeThrottleIP(peer)

	// A relay header with no trusted-proxies means gin is reporting the proxy's
	// address for every client, so this one bucket stands for all of them.
	if c != nil && c.Request != nil {
		if relayHeader := util.RelayIndicationHeader(c.Request); relayHeader != "" && !h.proxyTrustConfigured() {
			shared = true
			warnUntrustedProxyOnce(peer, relayHeader)
		}
	}
	return throttleKey{Scope: scope, Value: value, Shared: shared}
}

// normalizeThrottleIP collapses IPv4 to /32 and IPv6 to /64.
// Returns ("unknown", true) when no address can be parsed at all — that bucket
// is always treated as shared so it can never produce a hard ban.
func normalizeThrottleIP(raw string) (value string, shared bool) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return unknownThrottleValue, true
	}
	// Accept "host:port", "[v6]:port" and bare addresses alike; a port in the key
	// would make every new connection a new bucket.
	if host, _, err := net.SplitHostPort(candidate); err == nil {
		candidate = host
	}
	candidate = strings.TrimPrefix(candidate, "[")
	candidate = strings.TrimSuffix(candidate, "]")
	if zone := strings.IndexByte(candidate, '%'); zone >= 0 {
		candidate = candidate[:zone]
	}
	ip := net.ParseIP(strings.TrimSpace(candidate))
	if ip == nil {
		return unknownThrottleValue, true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String() + "/32", false
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64", false
}

// resolveThrottlePeer mirrors internal/api/public_lookup_middleware.go:
// ClientIP() -> RemoteAddr -> "unknown". Never returns "".
//
// Falling back rather than skipping the throttle is deliberate: a fail-open
// branch here would be an attacker-operable switch for turning rate limiting off.
func resolveThrottlePeer(c *gin.Context) string {
	if c == nil {
		return unknownThrottleValue
	}
	if ip := strings.TrimSpace(c.ClientIP()); ip != "" {
		return ip
	}
	if c.Request != nil {
		if remote := strings.TrimSpace(c.Request.RemoteAddr); remote != "" {
			return remote
		}
	}
	return unknownThrottleValue
}

// proxyTrustConfigured reports whether the operator declared any trusted proxy.
func (h *Handler) proxyTrustConfigured() bool {
	if h == nil || h.cfg == nil {
		return false
	}
	for _, proxy := range h.cfg.TrustedProxies {
		if strings.TrimSpace(proxy) != "" {
			return true
		}
	}
	return false
}

// untrustedProxyWarn deduplicates the misconfiguration warning; without it every
// single request behind the proxy would emit a line.
var untrustedProxyWarn struct {
	mu   sync.Mutex
	last time.Time
}

// warnUntrustedProxyOnce logs at most once per 10 minutes per process.
func warnUntrustedProxyOnce(peer, relayHeader string) {
	now := time.Now()
	untrustedProxyWarn.mu.Lock()
	if !untrustedProxyWarn.last.IsZero() && now.Sub(untrustedProxyWarn.last) < warnDedupInterval {
		untrustedProxyWarn.mu.Unlock()
		return
	}
	untrustedProxyWarn.last = now
	untrustedProxyWarn.mu.Unlock()

	// The remediation string is spelled out in full so an operator can paste it
	// into config.yaml without first working out that the proxy, not the client,
	// is what has to be trusted.
	hint := util.RemoteAddrIP(peer)
	if hint == "" {
		hint = peer
	}
	log.Warnf("auth-throttle: request relayed (%s present) but trusted-proxies is empty, so every client behind %s shares one throttle bucket and hard bans are downgraded to soft limits. Fix: trusted-proxies: [\"%s/32\"]",
		relayHeader, peer, hint)
}

func accountThrottleKey(scope throttleScope, normalizedUsername string) throttleKey {
	value := strings.TrimSpace(normalizedUsername)
	if value == "" {
		value = unknownThrottleValue
	}
	// Account buckets are never shared: the key names one account, so the penalty
	// can stay at full strength without collateral damage.
	return throttleKey{Scope: scope, Value: value}
}

// maskThrottleKey renders a key for logs. IPs stay readable because operators
// need them to write a firewall rule; usernames are truncated because a failed
// login is frequently a password typed into the username field.
func maskThrottleKey(k throttleKey) string {
	switch k.Scope {
	case scopeUserAccount, scopePortalAccount:
		runes := []rune(k.Value)
		prefix := runes
		if len(prefix) > 2 {
			prefix = prefix[:2]
		}
		return fmt.Sprintf("%s***(len=%d)", string(prefix), len(runes))
	default:
		return k.Value
	}
}
