package egressbreaker

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func okResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}
}

func newRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

// roundTripError sends one request and returns only how it failed.
func roundTripError(t *testing.T, rt http.RoundTripper) error {
	t.Helper()
	resp, err := rt.RoundTrip(newRequest(t))
	if resp != nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestTransportDoesNotSendWhileOpen(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	var sent atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent.Add(1)
		return nil, errRejected()
	})
	rt := b.Transport(proxyA, base)

	var err error
	for range DefaultRejectionThreshold {
		err = roundTripError(t, rt)
		if failure, ok := AsError(err); !ok || failure.Refused {
			t.Fatalf("request failed with %#v before the breaker opened, want the proxy's own failure as an *Error", err)
		}
	}
	for range 50 {
		err = roundTripError(t, rt)
		if refusal, isRefusal := AsError(err); !isRefusal || !refusal.Refused {
			t.Fatalf("request through an open breaker failed with %#v, want a refusal", err)
		}
	}
	if got := sent.Load(); got != DefaultRejectionThreshold {
		t.Fatalf("%d requests reached the transport, want only the %d that found the fault", got, DefaultRejectionThreshold)
	}

	// Another proxy entry on the same wrapper type is not affected.
	other := b.Transport(proxyB, roundTripFunc(func(*http.Request) (*http.Response, error) { return okResponse(), nil }))
	resp, err := other.RoundTrip(newRequest(t))
	if err != nil {
		t.Fatalf("request through the working entry failed: %v", err)
	}
	_ = resp.Body.Close()
}

func TestTransportProbeSettlesWhenTheConnectionIsUp(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)
	clock.Advance(DefaultBaseOpen)

	// The upstream is slow to answer. The breaker must not keep everyone out
	// until it does: the proxy proved itself when the connection came up.
	duringProbe := make(chan error, 1)
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		if trace == nil || trace.GotConn == nil || trace.WroteHeaders == nil {
			return nil, errors.New("the breaker installed no client trace")
		}
		if err := b.Check(proxyA); err == nil {
			return nil, errors.New("the breaker was not held by the probe before it connected")
		}
		trace.GotConn(httptrace.GotConnInfo{})
		duringProbe <- b.Check(proxyA)
		trace.WroteHeaders()
		return okResponse(), nil
	})

	resp, err := b.Transport(proxyA, base).RoundTrip(newRequest(t))
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	_ = resp.Body.Close()
	if errDuring := <-duringProbe; errDuring != nil {
		t.Fatalf("requests were still refused after the probe connected: %v", errDuring)
	}
}

func TestTransportPassesOtherErrorsThrough(t *testing.T) {
	b := New(Config{Now: newFakeClock().Now})
	errUpstream := errors.New("http2: server sent GOAWAY and closed the connection")
	rt := b.Transport(proxyA, roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errUpstream }))
	for range 10 {
		if err := roundTripError(t, rt); err != errUpstream {
			t.Fatalf("RoundTrip error = %v, want the transport's error unchanged", err)
		}
	}
	if open := b.Open(); len(open) != 0 {
		t.Fatalf("errors that are not proxy faults opened the breaker: %+v", open)
	}
}

func TestTransportWithoutProxyOrBreakerIsTheBaseTransport(t *testing.T) {
	base := roundTripFunc(func(*http.Request) (*http.Response, error) { return okResponse(), nil })
	var none *Breaker
	if got := none.Transport(proxyA, base); got == nil {
		t.Fatal("a nil breaker returned no transport")
	}
	b := New(Config{})
	if got := b.Transport(proxyA, nil); got != nil {
		t.Fatalf("Transport(nil base) = %v, want nil", got)
	}
	wrapped, ok := b.Transport(proxyA, base).(interface{ Unwrap() http.RoundTripper })
	if !ok || wrapped.Unwrap() == nil {
		t.Fatal("the wrapper does not expose the transport behind it")
	}
}
