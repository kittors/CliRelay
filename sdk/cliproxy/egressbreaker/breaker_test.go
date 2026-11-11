package egressbreaker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 11, 16, 16, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

const (
	proxyA = "socks5://alice:secret@203.0.113.7:443"
	// proxyB is the same endpoint with other credentials, as two tenants'
	// pool entries on one provider host are.
	proxyB = "socks5://bob:hunter2@203.0.113.7:443"
)

func errRejected() error {
	return AuthRejected(errors.New("socks connect tcp4 203.0.113.7:443->chatgpt.com:443: username/password authentication failed"))
}

func errUnreachable() error {
	return &HopError{Fault: FaultUnreachable, Err: errors.New("dial tcp4 203.0.113.7:443: i/o timeout")}
}

func mustAllow(t *testing.T, b *Breaker, proxyURL string) *Permit {
	t.Helper()
	permit, err := b.Allow(proxyURL)
	if err != nil {
		t.Fatalf("Allow refused the request: %v", err)
	}
	if permit == nil {
		t.Fatal("Allow returned no permit for a proxied request")
	}
	return permit
}

func mustRefuse(t *testing.T, b *Breaker, proxyURL string) *Error {
	t.Helper()
	permit, err := b.Allow(proxyURL)
	if err == nil {
		t.Fatalf("Allow admitted the request (probe=%v), want it refused", permit != nil && permit.probe)
	}
	refusal, ok := AsError(err)
	if !ok || !refusal.Refused {
		t.Fatalf("Allow error = %#v, want a refusal", err)
	}
	return refusal
}

// wantRefusal is mustRefuse for a caller that only needs the request refused.
func wantRefusal(t *testing.T, b *Breaker, proxyURL string) {
	t.Helper()
	_ = mustRefuse(t, b, proxyURL)
}

// openBreaker fails as many requests in a row on a rejected authentication as
// it takes to open the breaker.
func openBreaker(t *testing.T, b *Breaker, proxyURL string) {
	t.Helper()
	for range DefaultRejectionThreshold {
		if err := mustAllow(t, b, proxyURL).Done(errRejected()); err == nil {
			t.Fatal("Done returned nil for a rejected authentication")
		}
	}
}

func TestRejectedAuthenticationOpensOnTheSecondInARow(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})

	// One rejection may be the proxy gateway hiccupping. The request fails
	// with the proxy's failure, but nothing is refused yet.
	first, ok := AsError(mustAllow(t, b, proxyA).Done(errRejected()))
	if !ok || first.Refused || first.Fault != FaultAuthRejected || first.RetryAfter != 0 {
		t.Fatalf("first rejection = %+v, want the request's own failure and no wait", first)
	}
	if err := b.Check(proxyA); err != nil {
		t.Fatalf("Check refused after a single rejection: %v", err)
	}
	// A request that gets through in between ends the run.
	if err := mustAllow(t, b, proxyA).Done(nil); err != nil {
		t.Fatalf("Done(nil) = %v", err)
	}
	_ = mustAllow(t, b, proxyA).Done(errRejected())
	if err := b.Check(proxyA); err != nil {
		t.Fatalf("Check refused after two rejections with a success in between: %v", err)
	}

	// The second in a row confirms it.
	err := mustAllow(t, b, proxyA).Done(errRejected())
	failure, ok := AsError(err)
	if !ok {
		t.Fatalf("Done returned %#v, want an *Error", err)
	}
	if failure.Refused || failure.Fault != FaultAuthRejected || failure.RetryAfter != DefaultBaseOpen {
		t.Fatalf("failure = %+v, want the request's own failure with the base wait", failure)
	}
	if !errors.Is(err, failure.Unwrap()) || FaultOf(err) != FaultAuthRejected {
		t.Fatalf("the original error is no longer reachable through %v", err)
	}

	refusal := mustRefuse(t, b, proxyA)
	if refusal.Fault != FaultAuthRejected || refusal.RetryAfter != DefaultBaseOpen {
		t.Fatalf("refusal = %+v, want auth rejected with %s to wait", refusal, DefaultBaseOpen)
	}
	clock.Advance(4 * time.Second)
	if refusal = mustRefuse(t, b, proxyA); refusal.RetryAfter != DefaultBaseOpen-4*time.Second {
		t.Fatalf("RetryAfter = %s, want what is left of the wait", refusal.RetryAfter)
	}
	if errCheck := b.Check(proxyA); errCheck == nil {
		t.Fatal("Check let a request through an open breaker")
	}
}

