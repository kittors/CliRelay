package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func exhaustedWindow(minutes int, label string, reset time.Time) cliproxyexecutor.QuotaWindowObservation {
	return cliproxyexecutor.QuotaWindowObservation{Label: label, UsedPercent: 100, WindowMinutes: minutes, ResetAt: reset}
}

func usedWindow(used float64, minutes int, label string, reset time.Time) cliproxyexecutor.QuotaWindowObservation {
	return cliproxyexecutor.QuotaWindowObservation{Label: label, UsedPercent: used, WindowMinutes: minutes, ResetAt: reset}
}

func observationOf(windows ...cliproxyexecutor.QuotaWindowObservation) cliproxyexecutor.QuotaObservation {
	return cliproxyexecutor.QuotaObservation{ObservedAt: time.Now(), Windows: windows}
}

func TestObservedQuotaGateFor(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	hours := func(h float64) time.Time { return now.Add(time.Duration(h * float64(time.Hour))) }
	days := func(d float64) time.Time { return hours(24 * d) }

	cases := []struct {
		name        string
		observation cliproxyexecutor.QuotaObservation
		wantGate    bool
		wantUntil   time.Time
		wantWindow  string
		wantMinutes int
	}{
		{
			name:        "usage below the limit never gates",
			observation: observationOf(usedWindow(99.99, 300, "5h", hours(1))),
		},
		{
			name:        "a full window gates until it resets",
			observation: observationOf(exhaustedWindow(300, "5h", hours(2))),
			wantGate:    true, wantUntil: hours(2), wantWindow: "5h", wantMinutes: 300,
		},
		{
			name:        "usage past 100 is the same as 100",
			observation: observationOf(usedWindow(104.2, 300, "5h", hours(2))),
			wantGate:    true, wantUntil: hours(2), wantWindow: "5h", wantMinutes: 300,
		},
		{
			name: "with both windows full the later reset decides",
			observation: observationOf(
				exhaustedWindow(300, "5h", hours(2)),
				exhaustedWindow(10080, "week", days(3)),
			),
			wantGate: true, wantUntil: days(3), wantWindow: "week", wantMinutes: 10080,
		},
		{
			name: "the later reset decides regardless of the order windows arrive in",
			observation: observationOf(
				exhaustedWindow(10080, "week", days(3)),
				exhaustedWindow(300, "5h", hours(2)),
			),
			wantGate: true, wantUntil: days(3), wantWindow: "week", wantMinutes: 10080,
		},
		{
			name: "windows with room left do not count",
			observation: observationOf(
				usedWindow(40, 300, "5h", hours(1)),
				exhaustedWindow(10080, "week", days(3)),
			),
			wantGate: true, wantUntil: days(3), wantWindow: "week", wantMinutes: 10080,
		},
		{
			name:        "a full window with no reset time declines instead of guessing",
			observation: observationOf(exhaustedWindow(300, "5h", time.Time{})),
		},
		{
			name: "one full window with an unknown reset declines even if another is known",
			observation: observationOf(
				exhaustedWindow(300, "5h", time.Time{}),
				exhaustedWindow(10080, "week", days(3)),
			),
		},
		{
			name:        "a reset already in the past is a stale percentage, not a block",
			observation: observationOf(exhaustedWindow(300, "5h", now.Add(-time.Minute))),
		},
		{
			name:        "a reset exactly now has already happened",
			observation: observationOf(exhaustedWindow(300, "5h", now)),
		},
		{
			name: "a stale full window does not hide a live one",
			observation: observationOf(
				exhaustedWindow(300, "5h", now.Add(-time.Hour)),
				exhaustedWindow(10080, "week", days(2)),
			),
			wantGate: true, wantUntil: days(2), wantWindow: "week", wantMinutes: 10080,
		},
		{
			name:        "a reset further out than any allowance runs is a malformed answer",
			observation: observationOf(exhaustedWindow(300, "5h", days(36))),
		},
		{
			name:        "a reset at the edge of the horizon is still believed",
			observation: observationOf(exhaustedWindow(10080, "week", now.Add(observedQuotaMaxHorizon))),
			wantGate:    true, wantUntil: now.Add(observedQuotaMaxHorizon), wantWindow: "week", wantMinutes: 10080,
		},
		{
			name: "prepaid credits keep an account serving past its windows",
			observation: cliproxyexecutor.QuotaObservation{
				Windows:          []cliproxyexecutor.QuotaWindowObservation{exhaustedWindow(300, "5h", hours(2))},
				OverageAvailable: true,
			},
		},
		{
			name:        "an observation without windows says nothing",
			observation: cliproxyexecutor.QuotaObservation{},
		},
		{
			name:        "a window without a label still gates",
			observation: observationOf(exhaustedWindow(0, "", hours(2))),
			wantGate:    true, wantUntil: hours(2),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate, ok := observedQuotaGateFor(tc.observation, now)
			if ok != tc.wantGate {
				t.Fatalf("gate = %v (%+v), want %v", ok, gate, tc.wantGate)
			}
			if !ok {
				return
			}
			if !gate.until.Equal(tc.wantUntil) || gate.window != tc.wantWindow || gate.windowMinutes != tc.wantMinutes {
				t.Fatalf("gate = %+v, want until=%v window=%q minutes=%d", gate, tc.wantUntil, tc.wantWindow, tc.wantMinutes)
			}
		})
	}
}

