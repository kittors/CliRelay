package util

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// WebsocketOriginAllowed returns whether a websocket upgrade request should be
// accepted from the given request origin.
//
// Policy:
//   - Non-browser clients usually omit Origin; allow when Origin is empty.
//   - For browser clients, require a same-host origin to reduce CSRF-style
//     cross-site websocket attacks.
//
// This is intentionally host-focused (scheme differences may exist behind TLS
// termination / reverse proxies).
func WebsocketOriginAllowed(r *http.Request) bool {
	if r == nil {
		return false
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u == nil {
		return false
	}
	originHost := strings.TrimSpace(u.Host)
	if originHost == "" {
		return false
	}

	reqHost := strings.TrimSpace(requestHost(r))
	if reqHost == "" {
		return false
	}

	oh, _ := splitHostPortLoose(originHost)
	rh, _ := splitHostPortLoose(reqHost)
	return strings.EqualFold(oh, rh)
}

func requestHost(r *http.Request) string {
	return strings.TrimSpace(r.Host)
}

func splitHostPortLoose(hostport string) (host, port string) {
	hp := strings.TrimSpace(hostport)
	hp = strings.TrimPrefix(hp, "[")
	hp = strings.TrimSuffix(hp, "]")

	// Try strict split first.
	if h, p, err := net.SplitHostPort(hp); err == nil {
		return strings.TrimSpace(h), strings.TrimSpace(p)
	}
	// No port (or malformed). Treat as host only.
	return strings.TrimSpace(hp), ""
}
