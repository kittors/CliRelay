package auth

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// promisedBusyWaitRounds is the contract the tests below hold the scheduler to:
// one request waits at most twice. It is a literal here, not the scheduler's own
// constant, so that raising the constant fails these tests instead of moving
// them along.
const promisedBusyWaitRounds = 2

// Busy waiting is opt-in: a deployment that never configured a group, and a
// group that configured something else, behave as they did before it existed.
func TestBusyWaitIsOffUnlessAGroupAsksForIt(t *testing.T) {
	t.Run("no routing configuration", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			exec := &scriptedExecutor{}
			mgr := newPlainManager(t, exec, "bw-unset")
			park(mgr, "bw-unset", upstreamDown, 15*time.Second)
			start := time.Now()
			err := execute(mgr, context.Background())
			if err == nil || time.Since(start) != 0 {
				t.Fatalf("err = %v after %s, want an immediate failure", err, time.Since(start))
			}
			if got := headersFromError(err).Get("Retry-After"); got != strconv.Itoa(15) {
				t.Fatalf("Retry-After = %q, want the 15s the scheduler always derived", got)
			}
		})
	})
	t.Run("a group that sets only the first-token timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			exec := &scriptedExecutor{}
			mgr := newPlainManager(t, exec, "bw-other-guard")
			setDefaultGroup(mgr, 20, nil)
			park(mgr, "bw-other-guard", upstreamDown, 15*time.Second)
			start := time.Now()
			if err := execute(mgr, context.Background()); err == nil || time.Since(start) != 0 {
				t.Fatalf("err = %v after %s, want an immediate failure", err, time.Since(start))
			}
		})
	})
}

// A load-shed back-off (the 5-second tier, which a first-token timeout also
// gets) and an upstream asking for a few seconds are not waited for: the pool
// is back long before a client's own retry, and waiting would only send every
// parked request back to a struggling provider together. The request fails at
// once, as before busy waiting existed.
//
// Each case lets the first attempt fail with the brief back-off (the selector
// already lets a request through to an account whose block ends within
// optimisticRetryWindow, so a pre-parked account would simply be tried).
func TestBusyWaitDoesNotWaitForBriefBackoffs(t *testing.T) {
	cases := []struct {
		name    string
		failure error
		status  int
	}{
		{"load-shed tier", &Error{HTTPStatus: http.StatusServiceUnavailable, Code: "server_is_overloaded", Message: "server_is_overloaded"}, http.StatusServiceUnavailable},
		{"first-token timeout", newFirstTokenTimeoutError(30 * time.Second), http.StatusGatewayTimeout},
		{"upstream asks for a few seconds", &retryAfterError{status: http.StatusServiceUnavailable, message: "unavailable", retryAfter: 3 * time.Second}, http.StatusServiceUnavailable},
		{"upstream asks for exactly the tier", &retryAfterError{status: http.StatusServiceUnavailable, message: "unavailable", retryAfter: transientUpstreamOverloadCooldown}, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				exec := &scriptedExecutor{script: []error{tc.failure}}
				mgr := newBusyWaitManager(t, exec, "brief")
				start := time.Now()
				err := execute(mgr, context.Background())
				if statusCodeFromError(err) != tc.status || time.Since(start) != 0 {
					t.Fatalf("err = %v after %s, want the %d at once", err, time.Since(start), tc.status)
				}
				if calls := exec.Calls(); len(calls) != 1 {
					t.Fatalf("calls = %v, want the one attempt", calls)
				}
			})
		})
	}

	// One step past the tier is a back-off worth waiting for.
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{script: []error{&retryAfterError{status: http.StatusServiceUnavailable, message: "unavailable", retryAfter: transientUpstreamOverloadCooldown + time.Second}}}
		mgr := newBusyWaitManager(t, exec, "just-over")
		mgr.busyWaitSpread = fixedSpread(0)
		start := time.Now()
		if err := execute(mgr, context.Background()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if waited := time.Since(start); waited != transientUpstreamOverloadCooldown+time.Second {
			t.Fatalf("waited %s, want the 6s until the account recovers", waited)
		}
		if calls := exec.Calls(); len(calls) != 2 {
			t.Fatalf("calls = %v, want the failed attempt and the one after the wait", calls)
		}
	})

	// An account that is back within seconds makes the whole pool "not worth
	// holding the request for", even when another one is parked for a minute:
	// the client's retry finds the first one, a wait would sit out the second.
	synctest.Test(t, func(t *testing.T) {
		overloaded := &Error{HTTPStatus: http.StatusServiceUnavailable, Code: "server_is_overloaded", Message: "server_is_overloaded"}
		exec := &scriptedExecutor{script: []error{overloaded}}
		mgr := newBusyWaitManager(t, exec, "soon", "later")
		park(mgr, "later", upstreamDown, 40*time.Second)
		start := time.Now()
		err := execute(mgr, context.Background())
		if statusCodeFromError(err) != http.StatusServiceUnavailable || time.Since(start) != 0 {
			t.Fatalf("err = %v after %s, want the 503 at once", err, time.Since(start))
		}
		if calls := exec.Calls(); len(calls) != 1 || calls[0] != "soon" {
			t.Fatalf("calls = %v, want the one attempt on the account that is free", calls)
		}
	})
}