func TestObservedExhaustionParksTheAccountForEveryModel(t *testing.T) {
	m, registry := cooldownTestManager(t, "acct-1", "acct-2")
	notices := collectNotices(m)
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(300, "5h", until)))

	auth, _ := m.GetByID("acct-1")
	// The auth-level block is what keeps a client from burning requests by
	// switching models inside the same exhausted window.
	for _, model := range []string{"gpt-5.5", "gpt-5.4-mini", ""} {
		blocked, reason, next := isAuthBlockedForModel(auth, model, time.Now())
		if !blocked || reason != blockReasonCooldown || !next.Equal(until) {
			t.Fatalf("model %q: blocked=%v reason=%v next=%v, want a cooldown until %v", model, blocked, reason, next, until)
		}
	}
	state := auth.ModelStates["gpt-5.5"]
	if state == nil {
		t.Fatal("no model state recorded")
	}
	if !state.Unavailable || state.Status != StatusError || !state.NextRetryAfter.Equal(until) {
		t.Fatalf("state = %+v, want unavailable/error until %v", state, until)
	}
	if !state.Quota.Exceeded || state.Quota.Reason != "quota" || state.Quota.Window != "5h" ||
		state.Quota.WindowMinutes != 300 || !state.Quota.NextRecoverAt.Equal(until) {
		t.Fatalf("quota = %+v", state.Quota)
	}
	if state.Quota.RecoveryRequired {
		t.Fatal("an observed window must not arm the probe-confirmed gate: it has the announced reset to go by")
	}
	if state.LastError == nil || state.LastError.HTTPStatus != http.StatusTooManyRequests ||
		state.LastError.Code != observedQuotaExhaustedCode || !state.LastError.Retryable {
		t.Fatalf("last error = %+v", state.LastError)
	}
	if !auth.Quota.Exceeded || !auth.Unavailable {
		t.Fatalf("the auth-level rollup must follow: quota=%+v unavailable=%v", auth.Quota, auth.Unavailable)
	}

	other, _ := m.GetByID("acct-2")
	if blocked, _, _ := isAuthBlockedForModel(other, "gpt-5.5", time.Now()); blocked {
		t.Fatal("another account must keep serving")
	}
	if len(other.ModelStates) != 0 {
		t.Fatalf("another account's state was touched: %+v", other.ModelStates)
	}

	if got := registry.quota; len(got) != 1 || got[0] != "acct-1/gpt-5.5" {
		t.Fatalf("registry quota marks = %v", got)
	}
	if got := registry.suspended; len(got) != 1 || got[0] != "acct-1/gpt-5.5/quota" {
		t.Fatalf("registry suspensions = %v", got)
	}
	if len(*notices) != 1 {
		t.Fatalf("notices = %+v, want one", *notices)
	}
	if n := (*notices)[0]; n.AuthID != "acct-1" || n.Model != "gpt-5.5" || !n.Until.Equal(until) ||
		!n.Quota || n.Reason != "quota" || n.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("notice = %+v", n)
	}
}

