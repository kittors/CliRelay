package executor

import (
	"context"
	"time"
)

// Live quota observation.
//
// Some upstreams say how much of an account's quota is used on every answer, not
// only when a call is refused. An executor that reads this passes it to the
// observer the scheduler attached to the request context. That lets the
// scheduler act on a window that has just run out without spending a request on
// the refusal first, and lets the console show the number the upstream just gave
// instead of the one from the last status probe.

// QuotaWindowObservation is one quota window as the upstream reported it.
type QuotaWindowObservation struct {
	// Label is the provider's short name for the window length, such as "5h" or
	// "week". It is empty when the length is unknown.
	Label string
	// UsedPercent is the share of the window already used, from 0 to 100.
	UsedPercent float64
	// WindowMinutes is the length of the window; zero when the upstream did not
	// say.
	WindowMinutes int
	// ResetAt is when the window resets; zero when the upstream gave no usable
	// time.
	ResetAt time.Time
}

// QuotaObservation is what one upstream answer said about an account's quota.
//
// Windows belong to the account's main plan limit. Limits metered separately,
// such as a model with its own allowance, are not reported through it: using
// them up says nothing about whether the account can serve other models.
type QuotaObservation struct {
	// ObservedAt is when the upstream answered.
	ObservedAt time.Time
	Windows    []QuotaWindowObservation
	// OverageAvailable is true when the account keeps being served after its
	// windows are used up, for example from prepaid credits. A window at 100% then
	// is not an exhausted account.
	OverageAvailable bool
}

// QuotaObserver receives what an upstream answer said about quota. It runs on the
// request path, so it must return quickly and never block.
type QuotaObserver func(QuotaObservation)

type quotaObserverContextKey struct{}

// WithQuotaObserver returns a child context carrying observer. The scheduler
// attaches one per attempt, bound to the account and model that attempt uses.
func WithQuotaObserver(ctx context.Context, observer QuotaObserver) context.Context {
	if observer == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, quotaObserverContextKey{}, observer)
}

// HasQuotaObserver reports whether ObserveQuota would reach anyone. Executors use
// it to skip the work of reading an answer for quota when nothing is listening.
func HasQuotaObserver(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	observer, _ := ctx.Value(quotaObserverContextKey{}).(QuotaObserver)
	return observer != nil
}

// ObserveQuota hands an observation to the observer on ctx. It does nothing when
// there is none, which is the case for any call that did not come through the
// scheduler (token probes, status refreshes, tests).
func ObserveQuota(ctx context.Context, observation QuotaObservation) {
	if ctx == nil || len(observation.Windows) == 0 {
		return
	}
	if observer, _ := ctx.Value(quotaObserverContextKey{}).(QuotaObserver); observer != nil {
		observer(observation)
	}
}
