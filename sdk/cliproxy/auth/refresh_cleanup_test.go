package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// --- test helpers ---

// trackingStore records Save and Delete calls.
type trackingStore struct {
	saveCount   atomic.Int32
	deleteCount atomic.Int32
	lastDeleted string
}

func (s *trackingStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *trackingStore) Save(_ context.Context, _ *Auth) (string, error) {
	s.saveCount.Add(1)
	return "", nil
}
func (s *trackingStore) Delete(_ context.Context, id string) error {
	s.deleteCount.Add(1)
	s.lastDeleted = id
	return nil
}

// stubExecutor is a minimal executor whose Refresh result can be controlled.
type stubExecutor struct {
	refreshErr error
	refreshOut *Auth
}

func (e *stubExecutor) Identifier() string { return "test-provider" }
func (e *stubExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *stubExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *stubExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	if e.refreshErr != nil {
		return nil, e.refreshErr
	}
	if e.refreshOut != nil {
		return e.refreshOut, nil
	}
	return auth, nil
}
func (e *stubExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *stubExecutor) HttpRequest(_ context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
	return nil, nil
}

// --- tests ---

func TestRefreshAuth_PermanentError_MarksCredentialUnavailable(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-1",
		Provider: "test-provider",
		Metadata: map[string]any{"type": "codex"},
	}

	// Register the auth and executor.
	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mgr.RegisterExecutor(&stubExecutor{
		refreshErr: &PermanentAuthError{
			Reason: "credential revoked",
			Cause:  fmt.Errorf("invalid_grant"),
		},
	})

	// Trigger refresh.
	mgr.refreshAuth(context.Background(), "test-auth-1")

	current, ok := mgr.GetByID("test-auth-1")
	if !ok {
		t.Fatal("expected auth to remain in memory after permanent error")
	}
	if current.NextRefreshAfter.IsZero() {
		t.Fatal("expected NextRefreshAfter to be set after permanent error")
	}
	if current.LastError == nil || current.LastError.Message == "" {
		t.Fatal("expected LastError to be set after permanent error")
	}
	if current.Status != StatusError {
		t.Fatalf("expected StatusError after permanent error, got %q", current.Status)
	}
	if current.StatusMessage == "" {
		t.Fatal("expected StatusMessage to be set after permanent error")
	}

	// Store.Delete should NOT have been called: refresh failures are not proof
	// that the user intentionally removed the account.
	if got := store.deleteCount.Load(); got != 0 {
		t.Fatalf("expected 0 Delete calls, got %d", got)
	}
}

func TestRefreshAuth_TransientError_KeepsCredential(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-2",
		Provider: "test-provider",
		Metadata: map[string]any{"type": "codex"},
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mgr.RegisterExecutor(&stubExecutor{
		refreshErr: fmt.Errorf("network timeout"),
	})

	mgr.refreshAuth(context.Background(), "test-auth-2")

	// Auth should still exist in memory.
	current, ok := mgr.GetByID("test-auth-2")
	if !ok {
		t.Fatal("expected auth to remain in memory after transient error")
	}

	// Should have backoff set.
	if current.NextRefreshAfter.IsZero() {
		t.Fatal("expected NextRefreshAfter to be set after transient error")
	}
	if current.LastError == nil || current.LastError.Message == "" {
		t.Fatal("expected LastError to be set after transient error")
	}

	// Store.Delete should NOT have been called.
	if got := store.deleteCount.Load(); got != 0 {
		t.Fatalf("expected 0 Delete calls, got %d", got)
	}
}

func TestRefreshAuth_Success_UpdatesCredential(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-3",
		Provider: "test-provider",
		Metadata: map[string]any{"type": "codex"},
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	updated := auth.Clone()
	updated.Metadata["access_token"] = "new-token"

	mgr.RegisterExecutor(&stubExecutor{refreshOut: updated})

	mgr.refreshAuth(context.Background(), "test-auth-3")

	// Auth should still exist and be updated.
	current, ok := mgr.GetByID("test-auth-3")
	if !ok {
		t.Fatal("expected auth to remain after successful refresh")
	}
	if current.LastError != nil {
		t.Fatal("expected LastError to be nil after successful refresh")
	}
	if !current.LastRefreshedAt.After(time.Time{}) {
		t.Fatal("expected LastRefreshedAt to be set after successful refresh")
	}

	// Store.Delete should NOT have been called.
	if got := store.deleteCount.Load(); got != 0 {
		t.Fatalf("expected 0 Delete calls, got %d", got)
	}

	// Store.Save should have been called (for the Update).
	if got := store.saveCount.Load(); got < 1 {
		t.Fatalf("expected at least 1 Save call for update, got %d", got)
	}
}

func TestIsPermanentAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "plain error",
			err:  fmt.Errorf("network timeout"),
			want: false,
		},
		{
			name: "direct PermanentAuthError",
			err:  &PermanentAuthError{Reason: "revoked"},
			want: true,
		},
		{
			name: "wrapped PermanentAuthError",
			err:  fmt.Errorf("refresh failed: %w", &PermanentAuthError{Reason: "expired"}),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPermanentAuthError(tt.err); got != tt.want {
				t.Errorf("IsPermanentAuthError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPermanentAuthError_Unwrap(t *testing.T) {
	cause := fmt.Errorf("invalid_grant")
	permanent := &PermanentAuthError{Reason: "revoked", Cause: cause}
	if !errors.Is(permanent, cause) {
		t.Fatal("expected errors.Is to find cause through Unwrap")
	}
}