func TestObservedExhaustionIsReportedOnlyOnce(t *testing.T) {
	m, registry := cooldownTestManager(t, "acct-1")
	notices := collectNotices(m)
	until := time.Now().Add(time.Hour)

	// Every answer of an exhausted account repeats the same numbers.
	for i := 0; i < 5; i++ {
		m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(300, "5h", until)))
	}
	if len(registry.quota) != 1 || len(registry.suspended) != 1 || len(*notices) != 1 {
		t.Fatalf("registry quota=%v suspended=%v notices=%d, want one each", registry.quota, registry.suspended, len(*notices))
	}
}

func TestObservedUsageBelowTheLimitChangesNothing(t *testing.T) {
	m, registry := cooldownTestManager(t, "acct-1")
	notices := collectNotices(m)

	for _, used := range []float64{0, 1, 50, 99, 99.9} {
		m.observeQuota("acct-1", "gpt-5.5", observationOf(usedWindow(used, 300, "5h", time.Now().Add(time.Hour))))
	}

	auth, _ := m.GetByID("acct-1")
	if len(auth.ModelStates) != 0 || auth.Quota.Exceeded || auth.Unavailable || auth.Status != StatusActive {
		t.Fatalf("usage below 100%% must not touch scheduling: states=%+v quota=%+v unavailable=%v status=%v",
			auth.ModelStates, auth.Quota, auth.Unavailable, auth.Status)
	}
	if len(registry.quota)+len(registry.suspended)+len(*notices) != 0 {
		t.Fatalf("registry quota=%v suspended=%v notices=%v", registry.quota, registry.suspended, *notices)
	}
}

func TestObservedUsageDoesNotLiftExistingBackoff(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	backoffUntil := time.Now().Add(time.Minute)
	m.mu.Lock()
	auth := m.auths["acct-1"]
	state := ensureModelState(auth, "gpt-5.5")
	state.Unavailable, state.Status, state.NextRetryAfter = true, StatusError, backoffUntil
	updateAggregatedAvailability(auth, time.Now())
	m.mu.Unlock()

	m.observeQuota("acct-1", "gpt-5.5", observationOf(usedWindow(10, 300, "5h", time.Now().Add(time.Hour))))

	got, _ := m.GetByID("acct-1")
	if s := got.ModelStates["gpt-5.5"]; s == nil || !s.Unavailable || !s.NextRetryAfter.Equal(backoffUntil) {
		t.Fatalf("a reading that says the account has room left is information, not a verdict on someone else's backoff: %+v", s)
	}
}

func TestObservedExhaustionIsIgnoredWhereItCannotApply(t *testing.T) {
	until := time.Now().Add(time.Hour)
	exhausted := observationOf(exhaustedWindow(300, "5h", until))

	t.Run("unknown account", func(t *testing.T) {
		m, registry := cooldownTestManager(t, "acct-1")
		m.observeQuota("nobody", "gpt-5.5", exhausted)
		if len(registry.quota)+len(registry.suspended) != 0 {
			t.Fatalf("registry touched for an unknown account: %v %v", registry.quota, registry.suspended)
		}
	})
	t.Run("disabled account", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		if _, err := m.Register(context.Background(), &Auth{ID: "off", Provider: "codex", Disabled: true, Status: StatusDisabled}); err != nil {
			t.Fatal(err)
		}
		m.observeQuota("off", "gpt-5.5", exhausted)
		auth, _ := m.GetByID("off")
		if len(auth.ModelStates) != 0 || auth.Status != StatusDisabled {
			t.Fatalf("a disabled account must stay as it is: %+v status=%v", auth.ModelStates, auth.Status)
		}
	})
	t.Run("disabled model", func(t *testing.T) {
		m, registry := cooldownTestManager(t, "acct-1")
		m.mu.Lock()
		ensureModelState(m.auths["acct-1"], "gpt-5.5").Status = StatusDisabled
		m.mu.Unlock()
		m.observeQuota("acct-1", "gpt-5.5", exhausted)
		auth, _ := m.GetByID("acct-1")
		if state := auth.ModelStates["gpt-5.5"]; state.Status != StatusDisabled || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() {
			t.Fatalf("a model the operator disabled must stay disabled: %+v", state)
		}
		if len(registry.quota)+len(registry.suspended) != 0 {
			t.Fatalf("registry touched: %v %v", registry.quota, registry.suspended)
		}
	})
	t.Run("no routed model", func(t *testing.T) {
		m, _ := cooldownTestManager(t, "acct-1")
		m.observeQuota("acct-1", "", exhausted)
		auth, _ := m.GetByID("acct-1")
		if len(auth.ModelStates) != 0 || auth.Quota.Exceeded {
			t.Fatalf("without a model there is no state to key the gate under: %+v", auth.ModelStates)
		}
	})
	t.Run("empty observation", func(t *testing.T) {
		m, _ := cooldownTestManager(t, "acct-1")
		m.observeQuota("acct-1", "gpt-5.5", cliproxyexecutor.QuotaObservation{})
		auth, _ := m.GetByID("acct-1")
		if len(auth.ModelStates) != 0 {
			t.Fatalf("states = %+v", auth.ModelStates)
		}
	})
	t.Run("nil manager", func(t *testing.T) {
		var m *Manager
		m.observeQuota("acct-1", "gpt-5.5", exhausted) // must not panic
		if m.quotaObserverFor("acct-1", "gpt-5.5") != nil {
			t.Fatal("a nil manager must not hand out an observer")
		}
	})
	t.Run("blank account id gets no observer", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		if m.quotaObserverFor("  ", "gpt-5.5") != nil {
			t.Fatal("an attempt that has no account has nothing to observe for")
		}
	})
}

