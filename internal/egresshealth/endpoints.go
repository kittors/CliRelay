package egresshealth

import (
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// defaultPorts are the ports proxy clients assume when a proxy URL names none.
var defaultPorts = map[string]string{
	"socks5":  "1080",
	"socks5h": "1080",
	"http":    "80",
	"https":   "443",
}

// Endpoint returns the host:port a proxy URL makes this node connect to, or
// false when raw does not name a supported proxy. The result never carries the
// URL's credentials, so it is safe to log.
func Endpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	defaultPort, ok := defaultPorts[strings.ToLower(u.Scheme)]
	if !ok {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	if n, errPort := strconv.Atoi(port); errPort != nil || n < 1 || n > 65535 {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

// Endpoints turns proxy URLs into the sorted, de-duplicated endpoints to check.
// Blank entries mean "no proxy" and are dropped silently; skipped counts the
// entries that are set but name no supported proxy.
func Endpoints(urls []string) (endpoints []string, skipped int) {
	seen := make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		endpoint, ok := Endpoint(raw)
		if !ok {
			skipped++
			continue
		}
		if _, dup := seen[endpoint]; dup {
			continue
		}
		seen[endpoint] = struct{}{}
		endpoints = append(endpoints, endpoint)
	}
	slices.Sort(endpoints)
	return endpoints, skipped
}
