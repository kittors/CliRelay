package executor

import (
	"context"
	"testing"
	"time"
)

type unrelatedContextKey struct{}

func sampleQuotaObservation() QuotaObservation {
	return QuotaObservation{
		ObservedAt: time.Unix(1_800_000_000, 0),
		Windows:    []QuotaWindowObservation{{Label: "5h", UsedPercent: 42, WindowMinutes: 300}},
	}
}

func TestObserveQuotaReachesTheObserverOnTheContext(t *testing.T) {
	var got []QuotaObservation
	ctx := WithQuotaObserver(context.Background(), func(o QuotaObservation) { got = append(got, o) })

	if !HasQuotaObserver(ctx) {
		t.Fatal("HasQuotaObserver = false after WithQuotaObserver")
	}
	ObserveQuota(ctx, sampleQuotaObservation())
	if len(got) != 1 || got[0].Windows[0].UsedPercent != 42 {
		t.Fatalf("observer got %+v, want the one observation", got)
	}

	// The observer travels down the context tree, which is how it gets from the
	// scheduler to the executor's own derived contexts.
	child, cancel := context.WithTimeout(context.WithValue(ctx, unrelatedContextKey{}, "x"), time.Minute)
	defer cancel()
	ObserveQuota(child, sampleQuotaObservation())
	if len(got) != 2 {
		t.Fatalf("a derived context must reach the same observer, got %d calls", len(got))
	}
}

func TestObserveQuotaWithNoOneListeningDoesNothing(t *testing.T) {
	// Every call that does not come through the scheduler (token probes, status
	// refreshes, tests) has no observer and must not notice.
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"plain background context", context.Background()},
		{"nil context", nil},
		{"nil observer is not attached", WithQuotaObserver(context.Background(), nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if HasQuotaObserver(tc.ctx) {
				t.Fatal("HasQuotaObserver = true with no observer attached")
			}
			ObserveQuota(tc.ctx, sampleQuotaObservation()) // must not panic
		})
	}
}

func TestObserveQuotaSkipsObservationsWithoutWindows(t *testing.T) {
	calls := 0
	ctx := WithQuotaObserver(context.Background(), func(QuotaObservation) { calls++ })

	ObserveQuota(ctx, QuotaObservation{ObservedAt: time.Now()})
	ObserveQuota(ctx, QuotaObservation{Windows: []QuotaWindowObservation{}})
	if calls != 0 {
		t.Fatalf("an observation without windows says nothing and must not be delivered, got %d calls", calls)
	}
}

func TestWithQuotaObserverOnNilContextStartsFromBackground(t *testing.T) {
	calls := 0
	//nolint:staticcheck // a nil parent is the case under test.
	ctx := WithQuotaObserver(nil, func(QuotaObservation) { calls++ })
	if ctx == nil {
		t.Fatal("WithQuotaObserver returned a nil context")
	}
	ObserveQuota(ctx, sampleQuotaObservation())
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestInnerObserverShadowsTheOuterOne(t *testing.T) {
	// A retry binds a fresh observer to the next account; the earlier attempt's
	// observer must not receive what the new attempt reads.
	var outer, inner int
	ctx := WithQuotaObserver(context.Background(), func(QuotaObservation) { outer++ })
	ctx = WithQuotaObserver(ctx, func(QuotaObservation) { inner++ })
	ObserveQuota(ctx, sampleQuotaObservation())
	if outer != 0 || inner != 1 {
		t.Fatalf("outer=%d inner=%d, want 0 and 1", outer, inner)
	}
}
