package retryhint

import "net/http"

// proxyErrorHeaders are response headers a proxy product sets on the error
// pages it generates itself. They tell a failure of the proxy in front of the
// upstream from a failure of the upstream.
var proxyErrorHeaders = []string{
	"X-Squid-Error",    // Squid
	"X-Luminati-Error", // Bright Data (formerly Luminati)
	"X-Brd-Error",      // Bright Data
}

// FromProxy reports whether a failed response was produced by the outbound
// proxy rather than by the upstream. The credential did nothing wrong, and
// parking it would take a healthy account out of rotation because of a
// flapping proxy.
//
// It only recognises responses that say so themselves. A bare 502 could come
// from either side, and treating those as the proxy's would stop cooling down
// genuinely failing upstreams. Failures to reach the proxy at all (dial,
// CONNECT, SOCKS handshake) carry no HTTP status, so they never started a
// cooldown.
func FromProxy(h http.Header) bool {
	for _, name := range proxyErrorHeaders {
		if h.Get(name) != "" {
			return true
		}
	}
	return false
}

// FailureHeaders keeps the headers of a failed upstream response that the
// scheduler reads, and nothing else: the retry hints (see HintHeaders) and the
// markers a proxy puts on its own error pages (see FromProxy).
//
// An executor attaches them to its error so that every provider gets the same
// reading without parsing the response itself. The error may also be forwarded
// to the client when passthrough-headers is on, which is why cookies and the
// upstream's framing headers are not kept.
func FailureHeaders(src http.Header) http.Header {
	dst := HintHeaders(src)
	for _, name := range proxyErrorHeaders {
		values := src.Values(name)
		if len(values) == 0 {
			continue
		}
		if dst == nil {
			dst = make(http.Header)
		}
		dst[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	return dst
}
