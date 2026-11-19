package sharedredis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// failureKind sorts a command error by what it says about the server.
type failureKind uint8

const (
	// failureNone is no failure of the server: a missing key, the caller
	// giving up, or the client being closed.
	failureNone failureKind = iota
	// failureCommand is the server rejecting this one command (a script bug,
	// a wrong type). It fails that command only.
	failureCommand
	// failureTransient lost this one command to a timeout or to a connection
	// that broke under it. On a lossy link a single dropped packet costs a
	// whole retransmission timeout, at least 200ms on Linux, which is already
	// the request-path budget: one of these says little about the server, so
	// it only counts towards the failure window.
	failureTransient
	// failureFatal means nobody can use the server right now: a refused or
	// unroutable dial, a failed TLS handshake, or a reply such as LOADING or
	// NOAUTH. It marks the client down at once.
	failureFatal
)

// unusableServerPrefixes are error replies that mean the server as a whole
// cannot serve us right now, as opposed to rejecting one command.
var unusableServerPrefixes = []string{
	"LOADING", "READONLY", "MASTERDOWN", "BUSY", "NOAUTH", "WRONGPASS",
	"NOPERM", "CLUSTERDOWN", "TRYAGAIN", "MISCONF",
}

// dialError marks an error from opening a connection (the TCP connect or the
// TLS handshake), as opposed to one from a command on an open connection.
// contextDialer wraps every error it returns in one; go-redis hands dial
// errors back to the command unchanged, including the cached one it returns
// while its pool keeps failing to dial.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// classifyError sorts err; see failureKind.
func classifyError(err error) failureKind {
	if err == nil || errors.Is(err, redis.Nil) || errors.Is(err, context.Canceled) || errors.Is(err, redis.ErrClosed) {
		return failureNone
	}
	var replyErr redis.Error
	if errors.As(err, &replyErr) {
		msg := strings.TrimSpace(replyErr.Error())
		for _, prefix := range unusableServerPrefixes {
			if strings.HasPrefix(msg, prefix) {
				return failureFatal
			}
		}
		return failureCommand
	}
	// A dial that timed out is a lost packet like any other: the first SYN
	// lost costs a full second before it is resent. A dial the network or the
	// server answered with a refusal will not do better on the next command.
	var dialErr *dialError
	if errors.As(err, &dialErr) && !isTimeout(err) {
		return failureFatal
	}
	return failureTransient
}

// IsConnectivityError reports whether err means the server could not be used
// for the command, rather than that it rejected the command. The caller's own
// cancellation is not the server's fault and does not count.
func IsConnectivityError(err error) bool {
	return classifyError(err) >= failureTransient
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// windowBuckets is how many slices the failure window is counted in. A slice
// drops out of the window whole, so the window is accurate to one slice.
const windowBuckets = 10

type windowBucket struct {
	start  time.Time
	ok     int
	failed int
}

// outcomeWindow counts command outcomes over a sliding span, in
// windowBuckets slices. Its zero value is empty and ready to use.
type outcomeWindow struct {
	buckets [windowBuckets]windowBucket
	cur     int
}

func (w *outcomeWindow) add(now time.Time, span time.Duration, failed bool) {
	b := &w.buckets[w.cur]
	// Slices start when they are first written to, so a quiet spell costs
	// nothing; slices older than the span are skipped by totals.
	if b.start.IsZero() || now.Sub(b.start) >= span/windowBuckets {
		w.cur = (w.cur + 1) % windowBuckets
		b = &w.buckets[w.cur]
		*b = windowBucket{start: now}
	}
	if failed {
		b.failed++
	} else {
		b.ok++
	}
}

func (w *outcomeWindow) totals(now time.Time, span time.Duration) (ok, failed int) {
	for i := range w.buckets {
		b := &w.buckets[i]
		if b.start.IsZero() || now.Sub(b.start) >= span {
			continue
		}
		ok += b.ok
		failed += b.failed
	}
	return ok, failed
}

// failureTracker decides when transient failures add up to an outage. Only
// outcomes held to the request-path budget count as successes: request-path
// commands and health probes. It is not safe for concurrent use; the Client
// guards it with its mutex. Its zero value is ready to use.
type failureTracker struct {
	window outcomeWindow
	// streak counts transient failures since the last success, and
	// streakStart is when the first of them was seen.
	streak      int
	streakStart time.Time
}

func (t *failureTracker) success(now time.Time, o Options) {
	t.window.add(now, o.FailureWindow, false)
	t.streak = 0
	t.streakStart = time.Time{}
}

// failure records one transient failure and reports whether the failures now
// add up to an outage, with the reason for the log.
//
// Two tests, either of which marks the server down:
//   - the window: at least FailureThreshold failures in the last
//     FailureWindow, making up at least FailureRatio of its outcomes. A lossy
//     link loses a fraction of the commands however busy the node is, so the
//     ratio, not the count, tells loss from an outage;
//   - the streak: FailureThreshold failures in a row spanning OutageAfter with
//     nothing succeeding in between. On a busy node the window's earlier
//     successes would hold off a link that has gone dark for seconds, during
//     which every command waits out the op timeout; the streak catches that.
func (t *failureTracker) failure(now time.Time, o Options) (outage bool, reason string) {
	t.window.add(now, o.FailureWindow, true)
	if t.streak == 0 {
		t.streakStart = now
	}
	t.streak++
	ok, failed := t.window.totals(now, o.FailureWindow)
	if failed >= o.FailureThreshold && float64(failed) >= o.FailureRatio*float64(ok+failed) {
		return true, fmt.Sprintf("%d of the last %d commands failed within %s", failed, ok+failed, o.FailureWindow)
	}
	if span := now.Sub(t.streakStart); t.streak >= o.FailureThreshold && span >= o.OutageAfter {
		return true, fmt.Sprintf("%d commands in a row failed over %s with none succeeding", t.streak, span.Round(time.Millisecond))
	}
	return false, ""
}

func (t *failureTracker) totals(now time.Time, o Options) (ok, failed int) {
	return t.window.totals(now, o.FailureWindow)
}

func (t *failureTracker) reset() { *t = failureTracker{} }
