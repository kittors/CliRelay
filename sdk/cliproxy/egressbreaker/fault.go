// Package egressbreaker stops requests from going out through a proxy that is
// known not to work, and lets them through again once it does.
//
// On 2026-11-11 the residential SOCKS5 proxies most accounts leave through
// rejected their credentials for 27 minutes. Nothing a retry does changes that
// answer, yet every request kept trying: a failure to reach or get past the
// proxy carries no HTTP status, so the scheduler never cooled anything down,
// and each request walked the whole account pool, translating its body and
// performing a proxy handshake for every account, before failing with a bare
// 500. Clients retried within seconds and doubled the load.
//
// The breaker keeps one state per proxy URL, credentials included: two pool
// entries on the same host and port with different credentials fail and
// recover separately, as they do at the provider. It learns from the requests
// themselves instead of probing on a timer, because only a real handshake
// tells whether the credentials are accepted (the TCP check in package
// egresshealth saw every one of those proxies as reachable) and because a
// probe of its own would spend the proxy subscription.
//
// Only two findings count, both of them about the proxy hop and neither about
// the destination: the proxy rejecting the credentials, and no connection to
// the proxy at all. Either opens the breaker once it has happened a few times
// in a row. A proxy that completes the handshake and then reports that
// it cannot reach the destination proves nothing about itself: counting that
// would let one dead upstream take the proxy away from every other upstream
// behind it.
package egressbreaker

import (
	"context"
	"errors"
	"net"
	"strings"
)

// Fault is what is known to be wrong with a proxy hop.
type Fault int

const (
	// FaultNone means the error, if any, says nothing against the proxy.
	FaultNone Fault = iota
	// FaultAuthRejected means the proxy refused the credentials it was given.
	// The same credentials get the same answer until someone changes them or
	// the provider restores the account.
	FaultAuthRejected
	// FaultUnreachable means no connection to the proxy could be opened. A
	// single lost SYN looks the same, so it takes more of them to count.
	FaultUnreachable
)

// String is the wording used in errors and logs.
func (f Fault) String() string {
	switch f {
	case FaultAuthRejected:
		return "proxy authentication rejected"
	case FaultUnreachable:
		return "cannot connect to the proxy"
	default:
		return "no fault"
	}
}

// HopError marks an error as a failure of the proxy hop itself. The dialers
// that tunnel through a proxy attach it where the failure happens, the only
// place that knows which stage failed. It reads as the error it wraps, so
// callers that show or match the original text see no difference.
type HopError struct {
	Fault Fault
	Err   error
}

func (e *HopError) Error() string { return e.Err.Error() }

func (e *HopError) Unwrap() error { return e.Err }

// FaultOf reports the proxy fault err was marked with, FaultNone when it has
// none.
func FaultOf(err error) Fault {
	var hop *HopError
	if errors.As(err, &hop) && hop != nil {
		return hop.Fault
	}
	return FaultNone
}

// AuthRejected marks err as the proxy refusing the credentials.
func AuthRejected(err error) error {
	if err == nil {
		return nil
	}
	return &HopError{Fault: FaultAuthRejected, Err: err}
}

// DialFunc matches net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// MarkDial wraps the dial that connects to the proxy. A failure of it means
// the proxy cannot be reached, unless the caller gave up first: a cancelled
// dial says nothing about the address it was dialling.
func MarkDial(dial DialFunc) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil && ctx.Err() == nil && FaultOf(err) == FaultNone {
			err = &HopError{Fault: FaultUnreachable, Err: err}
		}
		return conn, err
	}
}

// HopDialer is MarkDial in the shape golang.org/x/net/proxy takes for the
// dialer that reaches the proxy (its Dialer and ContextDialer interfaces).
type HopDialer struct {
	Base DialFunc
}

// Dial implements proxy.Dialer.
func (d HopDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

// DialContext implements proxy.ContextDialer.
func (d HopDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return MarkDial(d.Base)(ctx, network, address)
}

// socks5AuthRejections are the errors golang.org/x/net/internal/socks returns
// when the proxy turns the credentials down: a failed username/password
// sub-negotiation, and a method selection that accepts none of the methods
// offered (which is how a proxy that wants credentials answers a client whose
// credentials it does not take). The library exports no sentinel for either,
// so they are matched by text; TestSOCKS5AuthRejectionIsMarked runs the real
// library against a rejecting server and fails if the wording ever changes.
var socks5AuthRejections = []string{
	"username/password authentication failed",
	"no acceptable authentication methods",
}

// MarkSOCKS5 marks the error of a SOCKS5 dial through golang.org/x/net/proxy
// when it says the proxy rejected the credentials. Every other handshake
// failure is returned as it is: a reply such as "host unreachable" is the
// proxy reporting on the destination, and a connection that drops in the
// middle of the handshake could be either side.
func MarkSOCKS5(err error) error {
	if err == nil || FaultOf(err) != FaultNone {
		return err
	}
	var op *net.OpError
	if !errors.As(err, &op) || op == nil || op.Err == nil || !strings.HasPrefix(op.Op, "socks") {
		return err
	}
	reason := op.Err.Error()
	for _, rejection := range socks5AuthRejections {
		if reason == rejection {
			return &HopError{Fault: FaultAuthRejected, Err: err}
		}
	}
	return err
}
