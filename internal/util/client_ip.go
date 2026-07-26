package util

import (
	"net"
	"net/http"
	"net/textproto"
	"strings"
)

// ForwardedClientIP extracts the original client IP from common CDN/proxy headers.
// It is intended for audit/log display only; access control should continue to use
// the framework's trusted-proxy-aware client IP.
func ForwardedClientIP(req *http.Request) (string, string) {
	if req == nil || req.Header == nil {
		return "", ""
	}
	headerPriority := []string{
		"CF-Connecting-IP",
		"True-Client-IP",
		"X-Real-IP",
		"X-Forwarded-For",
	}
	for _, header := range headerPriority {
		if ip := firstHeaderIP(req.Header, header); ip != "" {
			return ip, header
		}
	}
	if ip := forwardedHeaderIP(req.Header); ip != "" {
		return ip, "Forwarded"
	}
	return "", ""
}

func firstHeaderIP(headers http.Header, header string) string {
	for _, value := range headers.Values(header) {
		for _, candidate := range strings.Split(value, ",") {
			if ip := normalizeIPCandidate(candidate); ip != "" {
				return ip
			}
		}
	}
	return ""
}

func forwardedHeaderIP(headers http.Header) string {
	for _, value := range headers.Values("Forwarded") {
		for _, entry := range strings.Split(value, ",") {
			for _, part := range strings.Split(entry, ";") {
				key, rawValue, ok := strings.Cut(part, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "for") {
					continue
				}
				if ip := normalizeIPCandidate(rawValue); ip != "" {
					return ip
				}
			}
		}
	}
	return ""
}

// relayIndicationHeaders are headers that reverse proxies, CDNs, load balancers and
// tunnels add when they relay a request. Any of them being present means the direct
// TCP peer is a relay rather than the original client.
//
// The list is deliberately broader than ForwardedClientIP's priority list: here we
// only need to know that *some* relay handled the request, so headers that carry no
// client IP (Via, X-Forwarded-Proto, CF-Ray) are just as conclusive as the ones that
// do, and a header we fail to recognise is a bypass rather than a display glitch.
// Stored canonicalised so a future switch from Header.Values() to direct map indexing
// cannot silently disable half the list: written literally, entries like "X-Real-IP" and
// "CF-Ray" are not in textproto canonical form ("X-Real-Ip", "Cf-Ray").
var relayIndicationHeaders = canonicalHeaderNames([]string{
	"Forwarded",
	"Via",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Port",
	"X-Forwarded-Proto",
	"X-Forwarded-Server",
	"X-Original-Forwarded-For",
	"X-Real-IP",
	"X-Client-IP",
	"X-Cluster-Client-IP",
	"CF-Connecting-IP",
	"CF-Connecting-IPv6",
	"CF-Ray",
	"True-Client-IP",
	"Fastly-Client-IP",
	"X-Envoy-External-Address",
	"X-Envoy-Internal",
	"X-Azure-ClientIP",
	"X-Azure-SocketIP",
})

func canonicalHeaderNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, textproto.CanonicalMIMEHeaderKey(name))
	}
	return out
}

// IsLocalOriginRequest reports whether req provably originates from this host, and is
// the check to use when an endpoint is gated on "local access only".
//
// A loopback RemoteAddr on its own does not prove local origin: when CliRelay sits
// behind a same-host reverse proxy, the direct TCP peer is that proxy, so every
// external request arrives from 127.0.0.1. This function therefore requires both a
// loopback peer and the absence of any relay header, and fails closed on addresses it
// cannot parse.
//
// Known limitation: any relay that forwards a request without adding a header of its own
// is indistinguishable from a local client at the HTTP layer. That covers more than L4
// tunnels (nginx `stream`, HAProxy in TCP mode, socat, iptables DNAT, `cloudflared --url
// http://127.0.0.1:...`) — notably, a bare nginx `proxy_pass` sets no forwarded headers
// unless the operator adds proxy_set_header, so an ordinary L7 reverse proxy can also
// slip through. A front-end vulnerable to request smuggling likewise lets an attacker
// author the backend-bound bytes directly, with no operator misconfiguration needed.
//
// So this narrows the exposure, it does not close it. Endpoints that must hold against a
// hostile network need their own credential rather than only this check.
func IsLocalOriginRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	peer := net.ParseIP(RemoteAddrIP(req.RemoteAddr))
	if peer == nil || !peer.IsLoopback() {
		return false
	}
	return !hasRelayIndication(req.Header)
}

// RelayIndicationHeader returns the first relay header present on the request, for
// logging why a request was not treated as local. It returns "" when none is set.
func RelayIndicationHeader(req *http.Request) string {
	if req == nil {
		return ""
	}
	return firstRelayIndicationHeader(req.Header)
}

func hasRelayIndication(headers http.Header) bool {
	return firstRelayIndicationHeader(headers) != ""
}

func firstRelayIndicationHeader(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, header := range relayIndicationHeaders {
		for _, value := range headers.Values(header) {
			if strings.TrimSpace(value) != "" {
				return header
			}
		}
	}
	return ""
}

// RemoteAddrIP extracts and normalizes the IP part of net/http Request.RemoteAddr.
func RemoteAddrIP(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return normalizeIPCandidate(host)
	}
	return normalizeIPCandidate(remoteAddr)
}

func normalizeIPCandidate(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	candidate = strings.Trim(candidate, `"`)
	if candidate == "" || strings.EqualFold(candidate, "unknown") {
		return ""
	}
	if strings.HasPrefix(candidate, "[") && strings.Contains(candidate, "]") {
		if end := strings.Index(candidate, "]"); end >= 0 {
			candidate = candidate[1:end]
		}
	} else if host, _, err := net.SplitHostPort(candidate); err == nil {
		candidate = host
	}
	ip := net.ParseIP(strings.TrimSpace(candidate))
	if ip == nil {
		return ""
	}
	return ip.String()
}
