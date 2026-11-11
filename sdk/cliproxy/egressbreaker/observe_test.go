package egressbreaker

import (
	"testing"
	"time"
)

func TestObserveDescribesTheOutage(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	if got := b.Observe(proxyA); got != (Observation{}) {
		t.Fatalf("Observe of an unknown proxy = %+v, want nothing known", got)
	}

	openedAt := clock.Now()
	openBreaker(t, b, proxyA)
	wantRefusal(t, b, proxyA)
	got := b.Observe(proxyA)
	if !got.Open || got.Fault != FaultAuthRejected || !got.Since.Equal(openedAt) ||
		!got.NextProbe.Equal(openedAt.Add(DefaultBaseOpen)) || got.Refused != 1 || got.FailedProbes != 0 || got.Probing {
		t.Fatalf("Observe = %+v, want the open outage with one refusal and no probe yet", got)
	}
	if got.Label != labelOf(proxyA) {
		t.Fatalf("Label = %q, want %q", got.Label, labelOf(proxyA))
	}

	clock.Advance(DefaultBaseOpen)
	probe := mustAllow(t, b, proxyA)
	if got = b.Observe(proxyA); !got.Probing {
		t.Fatalf("Observe while the probe is out = %+v, want Probing", got)
	}
	_ = probe.Done(errRejected())
	got = b.Observe(proxyA)
	if !got.Open || got.Probing || got.FailedProbes != 1 || !got.Since.Equal(openedAt) {
		t.Fatalf("Observe after a failed probe = %+v, want the same outage with one failed probe", got)
	}

	// A new outage counts its probes from zero.
	clock.Advance(2 * DefaultBaseOpen)
	mustAllow(t, b, proxyA).Connected()
	openBreaker(t, b, proxyA)
	if got = b.Observe(proxyA); got.FailedProbes != 0 || got.Refused != 0 {
		t.Fatalf("Observe of the next outage = %+v, want its counts from zero", got)
	}
}

// The console polls Observe. Asking must not keep alive the state of a proxy
// that no request uses any more, which Check would.
func TestObserveDoesNotKeepAForgottenProxyAlive(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)
	for range 6 {
		clock.Advance(staleAfter / 5)
		_ = b.Observe(proxyA)
	}
	if got := b.Observe(proxyA); got.Open {
		t.Fatalf("Observe = %+v after %s without a request, want the state treated as forgotten", got, staleAfter+staleAfter/5)
	}
	if open := b.Open(); len(open) != 0 {
		t.Fatalf("Open() = %+v, want the stale state dropped", open)
	}
}

func TestPassesAreRecordedOnlyWhenTheyProveTheProxyWorks(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})

	firstPass := clock.Now()
	if err := mustAllow(t, b, proxyA).Done(nil); err != nil {
		t.Fatalf("Done(nil) = %v", err)
	}
	if got := b.Observe(proxyA); !got.LastPass.Equal(firstPass) || got.Open {
		t.Fatalf("Observe after a request got through = %+v, want LastPass %v", got, firstPass)
	}
	if got := b.Observe(proxyB); !got.LastPass.IsZero() {
		t.Fatalf("Observe(proxyB) = %+v, want no pass: the credentials differ", got)
	}

	// A request admitted before the outage that reports in after it opened
	// may have used an old connection: it proves nothing and is not a pass.
	clock.Advance(time.Minute)
	early := mustAllow(t, b, proxyA)
	openBreaker(t, b, proxyA)
	early.Connected()
	if got := b.Observe(proxyA); !got.LastPass.Equal(firstPass) || !got.Open {
		t.Fatalf("Observe = %+v, want the outage open and the pass before it kept", got)
	}

	// The probe that ends the outage is a pass.
	clock.Advance(DefaultBaseOpen)
	recovered := clock.Now()
	mustAllow(t, b, proxyA).Connected()
	if got := b.Observe(proxyA); got.Open || !got.LastPass.Equal(recovered) {
		t.Fatalf("Observe after recovery = %+v, want closed with LastPass %v", got, recovered)
	}
}

func TestOldPassesAreForgotten(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	mustAllow(t, b, proxyA).Connected()
	clock.Advance(staleAfter + time.Minute)
	if got := b.Observe(proxyA); !got.LastPass.IsZero() {
		t.Fatalf("Observe = %+v, want a pass older than %s ignored", got, staleAfter)
	}
	// The first pass of another proxy clears the old ones out of the map.
	mustAllow(t, b, proxyB).Connected()
	b.mu.Lock()
	_, kept := b.passed[proxyA]
	b.mu.Unlock()
	if kept {
		t.Fatal("the stale pass of proxyA is still held")
	}

	b.Reset()
	if got := b.Observe(proxyB); got != (Observation{}) {
		t.Fatalf("Observe after Reset = %+v, want nothing", got)
	}
}

func TestObserveOfADisabledBreakerKnowsNothing(t *testing.T) {
	clock := newFakeClock()
	b := New(Config{Now: clock.Now})
	openBreaker(t, b, proxyA)
	if !b.Enabled() {
		t.Fatal("a new breaker reports itself off")
	}
	b.SetEnabled(false)
	if b.Enabled() {
		t.Fatal("Enabled() = true after SetEnabled(false)")
	}
	if got := b.Observe(proxyA); got != (Observation{}) {
		t.Fatalf("Observe of a disabled breaker = %+v, want nothing", got)
	}
	var none *Breaker
	if none.Enabled() || none.Observe(proxyA) != (Observation{}) {
		t.Fatal("a nil breaker claims to be on or to know something")
	}
	if got := b.Observe("  "); got != (Observation{}) {
		t.Fatalf("Observe of no proxy = %+v", got)
	}
}

func TestFaultCodes(t *testing.T) {
	for fault, want := range map[Fault]string{FaultNone: "", FaultAuthRejected: "auth_rejected", FaultUnreachable: "unreachable"} {
		if got := fault.Code(); got != want {
			t.Fatalf("%v.Code() = %q, want %q", fault, got, want)
		}
	}
}