func TestOneProbeAfterTheWaitAndBackoffDoublesToTheCap(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)

	for _, wait := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute} {
		if refusal := mustRefuse(t, b, proxyA); refusal.RetryAfter != wait {
			t.Fatalf("RetryAfter = %s, want %s", refusal.RetryAfter, wait)
		}
		clock.Advance(wait - time.Second)
		wantRefusal(t, b, proxyA)
		clock.Advance(time.Second)

		// The wait is over. Check must not use up the probe, however often
		// the scheduler asks.
		for range 3 {
			if err := b.Check(proxyA); err != nil {
				t.Fatalf("Check refused once the wait was over: %v", err)
			}
		}
		probe := mustAllow(t, b, proxyA)
		if !probe.probe {
			t.Fatal("the request admitted after the wait is not marked as the probe")
		}
		// Everything else stays out while the probe is on its way.
		for range 5 {
			if refusal := mustRefuse(t, b, proxyA); refusal.RetryAfter != probeRetryAfter {
				t.Fatalf("RetryAfter during a probe = %s, want %s", refusal.RetryAfter, probeRetryAfter)
			}
		}
		if err := b.Check(proxyA); err == nil {
			t.Fatal("Check let a request through while a probe was out")
		}
		if err := probe.Done(errRejected()); err == nil {
			t.Fatal("a failed probe returned no error")
		}
	}
}

func TestProbeThatGetsThroughClosesTheBreaker(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)
	clock.Advance(DefaultBaseOpen)

	probe := mustAllow(t, b, proxyA)
	wantRefusal(t, b, proxyA)
	// The handshake is done; the upstream has not answered yet.
	probe.Connected()

	for range 3 {
		if permit := mustAllow(t, b, proxyA); permit.probe {
			t.Fatal("a request after recovery is still treated as a probe")
		}
	}
	if open := b.Open(); len(open) != 0 {
		t.Fatalf("Open() = %+v after recovery, want none", open)
	}
	if err := probe.Done(nil); err != nil {
		t.Fatalf("Done(nil) = %v", err)
	}

	// The next outage starts from the base wait again.
	openBreaker(t, b, proxyA)
	if refusal := mustRefuse(t, b, proxyA); refusal.RetryAfter != DefaultBaseOpen {
		t.Fatalf("RetryAfter after a recovery = %s, want the base wait", refusal.RetryAfter)
	}
}

func TestUnreachableOpensOnlyAfterFailuresInARow(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})

	fail := func() *Error {
		t.Helper()
		failure, ok := AsError(mustAllow(t, b, proxyA).Done(errUnreachable()))
		if !ok || failure.Fault != FaultUnreachable {
			t.Fatalf("Done did not report an unreachable proxy: %+v", failure)
		}
		return failure
	}

	for range DefaultUnreachableThreshold - 1 {
		if failure := fail(); failure.RetryAfter != 0 {
			t.Fatalf("RetryAfter = %s below the threshold, want 0", failure.RetryAfter)
		}
	}
	// A request that gets through ends the run.
	if err := mustAllow(t, b, proxyA).Done(nil); err != nil {
		t.Fatalf("Done(nil) = %v", err)
	}
	for range DefaultUnreachableThreshold - 1 {
		_ = fail()
	}
	_ = mustAllow(t, b, proxyA).Done(nil)

	for i := range DefaultUnreachableThreshold {
		failure := fail()
		if last := i == DefaultUnreachableThreshold-1; last != (failure.RetryAfter > 0) {
			t.Fatalf("failure %d: RetryAfter = %s", i+1, failure.RetryAfter)
		}
	}
	if refusal := mustRefuse(t, b, proxyA); refusal.Fault != FaultUnreachable {
		t.Fatalf("refusal fault = %v, want unreachable", refusal.Fault)
	}
}

// The two kinds of fault are counted apart: neither run is long enough here.
func TestFaultsOfDifferentKindsDoNotAddUp(t *testing.T) {
	b := New(Config{Now: newFakeClock().Now})
	for range DefaultRejectionThreshold - 1 {
		_ = mustAllow(t, b, proxyA).Done(errRejected())
	}
	for range DefaultUnreachableThreshold - 1 {
		_ = mustAllow(t, b, proxyA).Done(errUnreachable())
	}
	if err := b.Check(proxyA); err != nil {
		t.Fatalf("mixed faults below both thresholds opened the breaker: %v", err)
	}
	_ = mustAllow(t, b, proxyA).Done(errUnreachable())
	wantRefusal(t, b, proxyA)
}

