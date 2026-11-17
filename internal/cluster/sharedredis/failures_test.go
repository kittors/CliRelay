package sharedredis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// replyError is an error reply from the server, as go-redis reports one.
type replyError string

func (e replyError) Error() string { return string(e) }
func (replyError) RedisError()     {}

// errLostReply is what a command whose reply was lost on the link returns:
// the socket deadline set from the op timeout fires.
var errLostReply = &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}

func TestClassifyError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	cases := []struct {
		name string
		err  error
		want failureKind
	}{
		{"nil", nil, failureNone},
		{"missing key", redis.Nil, failureNone},
		{"caller cancelled", context.Canceled, failureNone},
		{"client closed", redis.ErrClosed, failureNone},
		{"script bug", replyError("ERR Error running script"), failureCommand},
		{"wrong type", replyError("WRONGTYPE Operation against a key holding the wrong kind of value"), failureCommand},
		{"loading", replyError("LOADING Redis is loading the dataset in memory"), failureFatal},
		{"no auth", replyError("NOAUTH Authentication required."), failureFatal},
		{"wrong password", replyError("WRONGPASS invalid username-password pair"), failureFatal},
		{"wrapped reply", fmt.Errorf("health probe: %w", replyError("READONLY You can't write against a read only replica.")), failureFatal},
		{"refused dial", &dialError{err: refused}, failureFatal},
		{"failed handshake", &dialError{err: errors.New("tls: failed to verify certificate")}, failureFatal},
		{"dial cut short by the op timeout", &dialError{err: context.DeadlineExceeded}, failureTransient},
		{"dial timed out", &dialError{err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}}, failureTransient},
		{"lost reply", errLostReply, failureTransient},
		{"wrapped lost reply", fmt.Errorf("health probe: %w", errLostReply), failureTransient},
		{"op timeout", context.DeadlineExceeded, failureTransient},
		{"connection closed", io.EOF, failureTransient},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, failureTransient},
		{"pool timeout", redis.ErrPoolTimeout, failureTransient},
		// A bare dial error that did not come through contextDialer is not
		// known to be one, so it gets the benefit of the doubt.
		{"unwrapped refused dial", refused, failureTransient},
	}
	for _, tc := range cases {
		if got := classifyError(tc.err); got != tc.want {
			t.Errorf("%s: classifyError(%v) = %d, want %d", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestContextDialerMarksDialErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	_, err = contextDialer(time.Second, nil)(context.Background(), "tcp", addr)
	var dialErr *dialError
	if !errors.As(err, &dialErr) {
		t.Fatalf("a failed dial must come back as a dialError, got %T %v", err, err)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dialError must keep the cause reachable, got %v", err)
	}
	if classifyError(err) != failureFatal {
		t.Fatalf("a refused dial must be fatal, got %d", classifyError(err))
	}
}

func trackerOptions() Options {
	return Options{
		FailureWindow:    10 * time.Second,
		FailureThreshold: 3,
		FailureRatio:     0.5,
		OutageAfter:      3 * time.Second,
	}.withDefaults()
}

// TestFailureTrackerToleratesALossyLink plays a link that loses one command
// in five, then two in five, at a steady rate for a minute: every lost command
// falls back on its own and the server is never marked down.
func TestFailureTrackerToleratesALossyLink(t *testing.T) {
	for _, lossEvery := range []int{5, 3} {
		o := trackerOptions()
		var tr failureTracker
		now := time.Unix(1_000, 0)
		for i := 0; i < 600; i++ {
			now = now.Add(100 * time.Millisecond)
			if i%lossEvery != lossEvery-1 {
				tr.success(now, o)
				continue
			}
			if outage, reason := tr.failure(now, o); outage {
				t.Fatalf("losing one command in %d marked the server down at command %d: %s", lossEvery, i, reason)
			}
		}
	}
}

func TestFailureTrackerQuietNodeNeedsThreshold(t *testing.T) {
	o := trackerOptions()
	var tr failureTracker
	now := time.Unix(1_000, 0)
	for i := 1; i < o.FailureThreshold; i++ {
		now = now.Add(100 * time.Millisecond)
		if outage, reason := tr.failure(now, o); outage {
			t.Fatalf("%d failures, below the threshold, marked the server down: %s", i, reason)
		}
	}
	now = now.Add(100 * time.Millisecond)
	outage, reason := tr.failure(now, o)
	if !outage {
		t.Fatal("the threshold reached with nothing succeeding must mark the server down")
	}
	if reason == "" {
		t.Fatal("an outage must come with a reason for the log")
	}
}

func TestFailureTrackerRatioMarksDown(t *testing.T) {
	o := trackerOptions()
	o.OutageAfter = time.Hour // leave the decision to the ratio
	var tr failureTracker
	now := time.Unix(1_000, 0)
	for i := 0; i < 4; i++ {
		now = now.Add(100 * time.Millisecond)
		tr.success(now, o)
	}
	for i := 1; i <= 4; i++ {
		now = now.Add(100 * time.Millisecond)
		outage, reason := tr.failure(now, o)
		// 3 failures of 7 outcomes are under half; 4 of 8 are half.
		if want := i == 4; outage != want {
			t.Fatalf("failure %d after 4 successes: outage = %v (%s), want %v", i, outage, reason, want)
		}
	}
}

func TestFailureTrackerForgetsOldFailures(t *testing.T) {
	o := trackerOptions()
	var tr failureTracker
	now := time.Unix(1_000, 0)
	tr.failure(now, o)
	tr.failure(now.Add(100*time.Millisecond), o)
	tr.success(now.Add(200*time.Millisecond), o)
	// Well past the window: the two old failures no longer count.
	now = now.Add(o.FailureWindow + 2*time.Second)
	if outage, reason := tr.failure(now, o); outage {
		t.Fatalf("failures that left the window still counted: %s", reason)
	}
	if ok, failed := tr.totals(now, o); ok != 0 || failed != 1 {
		t.Fatalf("window totals = %d ok, %d failed; want only the new failure", ok, failed)
	}
}

// TestFailureTrackerStreakCatchesADarkLink is the busy node whose link goes
// dark: the window still holds many successes, so the ratio would take
// seconds to tip, but nothing succeeds any more and the streak trips once it
// has lasted OutageAfter.
func TestFailureTrackerStreakCatchesADarkLink(t *testing.T) {
	o := trackerOptions()
	var tr failureTracker
	now := time.Unix(1_000, 0)
	for i := 0; i < 200; i++ {
		now = now.Add(10 * time.Millisecond)
		tr.success(now, o)
	}
	start := now
	for {
		now = now.Add(50 * time.Millisecond)
		outage, reason := tr.failure(now, o)
		elapsed := now.Sub(start)
		if outage {
			if elapsed < o.OutageAfter {
				t.Fatalf("streak tripped after %s, before OutageAfter %s: %s", elapsed, o.OutageAfter, reason)
			}
			if elapsed > o.OutageAfter+100*time.Millisecond {
				t.Fatalf("streak tripped only after %s", elapsed)
			}
			return
		}
		if elapsed > 2*o.OutageAfter {
			t.Fatal("a link with nothing succeeding was never marked down")
		}
	}
}

// TestFailureTrackerSuccessEndsTheStreak plays bursts of loss, each a run of
// failures just short of OutageAfter, ended by commands getting through: the
// streak restarts every time and never trips, nor does the ratio.
func TestFailureTrackerSuccessEndsTheStreak(t *testing.T) {
	o := trackerOptions()
	var tr failureTracker
	now := time.Unix(1_000, 0)
	for i := 0; i < 20; i++ {
		tr.success(now, o)
	}
	step := o.OutageAfter/5 - time.Millisecond
	for round := 0; round < 10; round++ {
		for i := 0; i < 5; i++ {
			now = now.Add(step)
			if outage, reason := tr.failure(now, o); outage {
				t.Fatalf("round %d, failure %d: %s", round, i, reason)
			}
		}
		for i := 0; i < 10; i++ {
			tr.success(now, o)
		}
	}
}

func TestOutcomeWindowSlides(t *testing.T) {
	const span = 10 * time.Second
	var w outcomeWindow
	now := time.Unix(1_000, 0)
	for i := 0; i < 20; i++ {
		w.add(now.Add(time.Duration(i)*time.Second), span, i%2 == 0)
	}
	// The last ten seconds hold seconds 10 to 19: five of each.
	ok, failed := w.totals(now.Add(19*time.Second), span)
	if ok != 5 || failed != 5 {
		t.Fatalf("totals = %d ok, %d failed; want 5 and 5", ok, failed)
	}
	if ok, failed := w.totals(now.Add(time.Hour), span); ok != 0 || failed != 0 {
		t.Fatalf("a window long after the last outcome must be empty, got %d ok, %d failed", ok, failed)
	}
}
