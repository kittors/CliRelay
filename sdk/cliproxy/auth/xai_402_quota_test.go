package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type retryAfterQuotaErrorStub struct {
	message      string
	status       int
	quotaWindow  string
	quotaMinutes int
	retryAfter   time.Duration
}

func (e *retryAfterQuotaErrorStub) Error() string { return e.message }
func (e *retryAfterQuotaErrorStub) StatusCode() int {
	return e.status
}
func (e *retryAfterQuotaErrorStub) QuotaWindow() (string, int) {
	return e.quotaWindow, e.quotaMinutes
}
func (e *retryAfterQuotaErrorStub) RetryAfter() *time.Duration {
	if e.retryAfter <= 0 {
		return nil
	}
	d := e.retryAfter
	return &d
}

type xaiQuotaExecutor struct {
	err     error
	execute func(*Auth) (cliproxyexecutor.Response, error)
	probe   func(context.Context, *Auth) (*QuotaProbeResult, error)
}

func (e *xaiQuotaExecutor) Identifier() string { return "xai" }
func (e *xaiQuotaExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.execute != nil {
		return e.execute(auth)
	}
	return cliproxyexecutor.Response{}, e.err
}
func (e *xaiQuotaExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, e.err
}
func (e *xaiQuotaExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }
func (e *xaiQuotaExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.err
}
func (e *xaiQuotaExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, e.err
}
func (e *xaiQuotaExecutor) ProbeQuotaRecovery(ctx context.Context, auth *Auth) (*QuotaProbeResult, error) {
	if e.probe != nil {
		return e.probe(ctx, auth)
	}
	return &QuotaProbeResult{Recovered: false}, nil
}

