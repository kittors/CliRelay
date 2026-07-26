package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type quotaProbeExecutorStub struct {
	id    string
	probe func(context.Context, *Auth) (*QuotaProbeResult, error)
}

func (s *quotaProbeExecutorStub) Identifier() string {
	if s.id == "" {
		return "codex"
	}
	return s.id
}

func (s *quotaProbeExecutorStub) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (s *quotaProbeExecutorStub) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (s *quotaProbeExecutorStub) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }

func (s *quotaProbeExecutorStub) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (s *quotaProbeExecutorStub) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (s *quotaProbeExecutorStub) ProbeQuotaRecovery(ctx context.Context, auth *Auth) (*QuotaProbeResult, error) {
	return s.probe(ctx, auth)
}

func TestManagerReconcileQuota_ClearsRecoveredModelCooldown(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&quotaProbeExecutorStub{
		id: "codex",
		probe: func(context.Context, *Auth) (*QuotaProbeResult, error) {
			return &QuotaProbeResult{Recovered: true}, nil
		},
	})

	next := time.Now().Add(30 * time.Minute)
	auth := &Auth{
		ID:          "codex-auth",
		Provider:    "codex",
		Status:      StatusError,
		Unavailable: true,
		Quota: QuotaState{
			Exceeded:         true,
			RecoveryRequired: true,
			Reason:           "quota",
			NextRecoverAt:    next,
		},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: next,
				LastError:      &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
				Quota: QuotaState{
					Exceeded:         true,
					RecoveryRequired: true,
					Reason:           "quota",
					NextRecoverAt:    next,
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	changed, err := manager.ReconcileQuota(context.Background(), auth.ID)
	if err != nil {
		t.Fatalf("ReconcileQuota() error = %v", err)
	}
	if !changed {
		t.Fatalf("ReconcileQuota() changed = false, want true")
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("GetByID() missing auth")
	}
	state := updated.ModelStates["gpt-5-codex"]
	if state == nil {
		t.Fatalf("expected model state to exist")
	}
	if state.Unavailable {
		t.Fatalf("state.Unavailable = true, want false")
	}
	if state.Quota.Exceeded {
		t.Fatalf("state.Quota.Exceeded = true, want false")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("state.NextRetryAfter = %v, want zero", state.NextRetryAfter)
	}
	if updated.Unavailable {
		t.Fatalf("auth.Unavailable = true, want false")
	}
	if updated.Quota.Exceeded {
		t.Fatalf("auth.Quota.Exceeded = true, want false")
	}
	if updated.Status != StatusActive {
		t.Fatalf("auth.Status = %q, want %q", updated.Status, StatusActive)
	}
}

func TestApplyQuotaProbeResult_NotRecoveredWithoutResetKeepsWindowGate(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{
		ID:          "xai-auth",
		Provider:    "xai",
		Status:      StatusError,
		Unavailable: true,
		Quota: QuotaState{
			Exceeded:         true,
			RecoveryRequired: true,
			Reason:           "quota",
			Window:           "week",
			WindowMinutes:    10080,
			NextRecoverAt:    now.Add(-time.Hour),
		},
		NextRetryAfter: now.Add(-time.Hour),
		ModelStates: map[string]*ModelState{
			"grok-4.5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(-time.Hour),
				Quota: QuotaState{
					Exceeded:         true,
					RecoveryRequired: true,
					Reason:           "quota",
					Window:           "week",
					WindowMinutes:    10080,
					NextRecoverAt:    now.Add(-time.Hour),
				},
			},
		},
	}

	applyQuotaProbeResult(auth, &QuotaProbeResult{Recovered: false}, now)
	blocked, _, _ := isAuthBlockedForModel(auth, "grok-4.5", now)
	if !blocked {
		t.Fatal("not-recovered zero-reset probe released the credential")
	}
	if !authHasActiveQuotaCooldown(auth, now) {
		t.Fatal("window gate no longer schedules recovery probes")
	}
	if next := nextQuotaProbeTime(auth, now); !next.After(now) || next.After(now.Add(2*quotaProbeMinInterval)) {
		t.Fatalf("nextQuotaProbeTime() = %v, want prompt follow-up probe", next)
	}
}

func TestManagerReconcileQuota_UpdatesModelRecoverAt(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&quotaProbeExecutorStub{
		id: "gemini-cli",
		probe: func(context.Context, *Auth) (*QuotaProbeResult, error) {
			next := time.Now().Add(10 * time.Minute).Round(time.Second)
			return &QuotaProbeResult{
				Models: map[string]QuotaProbeModelResult{
					"gemini-2.5-pro": {
						Recovered:     false,
						NextRecoverAt: next,
					},
				},
			}, nil
		},
	})

	oldNext := time.Now().Add(2 * time.Hour)
	auth := &Auth{
		ID:       "gemini-auth",
		Provider: "gemini-cli",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			"gemini-2.5-pro(high)": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: oldNext,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: oldNext,
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	changed, err := manager.ReconcileQuota(context.Background(), auth.ID)
	if err != nil {
		t.Fatalf("ReconcileQuota() error = %v", err)
	}
	if !changed {
		t.Fatalf("ReconcileQuota() changed = false, want true")
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("GetByID() missing auth")
	}
	state := updated.ModelStates["gemini-2.5-pro(high)"]
	if state == nil {
		t.Fatalf("expected model state to exist")
	}
	if !state.Unavailable {
		t.Fatalf("state.Unavailable = false, want true")
	}
	if !state.Quota.Exceeded {
		t.Fatalf("state.Quota.Exceeded = false, want true")
	}
	if !state.NextRetryAfter.Equal(state.Quota.NextRecoverAt) {
		t.Fatalf("state.NextRetryAfter = %v, want %v", state.NextRetryAfter, state.Quota.NextRecoverAt)
	}
	if !state.NextRetryAfter.Before(oldNext) {
		t.Fatalf("state.NextRetryAfter = %v, want earlier than %v", state.NextRetryAfter, oldNext)
	}
}

