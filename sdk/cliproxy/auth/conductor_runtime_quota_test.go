package auth

import (
	"context"
	"testing"
	"time"
)

func TestManagerUpdatePreservesRuntimeQuotaState(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	model := "gpt-5.5"

	if _, err := m.Register(ctx, &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token": "old-token",
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	retryAfter := 2 * time.Hour
	m.MarkResult(ctx, Result{
		AuthID:     "auth-1",
		Provider:   "codex",
		Model:      model,
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &Error{
			Code:               "usage_limit_reached",
			Message:            "usage limit reached",
			HTTPStatus:         429,
			QuotaWindow:        "5h",
			QuotaWindowMinutes: 300,
		},
	})

	before, ok := m.GetByID("auth-1")
	if !ok {
		t.Fatalf("expected auth to be present")
	}
	beforeState := before.ModelStates[model]
	if beforeState == nil || !beforeState.Quota.Exceeded {
		t.Fatalf("test setup failed: expected model quota to be exceeded")
	}

	updated, err := m.Update(ctx, &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token": "new-token",
		},
	})
	if err != nil {
		t.Fatalf("update auth: %v", err)
	}

	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected runtime model state to survive update")
	}
	if !state.Quota.Exceeded {
		t.Fatalf("state.Quota.Exceeded = false, want true")
	}
	if !state.NextRetryAfter.Equal(beforeState.NextRetryAfter) {
		t.Fatalf("state.NextRetryAfter = %v, want %v", state.NextRetryAfter, beforeState.NextRetryAfter)
	}
	if !updated.Quota.Exceeded {
		t.Fatalf("auth.Quota.Exceeded = false, want true")
	}
	if !updated.Unavailable {
		t.Fatalf("auth.Unavailable = false, want true")
	}
}

func TestManagerUpdateDoesNotPreserveRuntimeQuotaWhenDisabled(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	model := "gpt-5.5"

	if _, err := m.Register(ctx, &Auth{ID: "auth-1", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	retryAfter := 2 * time.Hour
	m.MarkResult(ctx, Result{
		AuthID:     "auth-1",
		Provider:   "codex",
		Model:      model,
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &Error{Code: "usage_limit_reached", Message: "usage limit reached", HTTPStatus: 429},
	})

	updated, err := m.Update(ctx, &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusDisabled,
		Disabled: true,
	})
	if err != nil {
		t.Fatalf("update auth: %v", err)
	}

	if updated.Quota.Exceeded {
		t.Fatalf("auth.Quota.Exceeded = true, want false for disabled auth update")
	}
	if updated.Unavailable {
		t.Fatalf("auth.Unavailable = true, want false for disabled auth update")
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected no preserved model states for disabled auth update, got %d", len(updated.ModelStates))
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("expected disabled status to be preserved, got disabled=%v status=%q", updated.Disabled, updated.Status)
	}
}

func TestManagerMarkResultSuccessKeepsActiveQuotaCooldown(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	model := "gpt-5.5"

	if _, err := m.Register(ctx, &Auth{ID: "auth-1", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	retryAfter := 2 * time.Hour
	m.MarkResult(ctx, Result{
		AuthID:     "auth-1",
		Provider:   "codex",
		Model:      model,
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &Error{Code: "usage_limit_reached", Message: "usage limit reached", HTTPStatus: 429},
	})

	before, ok := m.GetByID("auth-1")
	if !ok {
		t.Fatalf("expected auth to be present")
	}
	beforeState := before.ModelStates[model]
	if beforeState == nil || !beforeState.Quota.Exceeded {
		t.Fatalf("test setup failed: expected model quota to be exceeded")
	}

	m.MarkResult(ctx, Result{
		AuthID:   "auth-1",
		Provider: "codex",
		Model:    model,
		Success:  true,
	})

	updated, ok := m.GetByID("auth-1")
	if !ok {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.Quota.Exceeded {
		t.Fatalf("state.Quota.Exceeded = false, want true while cooldown is still in the future")
	}
	if !state.NextRetryAfter.Equal(beforeState.NextRetryAfter) {
		t.Fatalf("state.NextRetryAfter = %v, want %v", state.NextRetryAfter, beforeState.NextRetryAfter)
	}
	if !updated.Quota.Exceeded {
		t.Fatalf("auth.Quota.Exceeded = false, want true while cooldown is still in the future")
	}
}
