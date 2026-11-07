package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func percentObservation(used float64, resetAt time.Time) cliproxyexecutor.QuotaObservation {
	return observationOf(usedWindow(used, 300, "5h", resetAt))
}

func TestQuotaObservationSignature(t *testing.T) {
	reset := time.Unix(1_800_000_600, 0)
	base := quotaObservationSignature(percentObservation(42, reset))

	same := []struct {
		name        string
		observation cliproxyexecutor.QuotaObservation
	}{
		{"fractional usage that rounds to the same percent", percentObservation(42.4, reset)},
		{"a reset that moved within the same minute", percentObservation(42, reset.Add(20*time.Second))},
		{"the observation time", cliproxyexecutor.QuotaObservation{ObservedAt: reset, Windows: percentObservation(42, reset).Windows}},
	}
	for _, tc := range same {
		if got := quotaObservationSignature(tc.observation); got != base {
			t.Errorf("%s: signature %q differs from %q; it is jitter and would turn every answer into a write", tc.name, got, base)
		}
	}

	different := []struct {
		name        string
		observation cliproxyexecutor.QuotaObservation
	}{
		{"another whole percent", percentObservation(43, reset)},
		{"the next minute", percentObservation(42, reset.Add(time.Minute))},
		{"another window length", observationOf(usedWindow(42, 10080, "week", reset))},
		{"an extra window", observationOf(usedWindow(42, 300, "5h", reset), usedWindow(10, 10080, "week", reset))},
		{"no reset", percentObservation(42, time.Time{})},
	}
	for _, tc := range different {
		if got := quotaObservationSignature(tc.observation); got == base {
			t.Errorf("%s: signature did not change, so the change would never be written", tc.name)
		}
	}

	// The order the upstream lists windows in is not meaningful.
	a := observationOf(usedWindow(42, 300, "5h", reset), usedWindow(10, 10080, "week", reset))
	b := observationOf(usedWindow(10, 10080, "week", reset), usedWindow(42, 300, "5h", reset))
	if quotaObservationSignature(a) != quotaObservationSignature(b) {
		t.Error("window order changes the signature")
	}
}

func TestQuotaObservationBookCoalescesWrites(t *testing.T) {
	const minInterval, refresh = 20 * time.Second, 5 * time.Minute
	t0 := time.Unix(1_800_000_000, 0)
	reset := t0.Add(time.Hour)
	book := &quotaObservationBook{minInterval: minInterval, refresh: refresh}

	// The first answer for an account is written at once.
	if action, _ := book.offer("a", percentObservation(10, reset), t0); action != quotaObservationWriteNow {
		t.Fatalf("first offer = %v, want write now", action)
	}
	// What was just written is not written again, however often it is repeated.
	for i := 1; i <= 10; i++ {
		if action, _ := book.offer("a", percentObservation(10, reset), t0.Add(time.Duration(i)*time.Second)); action != quotaObservationDrop {
			t.Fatalf("repeat %d = %v, want drop", i, action)
		}
	}
	// A change inside the minimum interval waits for its turn, and arms one timer.
	action, delay := book.offer("a", percentObservation(20, reset), t0.Add(5*time.Second))
	if action != quotaObservationHoldAndArm || delay != minInterval-5*time.Second {
		t.Fatalf("early change = %v after %v, want hold-and-arm after %v", action, delay, minInterval-5*time.Second)
	}
	// Further changes replace the held one without arming a second timer.
	if action, _ := book.offer("a", percentObservation(30, reset), t0.Add(8*time.Second)); action != quotaObservationHold {
		t.Fatalf("second early change = %v, want hold", action)
	}
	held, ok := book.take("a", t0.Add(minInterval))
	if !ok || int(held.Windows[0].UsedPercent) != 30 {
		t.Fatalf("take = %+v ok=%v, want the newest held observation (30%%)", held, ok)
	}
	// Taking it counts as the write: the same numbers are not written again...
	if action, _ := book.offer("a", percentObservation(30, reset), t0.Add(minInterval+time.Second)); action != quotaObservationDrop {
		t.Fatalf("offer of the just-written numbers = %v, want drop", action)
	}
	// ...and the interval restarts from the moment of that write.
	if action, delay := book.offer("a", percentObservation(40, reset), t0.Add(minInterval+2*time.Second)); action != quotaObservationHoldAndArm || delay != minInterval-2*time.Second {
		t.Fatalf("change right after a write = %v after %v", action, delay)
	}
}

func TestQuotaObservationBookWritesChangesOnceTheIntervalHasPassed(t *testing.T) {
	const minInterval, refresh = 20 * time.Second, 5 * time.Minute
	t0 := time.Unix(1_800_000_000, 0)
	book := &quotaObservationBook{minInterval: minInterval, refresh: refresh}
	reset := t0.Add(time.Hour)

	book.offer("a", percentObservation(10, reset), t0)
	if action, _ := book.offer("a", percentObservation(11, reset), t0.Add(minInterval)); action != quotaObservationWriteNow {
		t.Fatalf("a change at exactly the minimum interval = %v, want write now", action)
	}
}

