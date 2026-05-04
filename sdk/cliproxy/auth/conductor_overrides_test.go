package auth

import (
	"context"
	"testing"
	"time"
)

func TestManager_ShouldRetryAfterError_RespectsAuthRequestRetryOverride(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"request_retry": float64(0),
		},
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait, nil)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for request_retry=0, got true (wait=%v)", wait)
	}

	auth.Metadata["request_retry"] = float64(1)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	wait, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait, nil)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true for request_retry=1, got false")
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}

	_, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 1, []string{"claude"}, model, maxWait, nil)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false on attempt=1 for request_retry=1, got true")
	}
}

func TestManager_ShouldRetryAfterError_DisablesInternalRetryForGroupedRoute(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, maxWait := m.retrySettings()
	meta := map[string]any{"route_group": "pro"}
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait, meta)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for grouped route, got true (wait=%v)", wait)
	}
	if wait != 0 {
		t.Fatalf("expected wait=0 for grouped route, got %v", wait)
	}
}

func TestManager_ShouldRetryAfterError_DisablesInternalRetryForSinglePick(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, maxWait := m.retrySettings()
	meta := map[string]any{"single_pick": true}
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"codex"}, model, maxWait, meta)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for single-pick request, got true (wait=%v)", wait)
	}
	if wait != 0 {
		t.Fatalf("expected wait=0 for single-pick request, got %v", wait)
	}
}

func TestManager_ShouldRetryAfterError_DisablesInternalRetryForCooldownWaitDisabled(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, maxWait := m.retrySettings()
	meta := map[string]any{"disable_cooldown_wait": true}
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"codex"}, model, maxWait, meta)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false when cooldown wait is disabled, got true (wait=%v)", wait)
	}
	if wait != 0 {
		t.Fatalf("expected wait=0 when cooldown wait is disabled, got %v", wait)
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model"
	m.MarkResult(context.Background(), Result{
		AuthID:   "auth-1",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 500, Message: "boom"},
	})

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}