// A gate the recovery prober armed lifts only on the prober's word and tracks its
// deadline; a window read off a request must not move it, in either direction.
func TestObservedExhaustionNeverOverridesAProbeConfirmedGate(t *testing.T) {
	m, registry := cooldownTestManager(t, "acct-1")
	notices := collectNotices(m)
	armedUntil := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	m.mu.Lock()
	auth := m.auths["acct-1"]
	state := ensureModelState(auth, "gpt-5.5")
	state.Unavailable, state.Status, state.NextRetryAfter = true, StatusError, armedUntil
	state.Quota = QuotaState{Exceeded: true, Reason: "quota", RecoveryRequired: true, NextRecoverAt: armedUntil}
	updateAggregatedAvailability(auth, time.Now())
	m.mu.Unlock()

	m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(10080, "week", time.Now().Add(72*time.Hour))))

	got, _ := m.GetByID("acct-1")
	s := got.ModelStates["gpt-5.5"]
	if !s.Quota.RecoveryRequired || !s.Quota.NextRecoverAt.Equal(armedUntil) || !s.NextRetryAfter.Equal(armedUntil) {
		t.Fatalf("armed gate was changed: %+v retry=%v", s.Quota, s.NextRetryAfter)
	}
	if len(registry.quota)+len(registry.suspended)+len(*notices) != 0 {
		t.Fatalf("nothing changed, so nothing is announced: %v %v %v", registry.quota, registry.suspended, *notices)
	}
}

func TestObservedExhaustionOnlyExtendsAnExistingCooldown(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	existing := time.Now().Add(10 * time.Hour).Truncate(time.Second)
	m.mu.Lock()
	auth := m.auths["acct-1"]
	state := ensureModelState(auth, "gpt-5.5")
	state.Unavailable, state.Status, state.NextRetryAfter = true, StatusError, existing
	state.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: existing, BackoffLevel: 2}
	updateAggregatedAvailability(auth, time.Now())
	m.mu.Unlock()

	// A shorter window than the one already in force changes nothing.
	m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(300, "5h", time.Now().Add(2*time.Hour))))
	got, _ := m.GetByID("acct-1")
	if s := got.ModelStates["gpt-5.5"]; !s.NextRetryAfter.Equal(existing) || !s.Quota.NextRecoverAt.Equal(existing) {
		t.Fatalf("a shorter window shortened the cooldown: %+v retry=%v", s.Quota, s.NextRetryAfter)
	}

	// A longer one extends it, and keeps the backoff history the refusals built up.
	longer := time.Now().Add(20 * time.Hour).Truncate(time.Second)
	m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(10080, "week", longer)))
	got, _ = m.GetByID("acct-1")
	s := got.ModelStates["gpt-5.5"]
	if !s.NextRetryAfter.Equal(longer) || !s.Quota.NextRecoverAt.Equal(longer) || s.Quota.Window != "week" {
		t.Fatalf("a longer window must extend the cooldown: %+v retry=%v", s.Quota, s.NextRetryAfter)
	}
	if s.Quota.BackoffLevel != 2 {
		t.Fatalf("backoff level = %d, want the existing 2 preserved", s.Quota.BackoffLevel)
	}
}

