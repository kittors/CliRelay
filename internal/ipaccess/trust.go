package ipaccess

import (
	"net"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// ClientAddress is everything the admission and throttle layers need to know
// about where a request came from.
type ClientAddress struct {
	// Peer is the direct TCP peer, before any forwarding header is considered.
	// Kept for diagnostics: it is the hop an operator has to declare next when
	// the chain cannot be resolved.
	Peer string
	// DirectPeer reports that the request carried no forwarding header at all, so
	// Peer is the client itself rather than infrastructure in front of it.
	DirectPeer bool
	// Chain is the forwarding chain as received, left to right, for diagnostics.
	Chain []string
	// IP is the address the framework resolved, honouring trusted-proxies.
	IP net.IP
	// Raw is the textual form, kept for display and for the rare case where the
	// address could not be parsed at all.
	Raw string
	// Trusted reports whether IP actually identifies the client. It is false
	// when the request was relayed but no trusted proxy was configured, in which
	// case IP is the proxy's address and stands for every client behind it.
	Trusted bool
	// RelayHeader names the forwarding header that was present, for diagnostics.
	RelayHeader string
}

// ProxyTrustConfigured reports whether the operator declared any trusted proxy,
// which is what decides whether forwarding headers may be believed.
func ProxyTrustConfigured(trustedProxies []string) bool {
	for _, proxy := range trustedProxies {
		if strings.TrimSpace(proxy) != "" {
			return true
		}
	}
	return false
}

// Loopback reports whether the resolved address is a loopback address.
func (a ClientAddress) Loopback() bool {
	return a.IP != nil && a.IP.IsLoopback()
}

// LocalOperator reports a request that genuinely originated on this host: a
// loopback peer that arrived with no forwarding header at all.
//
// This is the distinction that matters, and conflating it with Loopback() was a
// real defect. A relayed request whose chain resolves to a loopback address is
// not a local operator — it is a chain that could not be resolved, and treating
// it as the operator channel exempts the entire internet from rate limiting and
// from the rule list. Only a direct connection earns that exemption.
func (a ClientAddress) LocalOperator() bool {
	return a.DirectPeer && a.Loopback()
}

// Resolve derives the client address from a request.
//
// resolvedIP is what the web framework reports after applying trusted-proxies
// (gin's ClientIP), and proxyTrustConfigured is whether the operator declared
// any trusted proxy at all.
//
// The forwarding headers are deliberately never read as the address itself:
// they are attacker-controlled, so trusting them would let anyone both evade a
// ban and forge membership of the allow list.
func Resolve(req *http.Request, resolvedIP string, proxyTrustConfigured bool) ClientAddress {
	addr := ClientAddress{Trusted: true}
	if req != nil {
		addr.RelayHeader = util.RelayIndicationHeader(req)
	}

	candidate := strings.TrimSpace(resolvedIP)
	if candidate == "" && req != nil {
		candidate = util.RemoteAddrIP(req.RemoteAddr)
	}
	addr.Raw = candidate

	if normalized, ok := NormalizeObservedIP(candidate); ok {
		addr.IP = net.ParseIP(normalized)
		addr.Raw = normalized
	}
	if addr.IP == nil {
		// An address that cannot be parsed identifies nobody, so no admission
		// decision made from it can be correct.
		addr.Trusted = false
		return addr
	}
	if addr.RelayHeader != "" && !proxyTrustConfigured {
		addr.Trusted = false
	}
	return addr
}
