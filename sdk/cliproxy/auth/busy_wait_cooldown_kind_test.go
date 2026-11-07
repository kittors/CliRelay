package auth

import (
	"testing"
	"time"
)

// Which cooldowns a busy wait may sit out is decided by cooldownKindOf alone,
// so it is held to the table here with cooldown lengths the scheduler itself
// never produces for the other reasons (a revoked key is parked for 30 minutes
// in practice, far beyond any budget): were that ever shortened, the
// classification would be all that keeps a request waiting for a revoked key.
func TestCooldownKindOf(t *testing.T) {
	now := time.Now()
	modelState := func(failure *Error, quota QuotaState, recorded time.Time) *Auth {
		return &Auth{ID: "a", ModelStates: map[string]*ModelState{
			"test-model": {LastError: failure, Quota: quota, UpdatedAt: recorded},
		}}
	}
	accountWideQuota := modelState(throttled, QuotaState{}, now)
	accountWideQuota.Quota.Exceeded = true

	cases := []struct {
		name     string
		auth     *Auth
		cooldown time.Duration
		want     cooldownKind
	}{
		{"a 5xx parks for a minute", modelState(upstreamDown, QuotaState{}, now), time.Minute, cooldownWorthWaiting},
		{"a short 429 parks for a minute", modelState(throttled, QuotaState{}, now), time.Minute, cooldownWorthWaiting},
		{"the load-shed tier is brief", modelState(upstreamDown, QuotaState{}, now), transientUpstreamOverloadCooldown, cooldownBrief},
		{"anything shorter is brief too", modelState(throttled, QuotaState{}, now), 2 * time.Second, cooldownBrief},
		{"one tick past the tier is not", modelState(upstreamDown, QuotaState{}, now), transientUpstreamOverloadCooldown + time.Nanosecond, cooldownWorthWaiting},
		{"a state without a recorded time counts as long", modelState(upstreamDown, QuotaState{}, time.Time{}), 2 * time.Second, cooldownWorthWaiting},
		{"the account's own error is read when the model has no state", &Auth{ID: "a", LastError: upstreamDown, UpdatedAt: now}, time.Minute, cooldownWorthWaiting},
		{"a spent model quota", modelState(quotaGone, QuotaState{Exceeded: true}, now), 20 * time.Second, cooldownOther},
		{"a spent account-wide quota behind a rate-limited model", accountWideQuota, 20 * time.Second, cooldownOther},
		{"a revoked key", modelState(revoked, QuotaState{}, now), 20 * time.Second, cooldownOther},
		{"a forbidden key", modelState(&Error{HTTPStatus: 403, Message: "forbidden"}, QuotaState{}, now), 20 * time.Second, cooldownOther},
		{"a payment required", modelState(&Error{HTTPStatus: 402, Message: "payment required"}, QuotaState{}, now), 20 * time.Second, cooldownOther},
		{"a missing model", modelState(&Error{HTTPStatus: 404, Message: "not found"}, QuotaState{}, now), 20 * time.Second, cooldownOther},
		{"a failure that names no status", modelState(&Error{Message: "boom"}, QuotaState{}, now), 20 * time.Second, cooldownOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cooldownKindOf(tc.auth, "test-model", now.Add(tc.cooldown)); got != tc.want {
				t.Fatalf("cooldownKindOf = %d, want %d", got, tc.want)
			}
		})
	}
}