// A proxy the operator replaced is left behind with the breaker open and no
// request to probe it. Its state must not be kept for ever.
func TestStateOfAProxyNothingAsksAboutIsForgotten(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)

	clock.Advance(staleAfter - time.Minute)
	if open := b.Open(); len(open) != 1 {
		t.Fatalf("Open() = %+v before the state went stale, want the broken proxy", open)
	}
	clock.Advance(2 * time.Minute)
	if open := b.Open(); len(open) != 0 {
		t.Fatalf("Open() = %+v, want the abandoned proxy forgotten", open)
	}

	// A proxy that is still asked about stays, however long it is out.
	openBreaker(t, b, proxyB)
	for range 5 {
		clock.Advance(staleAfter - time.Minute)
		if err := b.Check(proxyB); err != nil {
			t.Fatalf("Check refused although the wait is long over: %v", err)
		}
	}
	if open := b.Open(); len(open) != 1 {
		t.Fatalf("Open() = %+v, want the proxy still in use kept", open)
	}
}

func TestConcurrentUnreachableFailuresAllCount(t *testing.T) {
	b := New(Config{Now: newFakeClock().Now})
	// Requests that were all admitted before any of them failed, as during
	// the first seconds of an outage.
	permits := make([]*Permit, DefaultUnreachableThreshold)
	for i := range permits {
		permits[i] = mustAllow(t, b, proxyA)
	}
	for _, permit := range permits {
		_ = permit.Done(errUnreachable())
	}
	wantRefusal(t, b, proxyA)
}

func TestProxiesWithDifferentCredentialsAreSeparate(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)

	wantRefusal(t, b, proxyA)
	for range 3 {
		if err := mustAllow(t, b, proxyB).Done(nil); err != nil {
			t.Fatalf("the other credentials' request failed: %v", err)
		}
	}
	if err := b.Check(proxyB); err != nil {
		t.Fatalf("Check refused the proxy entry that works: %v", err)
	}
	open := b.Open()
	if len(open) != 1 {
		t.Fatalf("Open() = %+v, want only the failing entry", open)
	}
	if strings.Contains(open[0].Label, "alice") || strings.Contains(open[0].Label, "secret") {
		t.Fatalf("label %q reveals the credentials", open[0].Label)
	}
	if !strings.HasPrefix(open[0].Label, "socks5://203.0.113.7:443 (credentials #") {
		t.Fatalf("label = %q, want the endpoint and a credentials fingerprint", open[0].Label)
	}
	if labelOf(proxyA) == labelOf(proxyB) {
		t.Fatal("two credentials on one endpoint got the same label")
	}
}

func TestOutcomesOfRequestsAdmittedBeforeOpeningAreIgnored(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	early := mustAllow(t, b, proxyA)
	alsoEarly := mustAllow(t, b, proxyA)
	openBreaker(t, b, proxyA)
	clock.Advance(5 * time.Second)

	// It reused a connection from before the outage. That does not show new
	// connections can be opened, so the breaker stays open.
	if err := early.Done(nil); err != nil {
		t.Fatalf("Done(nil) = %v", err)
	}
	if refusal := mustRefuse(t, b, proxyA); refusal.RetryAfter != DefaultBaseOpen-5*time.Second {
		t.Fatalf("RetryAfter = %s, want the wait to run on unchanged", refusal.RetryAfter)
	}
	// And a late failure of the same batch must not start the wait over.
	failure, _ := AsError(alsoEarly.Done(errRejected()))
	if failure == nil || failure.RetryAfter != DefaultBaseOpen-5*time.Second {
		t.Fatalf("late failure = %+v, want it to report the wait already running", failure)
	}
	if refusal := mustRefuse(t, b, proxyA); refusal.RetryAfter != DefaultBaseOpen-5*time.Second {
		t.Fatalf("RetryAfter = %s after a late failure, want it unchanged", refusal.RetryAfter)
	}
}

func TestProbeThatWasCancelledGivesTheSlotBack(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)
	clock.Advance(DefaultBaseOpen)

	probe := mustAllow(t, b, proxyA)
	wantRefusal(t, b, proxyA)
	if err := probe.Done(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Done = %v, want the cancellation passed through", err)
	}
	// The client went away before the handshake settled: the next request
	// probes straight away instead of waiting out the probe timeout.
	next := mustAllow(t, b, proxyA)
	if !next.probe {
		t.Fatal("the request after a cancelled probe is not the new probe")
	}
	// The cancelled probe's outcome no longer counts.
	probe.Connected()
	wantRefusal(t, b, proxyA)
	next.Connected()
	mustAllow(t, b, proxyA)
}

func TestProbeThatNeverAnswersIsReplaced(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)
	clock.Advance(DefaultBaseOpen)

	stuck := mustAllow(t, b, proxyA)
	clock.Advance(DefaultProbeTimeout - time.Second)
	wantRefusal(t, b, proxyA)
	clock.Advance(time.Second)
	replacement := mustAllow(t, b, proxyA)
	if !replacement.probe {
		t.Fatal("no new probe was admitted after the probe timeout")
	}
	// The stuck probe finally fails; it belongs to a round that is over.
	_ = stuck.Done(errRejected())
	replacement.Connected()
	if permit := mustAllow(t, b, proxyA); permit.probe {
		t.Fatal("the breaker did not close on the replacement probe")
	}
}