func TestQuotaObservationBookRefreshesAnUnchangedAccountOccasionally(t *testing.T) {
	const minInterval, refresh = 20 * time.Second, 5 * time.Minute
	t0 := time.Unix(1_800_000_000, 0)
	book := &quotaObservationBook{minInterval: minInterval, refresh: refresh}
	reset := t0.Add(time.Hour)

	book.offer("a", percentObservation(10, reset), t0)
	if action, _ := book.offer("a", percentObservation(10, reset), t0.Add(refresh-time.Second)); action != quotaObservationDrop {
		t.Fatalf("unchanged just before the refresh interval = %v, want drop", action)
	}
	// The console shows how long ago each window was last confirmed; an account that
	// is used but steady would otherwise look abandoned.
	if action, _ := book.offer("a", percentObservation(10, reset), t0.Add(refresh)); action != quotaObservationWriteNow {
		t.Fatalf("unchanged at the refresh interval = %v, want write now", action)
	}
}

func TestQuotaObservationBookDropsAHeldChangeThatWasUndone(t *testing.T) {
	const minInterval, refresh = 20 * time.Second, 5 * time.Minute
	t0 := time.Unix(1_800_000_000, 0)
	book := &quotaObservationBook{minInterval: minInterval, refresh: refresh}
	reset := t0.Add(time.Hour)

	book.offer("a", percentObservation(10, reset), t0)
	if action, _ := book.offer("a", percentObservation(11, reset), t0.Add(2*time.Second)); action != quotaObservationHoldAndArm {
		t.Fatalf("change = %v, want hold-and-arm", action)
	}
	// The next answer is back to what is stored (a window reset, say): nothing is
	// left to write, and the timer must not write the stale change.
	if action, _ := book.offer("a", percentObservation(10, reset), t0.Add(4*time.Second)); action != quotaObservationDrop {
		t.Fatalf("answer matching the stored one = %v, want drop", action)
	}
	if _, ok := book.take("a", t0.Add(minInterval)); ok {
		t.Fatal("the timer found a held observation that had been undone")
	}
	// The timer is spent, so the next change can arm a new one.
	if action, _ := book.offer("a", percentObservation(12, reset), t0.Add(minInterval+time.Second)); action != quotaObservationWriteNow {
		t.Fatalf("change after the interval = %v, want write now", action)
	}
}

func TestQuotaObservationBookKeepsAccountsApart(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	book := &quotaObservationBook{}
	reset := t0.Add(time.Hour)

	if action, _ := book.offer("a", percentObservation(10, reset), t0); action != quotaObservationWriteNow {
		t.Fatalf("a = %v", action)
	}
	if action, _ := book.offer("b", percentObservation(10, reset), t0); action != quotaObservationWriteNow {
		t.Fatalf("b = %v: another account's write must not throttle this one", action)
	}
	if _, ok := book.take("never-seen", t0); ok {
		t.Fatal("take invented an observation for an unknown account")
	}
}

func TestQuotaObservationBookDefaultsApplyToTheZeroValue(t *testing.T) {
	var book quotaObservationBook
	minInterval, refresh := book.intervals()
	if minInterval != quotaObservationMinInterval || refresh != quotaObservationRefreshInterval {
		t.Fatalf("intervals = %v/%v, want the package defaults", minInterval, refresh)
	}
	if minInterval >= refresh {
		t.Fatal("the refresh interval must be longer than the coalescing interval")
	}
}

func TestQuotaObservationBookIsSafeUnderConcurrentOffers(t *testing.T) {
	book := &quotaObservationBook{minInterval: time.Millisecond, refresh: 10 * time.Millisecond}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				now := time.Now()
				book.offer("shared", percentObservation(float64(i%7), now.Add(time.Hour)), now)
				book.take("shared", now)
			}
		}(g)
	}
	wg.Wait()
}

// quotaRecordingHook records the observations the manager hands to the host.
type quotaRecordingHook struct {
	NoopHook
	calls chan quotaHookCall
}

type quotaHookCall struct {
	auth        *Auth
	observation cliproxyexecutor.QuotaObservation
	at          time.Time
}

func newQuotaRecordingHook() *quotaRecordingHook {
	return &quotaRecordingHook{calls: make(chan quotaHookCall, 32)}
}

func (h *quotaRecordingHook) OnQuotaObserved(_ context.Context, auth *Auth, observation cliproxyexecutor.QuotaObservation) {
	h.calls <- quotaHookCall{auth: auth, observation: observation, at: time.Now()}
}

func (h *quotaRecordingHook) next(t *testing.T) quotaHookCall {
	t.Helper()
	select {
	case call := <-h.calls:
		return call
	case <-time.After(3 * time.Second):
		t.Fatal("the hook was not called")
		return quotaHookCall{}
	}
}

func (h *quotaRecordingHook) expectSilence(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case call := <-h.calls:
		t.Fatalf("unexpected hook call: %+v", call.observation)
	case <-time.After(wait):
	}
}

