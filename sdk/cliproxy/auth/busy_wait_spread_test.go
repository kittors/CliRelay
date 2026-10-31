package auth

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// seededSpread draws the waiters' offsets from one seeded generator, so a
// distribution test sees the same numbers on every run. Whichever waiter asks
// first gets the first draw, but the draws themselves, and therefore the
// histogram of wake-ups, do not depend on that order.
func seededSpread(seed uint64) func(time.Duration) time.Duration {
	var mu sync.Mutex
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	return func(window time.Duration) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return time.Duration(rng.Int64N(int64(window)))
	}
}

// runWaiters starts n requests, request i arriving arrival(i) after the call,
// and returns when each of them was served, measured from the call.
func runWaiters(t *testing.T, mgr *Manager, n int, arrival func(i int) time.Duration) []time.Duration {
	t.Helper()
	base := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	wakes := make([]time.Duration, 0, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(arrival(i))
			if err := execute(mgr, context.Background()); err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			mu.Lock()
			wakes = append(wakes, time.Since(base))
			mu.Unlock()
		}()
	}
	wg.Wait()
	return wakes
}

// checkWakeWindow verifies that every wake-up falls in [recovery, recovery+busyWaitJitter)
// and that the window is filled evenly: split into six slices, none may hold
// fewer than minShare or more than maxShare of the wake-ups.
func checkWakeWindow(t *testing.T, wakes []time.Duration, recovery time.Duration, minShare, maxShare float64) {
	t.Helper()
	const slices = 6
	counts := make([]int, slices)
	for _, wake := range wakes {
		if wake < recovery || wake >= recovery+busyWaitJitter {
			t.Fatalf("a request resumed at %s, outside [%s, %s)", wake, recovery, recovery+busyWaitJitter)
		}
		counts[int((wake-recovery)*slices/busyWaitJitter)]++
	}
	expected := float64(len(wakes)) / slices
	t.Logf("wake-ups per %s slice of the window: %v", busyWaitJitter/slices, counts)
	for slice, got := range counts {
		if float64(got) < minShare*expected || float64(got) > maxShare*expected {
			t.Fatalf("wake-ups per %s slice of the window: %v; slice %d holds %d, expected about %.0f (allowed %.0f to %.0f)",
				busyWaitJitter/slices, counts, slice, got, expected, minShare*expected, maxShare*expected)
		}
	}
}

// Every request that found the pool parked at the same moment waits for the
// same recovery. They must not all resume in the instant it happens: they are
// spread over a window of fixed length, even when the wait fills the whole
// budget (the case where a spread taken from the leftover budget is nil).
func TestBusyWaitSpreadsSimultaneousWaitersOverTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "herd")
		mgr.SetBusyWaitConfig(1000)
		mgr.busyWaitSpread = seededSpread(1)
		park(mgr, "herd", upstreamDown, 60*time.Second)

		wakes := runWaiters(t, mgr, 200, func(int) time.Duration { return 0 })
		checkWakeWindow(t, wakes, 60*time.Second, 0.5, 1.5)
	})
}

// Under steady arrival the requests that start waiting later have less time to
// wait, and a spread taken from the wait would shrink with it. The window is
// the same for all of them.
func TestBusyWaitSpreadsSteadyArrivalsOverTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "steady")
		mgr.SetBusyWaitConfig(1000)
		mgr.busyWaitSpread = seededSpread(2)
		park(mgr, "steady", upstreamDown, 60*time.Second)

		// One request every 100ms for 30s: waits from 60s down to 30s, all
		// longer than the selector's optimistic window.
		wakes := runWaiters(t, mgr, 300, func(i int) time.Duration { return time.Duration(i) * 100 * time.Millisecond })
		checkWakeWindow(t, wakes, 60*time.Second, 0.6, 1.4)
	})
}

// The window may carry a wake-up past the group's budget, never past the
// request's own deadline: the injected spread is asked for no more than what
// the deadline leaves after the recovery.
func TestBusyWaitSpreadStaysInsideTheRequestDeadline(t *testing.T) {
	cases := []struct {
		name       string
		deadline   time.Duration // 0: none
		wantWindow time.Duration
	}{
		{"no deadline", 0, busyWaitJitter},
		{"deadline far away", 5 * time.Minute, busyWaitJitter},
		{"deadline leaves less than the window", 16 * time.Second, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				exec := &scriptedExecutor{}
				mgr := newBusyWaitManager(t, exec, "deadline")
				var asked time.Duration
				mgr.busyWaitSpread = func(window time.Duration) time.Duration {
					asked = window
					return window / 2
				}
				park(mgr, "deadline", upstreamDown, 15*time.Second)

				ctx := context.Background()
				if tc.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.deadline)
					defer cancel()
				}
				start := time.Now()
				if err := execute(mgr, ctx); err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if asked != tc.wantWindow {
					t.Fatalf("spread asked for a %s window, want %s", asked, tc.wantWindow)
				}
				if waited, want := time.Since(start), 15*time.Second+tc.wantWindow/2; waited != want {
					t.Fatalf("waited %s, want %s", waited, want)
				}
			})
		})
	}
}

// The budget decides whether to wait at all, by the soonest recovery; the
// window then runs past it by up to its own length and no further.
func TestBusyWaitWindowMayRunPastTheBudgetButTheRecoveryMayNot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "budget-edge")
		enableBusyWait(mgr, 20)
		mgr.busyWaitSpread = func(window time.Duration) time.Duration { return window }
		park(mgr, "budget-edge", upstreamDown, 19*time.Second)
		start := time.Now()
		if err := execute(mgr, context.Background()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if waited := time.Since(start); waited != 19*time.Second+busyWaitJitter {
			t.Fatalf("waited %s, want the 19s recovery plus the whole %s window, which ends after the 20s budget", waited, busyWaitJitter)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "budget-over")
		enableBusyWait(mgr, 20)
		park(mgr, "budget-over", upstreamDown, 21*time.Second)
		start := time.Now()
		if err := execute(mgr, context.Background()); err == nil || time.Since(start) != 0 {
			t.Fatalf("err = %v after %s, want an immediate failure when the recovery itself is past the budget", err, time.Since(start))
		}
	})
}