// An upstream that keeps answering "overloaded" must cost it one attempt per
// account, not one pass per five seconds for as long as the budget lasts:
// before brief back-offs were left out, a 60s budget over a three-account pool
// sent more than thirty requests upstream, at the moment it was struggling.
func TestOverloadedUpstreamIsNotAmplifiedByBusyWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		overloaded := &Error{HTTPStatus: http.StatusServiceUnavailable, Code: "server_is_overloaded", Message: "server_is_overloaded"}
		script := make([]error, 200)
		for i := range script {
			script[i] = overloaded
		}
		exec := &scriptedExecutor{script: script}
		mgr := newBusyWaitManager(t, exec, "o1", "o2", "o3")
		start := time.Now()
		err := execute(mgr, context.Background())
		if statusCodeFromError(err) != http.StatusServiceUnavailable {
			t.Fatalf("err = %v, want the upstream's 503", err)
		}
		if took := time.Since(start); took != 0 {
			t.Fatalf("took %s, want an immediate failure", took)
		}
		if calls := exec.Calls(); len(calls) != 3 {
			t.Fatalf("upstream calls = %v, want one per account", calls)
		}
	})
}

// However the cooldowns line up, one request waits at most twice: each wait
// ends in a pass over the pool, and the rounds are what bound the upstream
// calls a request can cause. The time budget alone (60s here) would have
// allowed four waits of this length.
func TestBusyWaitWaitsAtMostTwicePerRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failing := make([]error, 20)
		for i := range failing {
			failing[i] = &retryAfterError{status: http.StatusServiceUnavailable, message: "unavailable", retryAfter: 12 * time.Second}
		}
		exec := &scriptedExecutor{script: failing}
		mgr := newBusyWaitManager(t, exec, "rounds")
		mgr.busyWaitSpread = fixedSpread(0)

		start := time.Now()
		err := execute(mgr, context.Background())
		if statusCodeFromError(err) != http.StatusServiceUnavailable {
			t.Fatalf("err = %v, want the 503", err)
		}
		if took := time.Since(start); took != promisedBusyWaitRounds*12*time.Second {
			t.Fatalf("took %s, want %d waits of 12s", took, promisedBusyWaitRounds)
		}
		if calls := exec.Calls(); len(calls) != promisedBusyWaitRounds+1 {
			t.Fatalf("upstream calls = %v, want the first attempt and one after each wait", calls)
		}
	})
}

// Handler-level failover starts a new scheduling pass for the same request; the
// rounds belong to the request, not to the pass.
func TestBusyWaitRoundsAreSharedAcrossSchedulingPasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "shared")
		mgr.busyWaitSpread = fixedSpread(0)
		ctx := WithRetryWaitBudget(context.Background())
		// Past optimisticRetryWindow, or the selector would try the account at
		// once instead of leaving the request to wait.
		const cooldown = 12 * time.Second

		for pass := 1; pass <= promisedBusyWaitRounds; pass++ {
			park(mgr, "shared", upstreamDown, cooldown)
			start := time.Now()
			if err := execute(mgr, ctx); err != nil || time.Since(start) != cooldown {
				t.Fatalf("pass %d: err = %v after %s, want to wait %s and be served", pass, err, time.Since(start), cooldown)
			}
		}
		park(mgr, "shared", upstreamDown, cooldown)
		start := time.Now()
		if err := execute(mgr, ctx); err == nil || time.Since(start) != 0 {
			t.Fatalf("after %d waits: err = %v after %s, want an immediate failure", promisedBusyWaitRounds, err, time.Since(start))
		}

		// A request of its own starts with all its rounds again.
		fresh := WithRetryWaitBudget(context.Background())
		start = time.Now()
		if err := execute(mgr, fresh); err != nil || time.Since(start) != cooldown {
			t.Fatalf("a new request: err = %v after %s, want to wait %s and be served", err, time.Since(start), cooldown)
		}
	})
}