func hookTestManager(t *testing.T, hook Hook) *Manager {
	t.Helper()
	m := NewManager(nil, nil, hook)
	m.quotaObservations.minInterval = 60 * time.Millisecond
	m.quotaObservations.refresh = time.Hour
	if _, err := m.Register(context.Background(), &Auth{ID: "acct-1", Provider: "codex", Status: StatusActive, TenantID: "tenant-a"}); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestQuotaObservationReachesTheHookOffTheRequestPath(t *testing.T) {
	hook := newQuotaRecordingHook()
	m := hookTestManager(t, hook)

	m.observeQuota("acct-1", "gpt-5.5", percentObservation(25, time.Now().Add(time.Hour)))
	call := hook.next(t)
	if call.auth == nil || call.auth.ID != "acct-1" || call.auth.TenantID != "tenant-a" {
		t.Fatalf("hook auth = %+v", call.auth)
	}
	if len(call.observation.Windows) != 1 || call.observation.Windows[0].UsedPercent != 25 {
		t.Fatalf("hook observation = %+v", call.observation)
	}
	if call.observation.ObservedAt.IsZero() {
		t.Fatal("the observation must carry the time the upstream answered")
	}

	// The hook gets a copy: whatever it does to the auth is not the scheduler's state.
	call.auth.Provider = "tampered"
	call.auth.ModelStates = map[string]*ModelState{"x": {}}
	got, _ := m.GetByID("acct-1")
	if got.Provider != "codex" || len(got.ModelStates) != 0 {
		t.Fatalf("the manager's auth was reachable from the hook: %+v", got)
	}
}

func TestQuotaObservationHookSeesAnExhaustedWindowToo(t *testing.T) {
	hook := newQuotaRecordingHook()
	m := hookTestManager(t, hook)

	m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(300, "5h", time.Now().Add(time.Hour))))
	call := hook.next(t)
	if call.observation.Windows[0].UsedPercent != 100 {
		t.Fatalf("hook observation = %+v", call.observation)
	}
	// The scheduler's own gate must already be in force by the time the console is told.
	got, _ := m.GetByID("acct-1")
	if !got.Quota.Exceeded {
		t.Fatal("the gate is not applied")
	}
}

func TestQuotaObservationHookCallsAreCoalescedPerAccount(t *testing.T) {
	hook := newQuotaRecordingHook()
	m := hookTestManager(t, hook)
	reset := time.Now().Add(time.Hour)

	m.observeQuota("acct-1", "gpt-5.5", percentObservation(10, reset))
	first := hook.next(t)

	// Three changes inside the interval: only the newest is written, once, late.
	m.observeQuota("acct-1", "gpt-5.5", percentObservation(11, reset))
	m.observeQuota("acct-1", "gpt-5.5", percentObservation(12, reset))
	m.observeQuota("acct-1", "gpt-5.5", percentObservation(13, reset))
	second := hook.next(t)
	if got := second.observation.Windows[0].UsedPercent; got != 13 {
		t.Fatalf("held write carried %v%%, want the newest (13%%)", got)
	}
	if gap := second.at.Sub(first.at); gap < 30*time.Millisecond {
		t.Fatalf("second write came %v after the first, want it held back for the interval", gap)
	}
	hook.expectSilence(t, 200*time.Millisecond)

	// Repeating what was written costs nothing.
	m.observeQuota("acct-1", "gpt-5.5", percentObservation(13, reset))
	hook.expectSilence(t, 150*time.Millisecond)
}

func TestQuotaObservationHookIsSkippedWhenTheAccountIsGone(t *testing.T) {
	hook := newQuotaRecordingHook()
	m := hookTestManager(t, hook)

	m.observeQuota("acct-1", "gpt-5.5", percentObservation(10, time.Now().Add(time.Hour)))
	hook.next(t)
	m.observeQuota("acct-1", "gpt-5.5", percentObservation(11, time.Now().Add(time.Hour)))
	// The held write finds no account to describe by the time its turn comes.
	if _, err := m.Delete(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	hook.expectSilence(t, 250*time.Millisecond)
}

func TestQuotaObservationWithAHookThatDoesNotListenIsHarmless(t *testing.T) {
	m := hookTestManager(t, NoopHook{})
	m.observeQuota("acct-1", "gpt-5.5", percentObservation(10, time.Now().Add(time.Hour)))
	time.Sleep(50 * time.Millisecond) // let the off-path write run; it must find nobody to call

	bare := NewManager(nil, nil, nil)
	if _, err := bare.Register(context.Background(), &Auth{ID: "acct-1", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	bare.observeQuota("acct-1", "gpt-5.5", percentObservation(10, time.Now().Add(time.Hour)))
	time.Sleep(50 * time.Millisecond)
}

func TestQuotaObservationWithoutARoutedModelStillReachesTheHook(t *testing.T) {
	hook := newQuotaRecordingHook()
	m := hookTestManager(t, hook)

	// No model means no scheduling state to set, but the console still wants the numbers.
	m.observeQuota("acct-1", "", observationOf(exhaustedWindow(300, "5h", time.Now().Add(time.Hour))))
	hook.next(t)
	got, _ := m.GetByID("acct-1")
	if got.Quota.Exceeded || len(got.ModelStates) != 0 {
		t.Fatalf("a gate was set without a model: %+v", got.ModelStates)
	}
}