func TestManagerClearQuotaStatus_ClearsAuthAndModelRuntimeQuota(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	next := time.Now().Add(30 * time.Minute)
	auth := &Auth{
		ID:             "codex-auth",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		LastError:      &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: next,
		},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: next,
				LastError:      &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	changed, err := manager.ClearQuotaStatus(context.Background(), auth.ID)
	if err != nil {
		t.Fatalf("ClearQuotaStatus() error = %v", err)
	}
	if !changed {
		t.Fatalf("ClearQuotaStatus() changed = false, want true")
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("GetByID() missing auth")
	}
	if updated.Status != StatusActive {
		t.Fatalf("auth.Status = %q, want %q", updated.Status, StatusActive)
	}
	if updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() || updated.LastError != nil || updated.Quota != (QuotaState{}) {
		t.Fatalf("auth quota runtime state was not cleared: %#v", updated)
	}
	state := updated.ModelStates["gpt-5-codex"]
	if state == nil {
		t.Fatalf("expected model state")
	}
	if state.Status != StatusActive {
		t.Fatalf("state.Status = %q, want %q", state.Status, StatusActive)
	}
	if state.StatusMessage != "" || state.Unavailable || !state.NextRetryAfter.IsZero() || state.LastError != nil || state.Quota != (QuotaState{}) {
		t.Fatalf("model quota runtime state was not cleared: %#v", state)
	}
}

func TestManagerClearQuotaStatus_PreservesStatusDisabled(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	next := time.Now().Add(30 * time.Minute)
	auth := &Auth{
		ID:             "disabled-auth",
		Provider:       "codex",
		Status:         StatusDisabled,
		StatusMessage:  "manually disabled",
		Disabled:       true,
		Unavailable:    true,
		NextRetryAfter: next,
		LastError:      &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: next,
		},
		ModelStates: map[string]*ModelState{
			"gpt-5-codex": {
				Status:         StatusDisabled,
				StatusMessage:  "model disabled",
				Unavailable:    true,
				NextRetryAfter: next,
				LastError:      &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	changed, err := manager.ClearQuotaStatus(context.Background(), auth.ID)
	if err != nil {
		t.Fatalf("ClearQuotaStatus() error = %v", err)
	}
	if !changed {
		t.Fatalf("ClearQuotaStatus() changed = false, want true")
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("GetByID() missing auth")
	}
	if !updated.Disabled || updated.Status != StatusDisabled || updated.StatusMessage != "manually disabled" {
		t.Fatalf("disabled auth status changed: disabled=%v status=%q message=%q", updated.Disabled, updated.Status, updated.StatusMessage)
	}
	if updated.Unavailable || !updated.NextRetryAfter.IsZero() || updated.LastError != nil || updated.Quota != (QuotaState{}) {
		t.Fatalf("disabled auth quota runtime state was not cleared: %#v", updated)
	}
	state := updated.ModelStates["gpt-5-codex"]
	if state == nil {
		t.Fatalf("expected model state")
	}
	if state.Status != StatusDisabled || state.StatusMessage != "model disabled" {
		t.Fatalf("disabled model status changed: status=%q message=%q", state.Status, state.StatusMessage)
	}
	if state.Unavailable || !state.NextRetryAfter.IsZero() || state.LastError != nil || state.Quota != (QuotaState{}) {
		t.Fatalf("disabled model quota runtime state was not cleared: %#v", state)
	}
}

// A week-exhausted credential whose recovery probe can never succeed (xAI
// API-key mode rejects the Grok Build billing probe) must still leave the gate
// once the real quota window elapses, instead of being blacklisted forever.
func TestWeekExhaustedGateExpiresWhenProbeNeverConfirms(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{ID: "xai-api-key", Provider: "xai", Status: StatusActive}
	applyAuthFailureState(auth, &Error{
		Message:            `{"error":"Grok Build usage balance exhausted"}`,
		HTTPStatus:         http.StatusPaymentRequired,
		QuotaWindow:        "week",
		QuotaWindowMinutes: 10080,
	}, nil, now)

	if blocked, _, _ := isAuthBlockedForModel(auth, "grok-4.5", now.Add(6*time.Hour)); !blocked {
		t.Fatal("blocked = false at +6h, want the gate to outlast the old short cooldown")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "grok-4.5", now.Add(8*24*time.Hour)); blocked {
		t.Fatal("blocked = true past the quota window, want the credential back in rotation")
	}
}

// disable_cooling is an explicit operator override and must also disable the
// probe-confirmed gate, otherwise it silently stops working for weekly quota.
func TestWeekExhaustedGateHonoursDisableCooling(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{
		ID:       "xai-no-cooling",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{"disable_cooling": true},
	}
	applyAuthFailureState(auth, &Error{
		Message:            `{"error":"Grok Build usage balance exhausted"}`,
		HTTPStatus:         http.StatusPaymentRequired,
		QuotaWindow:        "week",
		QuotaWindowMinutes: 10080,
	}, nil, now)

	if auth.Quota.RecoveryRequired {
		t.Fatal("RecoveryRequired = true, want disable_cooling to suppress the gate")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "grok-4.5", now.Add(time.Minute)); blocked {
		t.Fatal("blocked = true, want disable_cooling to keep the credential selectable")
	}
}
