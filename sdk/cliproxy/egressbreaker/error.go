package egressbreaker

import (
	"context"
	"errors"
	"math"
	"net"
	"syscall"
	"time"
)

// Error is what a request gets when the proxy it leaves through does not
// work: either the breaker refused it before anything was sent (Refused), or
// it is the request whose own connection attempt just failed.
//
// It has no StatusCode method on purpose. The scheduler reads a status as the
// upstream's verdict on the account (a 503 cools the model down for a minute),
// and this is not about the account. The API handlers recognise the type and
// answer 503 themselves.
//
// The text never names the proxy: it reaches API clients, and proxy hosts are
// operator information. The breaker's own log lines name it.
type Error struct {
	// Fault is what is wrong with the proxy.
	Fault Fault
	// Refused says the request was not sent.
	Refused bool
	// RetryAfter is how long until the breaker lets a request through to try
	// the proxy again; zero when the breaker is not open.
	RetryAfter time.Duration

	cause error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Refused {
		text := "egress proxy unavailable (" + e.Fault.String() + "); request not sent"
		if seconds := e.RetryAfterSeconds(); seconds > 0 {
			text += ", retry in " + (time.Duration(seconds) * time.Second).String()
		}
		return text
	}
	text := "egress proxy unavailable: " + e.Fault.String()
	if detail := describeDialFailure(e.cause); e.Fault == FaultUnreachable && detail != "" {
		text += " (" + detail + ")"
	}
	return text
}

// Unwrap returns the failure of the connection attempt, nil for a refused
// request.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// RetryAfterSeconds is RetryAfter in whole seconds, rounded up so that a
// client honouring it does not come back just before the probe is due.
func (e *Error) RetryAfterSeconds() int {
	if e == nil || e.RetryAfter <= 0 {
		return 0
	}
	return int(math.Ceil(e.RetryAfter.Seconds()))
}

// AsError finds the breaker's error in err's chain. An http.Client wraps what
// its transport returns in a *url.Error, so callers cannot assert the type
// directly.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) && target != nil {
		return target, true
	}
	return nil, false
}

// describeDialFailure names why a connect failed without repeating the
// address, which the net errors print in full.
func describeDialFailure(err error) string {
	if err == nil {
		return ""
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "name resolution failed"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return ""
}