func TestManagerMarkResult_XAI402BalanceExhaustedUsesExplicitRetryAfter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "grok-4.5"
	retryAfter := 47 * time.Hour
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		err: &retryAfterQuotaErrorStub{
			message:      `{"error":"Grok Build usage balance exhausted"}`,
			status:       http.StatusPaymentRequired,
			quotaWindow:  "week",
			quotaMinutes: 10080,
			retryAfter:   retryAfter,
		},
	})
	auth := &Auth{
		ID:       "xai-weekly",
		Provider: "xai",
		Status:   StatusActive,
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	before := time.Now()
	_, err := manager.Execute(ctx, []string{"xai"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 402")
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if !state.Quota.Exceeded || !state.Quota.RecoveryRequired || state.Quota.Reason != "quota" {
		t.Fatalf("quota = %#v, want exceeded quota", state.Quota)
	}
	if state.Quota.Window != "week" || state.Quota.WindowMinutes != 10080 {
		t.Fatalf("quota window = %q/%d, want week/10080", state.Quota.Window, state.Quota.WindowMinutes)
	}
	minExpected := before.Add(retryAfter - time.Minute)
	maxExpected := before.Add(retryAfter + time.Minute)
	if state.NextRetryAfter.Before(minExpected) || state.NextRetryAfter.After(maxExpected) {
		t.Fatalf("NextRetryAfter = %v, want ~%v", state.NextRetryAfter, before.Add(retryAfter))
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("NextRecoverAt = %v, want %v", state.Quota.NextRecoverAt, state.NextRetryAfter)
	}
}

func TestManagerMarkResult_XAI402BalanceExhaustedWithoutRetryUsesRecoveryGate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "grok-4.5"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		err: &retryAfterQuotaErrorStub{
			message:      `{"error":"Grok Build usage balance exhausted"}`,
			status:       http.StatusPaymentRequired,
			quotaWindow:  "week",
			quotaMinutes: 10080,
		},
	})
	auth := &Auth{
		ID:       "xai-weekly-no-reset",
		Provider: "xai",
		Status:   StatusActive,
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	before := time.Now()
	_, err := manager.Execute(ctx, []string{"xai"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 402")
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if !state.Quota.Exceeded || state.Quota.Window != "week" {
		t.Fatalf("quota = %#v, want week exceeded", state.Quota)
	}
	// Without an upstream reset the gate falls back to the declared window so a
	// credential whose recovery probe never succeeds still returns to the pool.
	wantDeadline := before.Add(10080 * time.Minute)
	if state.NextRetryAfter.Before(wantDeadline.Add(-time.Minute)) || state.NextRetryAfter.After(wantDeadline.Add(time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want ~%v (quota window fallback)", state.NextRetryAfter, wantDeadline)
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("NextRecoverAt = %v, want same as NextRetryAfter", state.Quota.NextRecoverAt)
	}
	if !got.Quota.Exceeded || !got.Quota.RecoveryRequired {
		t.Fatalf("aggregated auth quota = %#v, want confirmed-recovery gate", got.Quota)
	}
	blocked, _, _ := isAuthBlockedForModel(got, model, before.Add(7*time.Hour))
	if !blocked {
		t.Fatal("week-exhausted auth became selectable after the old 6h cooldown")
	}
}

func TestManagerExecute_XAIWeekExhaustedSkipsAuthForHealthyPeer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "grok-4.5"
	calls := map[string]int{}
	exhaustedErr := &retryAfterQuotaErrorStub{
		message:      `{"error":"Grok Build usage balance exhausted"}`,
		status:       http.StatusPaymentRequired,
		quotaWindow:  "week",
		quotaMinutes: 10080,
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		execute: func(auth *Auth) (cliproxyexecutor.Response, error) {
			calls[auth.ID]++
			if auth.ID == "xai-exhausted" {
				return cliproxyexecutor.Response{}, exhaustedErr
			}
			return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		},
	})
	for _, auth := range []*Auth{
		{ID: "xai-exhausted", Provider: "xai", Status: StatusActive, Attributes: map[string]string{"priority": "10"}},
		{ID: "xai-healthy", Provider: "xai", Status: StatusActive, Attributes: map[string]string{"priority": "1"}},
	} {
		if _, err := manager.Register(ctx, auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}

	request := cliproxyexecutor.Request{Model: model}
	if _, err := manager.Execute(ctx, []string{"xai"}, request, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if calls["xai-exhausted"] != 1 || calls["xai-healthy"] != 1 {
		t.Fatalf("first call counts = %#v, want exhausted=1 healthy=1", calls)
	}

	if _, err := manager.Execute(ctx, []string{"xai"}, request, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if calls["xai-exhausted"] != 1 || calls["xai-healthy"] != 2 {
		t.Fatalf("second call counts = %#v, want exhausted still 1 and healthy=2", calls)
	}

	exhausted, ok := manager.GetByID("xai-exhausted")
	if !ok || exhausted == nil {
		t.Fatal("exhausted auth missing")
	}
	now := time.Now()
	if !exhausted.Quota.Exceeded || !exhausted.Quota.RecoveryRequired {
		t.Fatalf("exhausted quota = %#v, want confirmed-recovery gate", exhausted.Quota)
	}
	if !authHasActiveQuotaCooldown(exhausted, now) {
		t.Fatal("authHasActiveQuotaCooldown() = false, want xAI gate eligible for recovery probes")
	}
	nextProbe := nextQuotaProbeTime(exhausted, now)
	if !nextProbe.After(now) || nextProbe.After(now.Add(2*quotaProbeMinInterval)) {
		t.Fatalf("nextQuotaProbeTime() = %v, want prompt follow-up probe", nextProbe)
	}
}

func TestManagerMarkResult_Generic402KeepsThirtyMinuteCooldown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "grok-4.5"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		err: &statusQuotaErrorStub{
			message: "payment required",
			status:  http.StatusPaymentRequired,
		},
	})
	auth := &Auth{
		ID:       "xai-generic-402",
		Provider: "xai",
		Status:   StatusActive,
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	before := time.Now()
	_, err := manager.Execute(ctx, []string{"xai"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 402")
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Quota.Exceeded {
		t.Fatalf("quota exceeded = true for generic 402, want payment_required path")
	}
	if state.NextRetryAfter.Before(before.Add(29*time.Minute)) || state.NextRetryAfter.After(before.Add(31*time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want ~30m payment_required", state.NextRetryAfter)
	}
}

func TestApplyAuthFailureState_XAI402BalanceExhausted(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	retry := 47 * time.Hour
	auth := &Auth{ID: "auth", Provider: "xai", Status: StatusActive}
	applyAuthFailureState(auth, &Error{
		Message:            `{"error":"Grok Build usage balance exhausted"}`,
		HTTPStatus:         http.StatusPaymentRequired,
		QuotaWindow:        "week",
		QuotaWindowMinutes: 10080,
	}, &retry, now)

	if !auth.Quota.Exceeded || auth.Quota.Window != "week" || auth.Quota.WindowMinutes != 10080 {
		t.Fatalf("quota = %#v, want week exceeded", auth.Quota)
	}
	if !auth.NextRetryAfter.Equal(now.Add(retry)) {
		t.Fatalf("NextRetryAfter = %v, want %v", auth.NextRetryAfter, now.Add(retry))
	}
	if auth.StatusMessage == "payment_required" {
		t.Fatal("StatusMessage still payment_required, want balance exhausted / quota path")
	}
}

func TestApplyAuthFailureState_XAI402WithoutRetryUsesRecoveryGate(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	auth := &Auth{ID: "auth", Provider: "xai", Status: StatusActive}
	applyAuthFailureState(auth, &Error{
		Message:            `{"error":"Grok Build usage balance exhausted"}`,
		HTTPStatus:         http.StatusPaymentRequired,
		QuotaWindow:        "week",
		QuotaWindowMinutes: 10080,
	}, nil, now)

	if !auth.Quota.Exceeded || !auth.Quota.RecoveryRequired || auth.Quota.Window != "week" {
		t.Fatalf("quota = %#v, want week exceeded", auth.Quota)
	}
	wantDeadline := now.Add(10080 * time.Minute)
	if !auth.NextRetryAfter.Equal(wantDeadline) {
		t.Fatalf("NextRetryAfter = %v, want %v (quota window fallback)", auth.NextRetryAfter, wantDeadline)
	}
	if !auth.Quota.NextRecoverAt.Equal(wantDeadline) {
		t.Fatalf("NextRecoverAt = %v, want %v (quota window fallback)", auth.Quota.NextRecoverAt, wantDeadline)
	}
}

func TestManagerMarkResult_XAIWeekExhaustedProbesImmediately(t *testing.T) {
	t.Parallel()

	probed := make(chan struct{}, 1)
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		err: &retryAfterQuotaErrorStub{
			message:      `{"error":"Grok Build usage balance exhausted"}`,
			status:       http.StatusPaymentRequired,
			quotaWindow:  "week",
			quotaMinutes: 10080,
		},
		probe: func(context.Context, *Auth) (*QuotaProbeResult, error) {
			probed <- struct{}{}
			return &QuotaProbeResult{Recovered: false}, nil
		},
	})
	if _, err := manager.Register(context.Background(), &Auth{ID: "xai-probe", Provider: "xai", Status: StatusActive}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, _ = manager.Execute(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: "grok-4.5"}, cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.SinglePickMetadataKey: true},
	})

	select {
	case <-probed:
	case <-time.After(2 * time.Second):
		t.Fatal("billing recovery probe was not started after week-exhausted 402")
	}
}

func TestManagerMarkResult_XAIWeekExhaustedProbePinsBillingReset(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(3 * time.Hour).Round(time.Second)
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		err: &retryAfterQuotaErrorStub{
			message:      `{"error":"Grok Build usage balance exhausted"}`,
			status:       http.StatusPaymentRequired,
			quotaWindow:  "week",
			quotaMinutes: 10080,
		},
		probe: func(context.Context, *Auth) (*QuotaProbeResult, error) {
			return &QuotaProbeResult{
				Recovered:       false,
				WindowExhausted: true,
				Window:          "week",
				WindowMinutes:   10080,
				NextRecoverAt:   resetAt,
			}, nil
		},
	})
	auth := &Auth{ID: "xai-reset", Provider: "xai", Status: StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, _ = manager.Execute(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: "grok-4.5"}, cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.SinglePickMetadataKey: true},
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := manager.GetByID(auth.ID)
		if got != nil {
			if state := got.ModelStates["grok-4.5"]; state != nil && state.Quota.NextRecoverAt.Equal(resetAt) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := manager.GetByID(auth.ID)
	t.Fatalf("billing reset was not applied: %#v", got)
}

func TestManagerMarkResult_SuccessClearsWindowExhaustedGate(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "xai-success", Provider: "xai", Status: StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "grok-4.5",
		Error: &Error{
			Message:            `{"error":"Grok Build usage balance exhausted"}`,
			HTTPStatus:         http.StatusPaymentRequired,
			QuotaWindow:        "week",
			QuotaWindowMinutes: 10080,
		},
	})
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "grok-4.5",
		Success:  true,
	})

	got, _ := manager.GetByID(auth.ID)
	if got.Quota.Exceeded || got.Quota.RecoveryRequired {
		t.Fatalf("quota = %#v, want success-confirmed recovery", got.Quota)
	}
	if state := got.ModelStates["grok-4.5"]; state == nil || state.Quota.Exceeded || state.Unavailable {
		t.Fatalf("model state = %#v, want recovered", state)
	}
}