func TestProbeEndingOnAnotherKindOfFailureCloses(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)
	clock.Advance(DefaultBaseOpen)

	// The proxy took the credentials and then reported it cannot reach the
	// destination. That is not one of the faults the breaker opens for.
	probe := mustAllow(t, b, proxyA)
	errUpstream := errors.New("socks connect tcp4 203.0.113.7:443->chatgpt.com:443: unknown error host unreachable")
	if err := probe.Done(errUpstream); err != errUpstream {
		t.Fatalf("Done = %v, want the error unchanged", err)
	}
	if permit := mustAllow(t, b, proxyA); permit.probe {
		t.Fatal("the breaker stayed open after the proxy accepted the credentials")
	}
}

func TestDisabledBreakerLetsEverythingThrough(t *testing.T) {
	b := New(Config{Now: newFakeClock().Now})
	openBreaker(t, b, proxyA)
	b.SetEnabled(false)

	permit, err := b.Allow(proxyA)
	if permit != nil || err != nil {
		t.Fatalf("Allow = %v, %v while disabled, want nothing", permit, err)
	}
	if err = b.Check(proxyA); err != nil {
		t.Fatalf("Check = %v while disabled", err)
	}
	// A fault is passed through untouched, as before the breaker existed.
	fault := errRejected()
	if got := permit.Done(fault); got != fault {
		t.Fatalf("Done = %v while disabled, want the error unchanged", got)
	}

	b.SetEnabled(true)
	if permit = mustAllow(t, b, proxyA); permit.probe {
		t.Fatal("state from before the breaker was switched off survived")
	}
}

func TestEnvSwitchTurnsTheBreakerOffOnlyWhenAskedTo(t *testing.T) {
	for _, value := range []string{"off", " OFF ", "false", "0", "disabled"} {
		if EnabledByEnv(value) {
			t.Errorf("EnabledByEnv(%q) = true, want the breaker off", value)
		}
	}
	// Unset, and anything that does not plainly say off, leaves it on: a typo
	// must not silently remove the protection.
	for _, value := range []string{"", "on", "true", "1", "of", "no"} {
		if !EnabledByEnv(value) {
			t.Errorf("EnabledByEnv(%q) = false, want the breaker on", value)
		}
	}
}

func TestNoProxyAndNilBreakerAreAlwaysAdmitted(t *testing.T) {
	b := New(Config{})
	if permit, err := b.Allow("  "); permit != nil || err != nil {
		t.Fatalf("Allow(no proxy) = %v, %v", permit, err)
	}
	var none *Breaker
	if permit, err := none.Allow(proxyA); permit != nil || err != nil {
		t.Fatalf("nil breaker Allow = %v, %v", permit, err)
	}
	if err := none.Check(proxyA); err != nil {
		t.Fatalf("nil breaker Check = %v", err)
	}
	none.SetEnabled(false)
	none.Reset()
	if open := none.Open(); open != nil {
		t.Fatalf("nil breaker Open() = %v", open)
	}
}

func TestErrorTextNamesNeitherProxyNorUpstream(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})

	failure := mustAllow(t, b, proxyA).Done(errRejected())
	_ = mustAllow(t, b, proxyA).Done(errRejected())
	refusal := mustRefuse(t, b, proxyA)
	unreachable := (&Permit{breaker: b, key: proxyB}).Done(&HopError{Fault: FaultUnreachable, Err: context.DeadlineExceeded})

	for _, err := range []error{failure, refusal, unreachable} {
		text := err.Error()
		for _, secret := range []string{"203.0.113.7", "chatgpt.com", "alice", "secret", "bob"} {
			if strings.Contains(text, secret) {
				t.Fatalf("error text %q names %q", text, secret)
			}
		}
	}
	if got, want := failure.Error(), "egress proxy unavailable: proxy authentication rejected"; got != want {
		t.Fatalf("failure text = %q, want %q", got, want)
	}
	if got, want := refusal.Error(), "egress proxy unavailable (proxy authentication rejected); request not sent, retry in 15s"; got != want {
		t.Fatalf("refusal text = %q, want %q", got, want)
	}
	if got, want := unreachable.Error(), "egress proxy unavailable: cannot connect to the proxy (timeout)"; got != want {
		t.Fatalf("unreachable text = %q, want %q", got, want)
	}
	if seconds := (&Error{RetryAfter: 1200 * time.Millisecond}).RetryAfterSeconds(); seconds != 2 {
		t.Fatalf("RetryAfterSeconds = %d, want the wait rounded up to 2", seconds)
	}
}