// A short 429 that comes back every time: the pool is tried once per pass and
// a request has three passes, so the upstream sees at most three attempts per
// account however many accounts there are. Retry-After of 8s is the shortest
// back-off that is waited for; a shorter one is not waited for at all and costs
// one attempt per account.
func TestShortRateLimitsCostAtMostThreeAttemptsPerAccount(t *testing.T) {
	const accounts = 4
	ids := []string{"rl-1", "rl-2", "rl-3", "rl-4"}
	limited := func(retryAfter time.Duration) []error {
		script := make([]error, 100)
		for i := range script {
			script[i] = &retryAfterError{status: http.StatusTooManyRequests, message: "rate_limit: too many requests per minute", retryAfter: retryAfter}
		}
		return script
	}

	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{script: limited(8 * time.Second)}
		mgr := newBusyWaitManager(t, exec, ids...)
		mgr.busyWaitSpread = fixedSpread(0)
		start := time.Now()
		err := execute(mgr, context.Background())
		if statusCodeFromError(err) != http.StatusTooManyRequests {
			t.Fatalf("err = %v, want the 429", err)
		}
		if calls := exec.Calls(); len(calls) != (promisedBusyWaitRounds+1)*accounts {
			t.Fatalf("upstream calls = %d, want %d (a pass per round, plus the first)", len(calls), (promisedBusyWaitRounds+1)*accounts)
		}
		if took := time.Since(start); took != promisedBusyWaitRounds*8*time.Second {
			t.Fatalf("took %s, want %d waits of 8s", took, promisedBusyWaitRounds)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{script: limited(3 * time.Second)}
		mgr := newBusyWaitManager(t, exec, ids...)
		start := time.Now()
		err := execute(mgr, context.Background())
		if statusCodeFromError(err) != http.StatusTooManyRequests {
			t.Fatalf("err = %v, want the 429", err)
		}
		if calls := exec.Calls(); len(calls) != accounts {
			t.Fatalf("upstream calls = %d, want one per account", len(calls))
		}
		if took := time.Since(start); took != 0 {
			t.Fatalf("took %s, want an immediate failure", took)
		}
	})
}

// The cap on simultaneous waiters is the configured one, applies to the waits
// that start after a change, and never evicts a request already waiting.
func TestBusyWaitCapFollowsTheConfiguration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "cap")
		park(mgr, "cap", upstreamDown, 30*time.Second)
		mgr.SetBusyWaitConfig(3)

		var mu sync.Mutex
		var wg sync.WaitGroup
		immediate, served := 0, 0
		launch := func(n int) {
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					start := time.Now()
					err := execute(mgr, context.Background())
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err != nil && time.Since(start) == 0:
						immediate++
					case err == nil:
						served++
					}
				}()
			}
			synctest.Wait()
		}

		launch(5)
		if got := mgr.busyWaiters.Load(); got != 3 {
			t.Fatalf("waiting = %d, want the cap of 3", got)
		}
		mu.Lock()
		if immediate != 2 {
			t.Fatalf("failed at once = %d, want the 2 over the cap", immediate)
		}
		mu.Unlock()

		// Lowering the cap leaves the three waiting; raising it admits more.
		mgr.SetBusyWaitConfig(1)
		launch(1)
		mu.Lock()
		if immediate != 3 {
			t.Fatalf("failed at once = %d, want the newcomer turned away under a cap of 1", immediate)
		}
		mu.Unlock()
		mgr.SetBusyWaitConfig(10)
		launch(2)
		if got := mgr.busyWaiters.Load(); got != 5 {
			t.Fatalf("waiting = %d after raising the cap, want 5", got)
		}

		wg.Wait()
		if served != 5 {
			t.Fatalf("served = %d, want all five that waited", served)
		}
		if got := mgr.busyWaiters.Load(); got != 0 {
			t.Fatalf("waiter slots leaked: %d", got)
		}
	})
}

// A Manager the host never configured, and one configured with 0, take the
// default cap; neither means unlimited.
func TestBusyWaitCapDefaultsWhenUnsetOrZero(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "cap-default")
		park(mgr, "cap-default", upstreamDown, 30*time.Second)
		if got := mgr.BusyWaitMaxWaiters(); got != 64 {
			t.Fatalf("unset cap = %d, want the default 64", got)
		}
		mgr.SetBusyWaitConfig(0)
		if got := mgr.BusyWaitMaxWaiters(); got != 64 {
			t.Fatalf("cap after 0 = %d, want the default 64", got)
		}
		mgr.SetBusyWaitConfig(-7)
		if got := mgr.BusyWaitMaxWaiters(); got != 64 {
			t.Fatalf("cap after a negative value = %d, want the default 64", got)
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		immediate := 0
		for range 70 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				if err := execute(mgr, context.Background()); err != nil && time.Since(start) == 0 {
					mu.Lock()
					immediate++
					mu.Unlock()
				}
			}()
		}
		synctest.Wait()
		if got := mgr.busyWaiters.Load(); got != 64 {
			t.Fatalf("waiting = %d, want the default cap of 64", got)
		}
		wg.Wait()
		if immediate != 6 {
			t.Fatalf("failed at once = %d, want the 6 over the cap", immediate)
		}
	})
}

// setDefaultGroup configures the manager's default group with a first-token
// timeout and, when given, a busy-wait budget.
func setDefaultGroup(mgr *Manager, firstTokenSeconds int, busyWaitSeconds *int) {
	mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{ChannelGroups: []sdkconfig.RoutingChannelGroup{
		{Name: "default", FirstTokenTimeoutSeconds: firstTokenSeconds, BusyWaitMaxSeconds: busyWaitSeconds},
	}}})
}