func TestSuccessDoesNotLiftAnObservedGate(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	// The request that carried the observation is itself a success, and finishes
	// after the gate is set.
	m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(300, "5h", until)))
	m.MarkResult(context.Background(), Result{AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5", Success: true})

	auth, _ := m.GetByID("acct-1")
	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5.5", time.Now())
	if !blocked || reason != blockReasonCooldown || !next.Equal(until) {
		t.Fatalf("the finished request lifted the gate: blocked=%v reason=%v next=%v", blocked, reason, next)
	}
}

func TestObservedGateEndsWhenTheWindowResets(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(300, "5h", until)))

	auth, _ := m.GetByID("acct-1")
	if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5.5", until.Add(time.Second)); blocked {
		t.Fatal("the account must be selectable again once the window has reset, with no probe needed")
	}
}

// The upstream answers 429 on the very request whose headers say the window is
// full. The refusal arrives after the gate and must leave one coherent cooldown.
func TestObservedGateFollowedByTheRefusalItPredicted(t *testing.T) {
	m, _ := cooldownTestManager(t, "acct-1")
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	m.observeQuota("acct-1", "gpt-5.5", observationOf(exhaustedWindow(300, "5h", until)))

	retry := time.Until(until)
	m.MarkResult(context.Background(), Result{
		AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5", RetryAfter: &retry,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Code: "usage_limit_reached", Message: "usage limit reached",
			QuotaWindow: "5h", QuotaWindowMinutes: 300},
	})

	auth, _ := m.GetByID("acct-1")
	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5.5", time.Now())
	if !blocked || reason != blockReasonCooldown || next.Before(until.Add(-2*time.Second)) {
		t.Fatalf("blocked=%v reason=%v next=%v, want a cooldown to at least %v", blocked, reason, next, until)
	}
	if s := auth.ModelStates["gpt-5.5"]; !s.Quota.Exceeded || s.LastError == nil || s.LastError.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("state = %+v", s)
	}
}

// quotaObservingExecutor is a Codex executor that reports a quota observation the
// way the real one does: through the context the scheduler attached.
type quotaObservingExecutor struct {
	mu       sync.Mutex
	observe  map[string]cliproxyexecutor.QuotaObservation
	served   []string
	attached []bool
}

func (e *quotaObservingExecutor) Identifier() string { return "codex" }

func (e *quotaObservingExecutor) report(ctx context.Context, auth *Auth) {
	e.mu.Lock()
	e.served = append(e.served, auth.ID)
	e.attached = append(e.attached, cliproxyexecutor.HasQuotaObserver(ctx))
	observation, ok := e.observe[auth.ID]
	e.mu.Unlock()
	if ok {
		cliproxyexecutor.ObserveQuota(ctx, observation)
	}
}

func (e *quotaObservingExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.report(ctx, auth)
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *quotaObservingExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.report(ctx, auth)
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\"}\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *quotaObservingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *quotaObservingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *quotaObservingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *quotaObservingExecutor) servedBy() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.served...)
}

