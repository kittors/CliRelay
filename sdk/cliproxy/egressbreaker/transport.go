package egressbreaker

import (
	"net/http"
	"net/http/httptrace"
)

// Transport returns base behind the breaker: a request through proxyURL is
// refused with an *Error while the breaker is open for it, and otherwise
// reports how its connection went. base must be a transport that sends every
// request through proxyURL.
func (b *Breaker) Transport(proxyURL string, base http.RoundTripper) http.RoundTripper {
	if b == nil || base == nil {
		return base
	}
	return &transport{breaker: b, proxyURL: proxyURL, base: base}
}

type transport struct {
	breaker  *Breaker
	proxyURL string
	base     http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	permit, err := t.breaker.Allow(t.proxyURL)
	if err != nil {
		return nil, err
	}
	if permit != nil {
		// Both hooks fire only once the request holds a connection through
		// the proxy. net/http reports GotConn for new and reused connections
		// alike; the TLS fingerprint transport drives HTTP/2 itself and only
		// produces WroteHeaders. Either one settles a probe without waiting
		// for the upstream to answer.
		trace := &httptrace.ClientTrace{
			GotConn:      func(httptrace.GotConnInfo) { permit.Connected() },
			WroteHeaders: permit.Connected,
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}
	resp, err := t.base.RoundTrip(req)
	return resp, permit.Done(err)
}

// Unwrap returns the transport the breaker stands in front of.
func (t *transport) Unwrap() http.RoundTripper { return t.base }

// CloseIdleConnections forwards to base, as http.Client.CloseIdleConnections
// expects of a transport.
func (t *transport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