func quotaPoolManager(t *testing.T, executor *quotaObservingExecutor, hook Hook) *Manager {
	t.Helper()
	manager := NewManager(nil, &FillFirstSelector{}, hook)
	manager.RegisterExecutor(executor)
	for _, id := range []string{"acct-a", "acct-b"} {
		if _, err := manager.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	return manager
}

// The point of the whole feature, end to end through the real scheduler: the
// request that learns an account's window is full is still served, and no later
// request is sent to that account until the window resets.
func TestExecuteParksAnAccountTheMomentItsWindowIsFull(t *testing.T) {
	until := time.Now().Add(2 * time.Hour)
	executor := &quotaObservingExecutor{observe: map[string]cliproxyexecutor.QuotaObservation{
		"acct-a": observationOf(exhaustedWindow(300, "5h", until)),
	}}
	manager := quotaPoolManager(t, executor, nil)
	req := cliproxyexecutor.Request{Model: "gpt-5.5"}

	for i := 0; i < 4; i++ {
		resp, err := manager.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
		if err != nil || string(resp.Payload) != "ok" {
			t.Fatalf("request %d: resp=%q err=%v", i, resp.Payload, err)
		}
	}

	served := executor.servedBy()
	want := []string{"acct-a", "acct-b", "acct-b", "acct-b"}
	if len(served) != len(want) {
		t.Fatalf("served by %v, want %v", served, want)
	}
	for i := range want {
		if served[i] != want[i] {
			t.Fatalf("served by %v, want %v: acct-a took a request after its window was known to be full", served, want)
		}
	}
	for i, attached := range executor.attached {
		if !attached {
			t.Fatalf("request %d reached the executor with no quota observer attached", i)
		}
	}
}

func TestExecuteStreamParksAnAccountTheMomentItsWindowIsFull(t *testing.T) {
	until := time.Now().Add(2 * time.Hour)
	executor := &quotaObservingExecutor{observe: map[string]cliproxyexecutor.QuotaObservation{
		"acct-a": observationOf(exhaustedWindow(300, "5h", until)),
	}}
	manager := quotaPoolManager(t, executor, nil)

	for i := 0; i < 3; i++ {
		stream, err := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.5"}, cliproxyexecutor.Options{})
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream %d chunk error: %v", i, chunk.Err)
			}
		}
	}
	if served := executor.servedBy(); len(served) != 3 || served[0] != "acct-a" || served[1] != "acct-b" || served[2] != "acct-b" {
		t.Fatalf("served by %v, want acct-a once and then only acct-b", served)
	}
}

// With no account left to fail over to, the caller gets the scheduler's cooldown
// answer rather than a request the upstream is certain to refuse.
func TestExecuteWithEveryAccountFullAnswersWithTheCooldown(t *testing.T) {
	until := time.Now().Add(2 * time.Hour)
	full := observationOf(exhaustedWindow(300, "5h", until))
	executor := &quotaObservingExecutor{observe: map[string]cliproxyexecutor.QuotaObservation{"acct-a": full, "acct-b": full}}
	manager := quotaPoolManager(t, executor, nil)
	req := cliproxyexecutor.Request{Model: "gpt-5.5"}

	for i := 0; i < 2; i++ {
		if _, err := manager.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	_, err := manager.Execute(context.Background(), []string{"codex"}, req, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("every account's window is full: the third request has nowhere to go")
	}
	if served := executor.servedBy(); len(served) != 2 {
		t.Fatalf("served by %v: the exhausted accounts were tried again", served)
	}
}

// Below 100% the scheduler must behave exactly as it did before observation
// existed: usage is information, not a routing weight. The baseline is the same
// pool with an executor that reports nothing, so the comparison does not depend
// on how the selector spreads load.
func TestExecuteWithUsageBelowTheLimitKeepsRouting(t *testing.T) {
	soon := time.Now().Add(time.Hour)
	served := func(observe map[string]cliproxyexecutor.QuotaObservation) []string {
		executor := &quotaObservingExecutor{observe: observe}
		manager := quotaPoolManager(t, executor, nil)
		for i := 0; i < 8; i++ {
			if _, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.5"}, cliproxyexecutor.Options{}); err != nil {
				t.Fatal(err)
			}
		}
		return executor.servedBy()
	}

	baseline := served(nil)
	nearlyFull := served(map[string]cliproxyexecutor.QuotaObservation{
		"acct-a": observationOf(usedWindow(99, 300, "5h", soon), usedWindow(97, 10080, "week", soon)),
		"acct-b": observationOf(usedWindow(99.9, 300, "5h", soon)),
	})
	if len(baseline) != 8 || len(nearlyFull) != 8 {
		t.Fatalf("baseline=%v nearlyFull=%v", baseline, nearlyFull)
	}
	for i := range baseline {
		if baseline[i] != nearlyFull[i] {
			t.Fatalf("usage at 99%% changed routing:\n baseline   %v\n nearlyFull %v", baseline, nearlyFull)
		}
	}
}
