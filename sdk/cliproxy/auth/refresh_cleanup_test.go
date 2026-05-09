package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	listItems   []*Auth
	listHook    func()
}

func (s *trackingStore) List(context.Context) ([]*Auth, error) {
	out := make([]*Auth, 0, len(s.listItems))
	for _, auth := range s.listItems {
		if auth != nil {
			out = append(out, auth.Clone())
		}
	}
	if s.listHook != nil {
		s.listHook()
	}
	return out, nil
}
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
	id         string
	refreshErr error
	refreshOut *Auth
	onRefresh  func()
}

func (e *stubExecutor) Identifier() string {
	if e.id != "" {
		return e.id
	}
	return "test-provider"
}
func (e *stubExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *stubExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *stubExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	if e.onRefresh != nil {
		e.onRefresh()
	}
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
	if current.Status != StatusRevoked {
		t.Fatalf("expected StatusRevoked after permanent error, got %q", current.Status)
	}
	if current.StatusMessage != "credential_revoked" {
		t.Fatalf("expected StatusMessage 'credential_revoked', got %q", current.StatusMessage)
	}

	// Store.Delete should NOT have been called: refresh failures are not proof
	// that the user intentionally removed the account.
	if got := store.deleteCount.Load(); got != 0 {
		t.Fatalf("expected 0 Delete calls, got %d", got)
	}
}

func TestRefreshAuth_PermanentError_KeepsUsableAccessTokenActive(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-usable-token",
		Provider: "test-provider",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "still-usable-access-token",
			"refresh_token": "stale-refresh-token",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mgr.RegisterExecutor(&stubExecutor{
		refreshErr: &PermanentAuthError{
			Reason: "refresh token reused",
			Cause:  fmt.Errorf("refresh_token_reused"),
		},
	})

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain in memory after permanent error")
	}
	if current.Status != StatusRevoked {
		t.Fatalf("expected usable access token to be marked revoked after permanent refresh failure, got %q", current.Status)
	}
	if current.StatusMessage != "credential_revoked" {
		t.Fatalf("expected StatusMessage 'credential_revoked', got %q", current.StatusMessage)
	}
	if current.LastError == nil || current.LastError.Message == "" {
		t.Fatal("expected LastError to retain refresh failure diagnostics")
	}
	if current.NextRefreshAfter.IsZero() {
		t.Fatal("expected NextRefreshAfter to be set after permanent error")
	}
	if got := store.deleteCount.Load(); got != 0 {
		t.Fatalf("expected 0 Delete calls, got %d", got)
	}
}

func TestRefreshAuth_PermanentError_RecoversRotatedRefreshTokenFromStore(t *testing.T) {
	stored := &Auth{
		ID:       "test-auth-rotated-token",
		Provider: "test-provider",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	store := &trackingStore{listItems: []*Auth{stored}}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       stored.ID,
		Provider: stored.Provider,
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}

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

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain in memory after permanent error")
	}
	if got, _ := current.Metadata["refresh_token"].(string); got != "new-refresh-token" {
		t.Fatalf("refresh_token = %q, want new-refresh-token", got)
	}
	if got, _ := current.Metadata["access_token"].(string); got != "new-access-token" {
		t.Fatalf("access_token = %q, want new-access-token", got)
	}
	if current.Status != StatusActive {
		t.Fatalf("expected recovered auth to remain active, got %q", current.Status)
	}
	if current.LastError != nil {
		t.Fatalf("expected recovered auth LastError to be nil, got %+v", current.LastError)
	}
	if !current.NextRefreshAfter.IsZero() {
		t.Fatalf("expected recovered auth NextRefreshAfter to be cleared, got %s", current.NextRefreshAfter)
	}
	if got := store.deleteCount.Load(); got != 0 {
		t.Fatalf("expected 0 Delete calls, got %d", got)
	}
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("expected recovery to avoid overwriting store, got %d Save calls", got)
	}
}

func TestRefreshAuth_PermanentError_RecoversRotatedRefreshTokenFromMemory(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-memory-rotated-token",
		Provider: "test-provider",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mgr.RegisterExecutor(&stubExecutor{
		refreshErr: &PermanentAuthError{
			Reason: "credential revoked",
			Cause:  fmt.Errorf("invalid_grant"),
		},
		onRefresh: func() {
			mgr.mu.Lock()
			latest := auth.Clone()
			latest.Metadata["access_token"] = "memory-access-token"
			latest.Metadata["refresh_token"] = "memory-refresh-token"
			latest.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
			mgr.auths[auth.ID] = latest
			mgr.mu.Unlock()
		},
	})

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain in memory after recovery")
	}
	if got, _ := current.Metadata["refresh_token"].(string); got != "memory-refresh-token" {
		t.Fatalf("refresh_token = %q, want memory-refresh-token", got)
	}
	if current.Status != StatusActive {
		t.Fatalf("expected recovered auth to remain active, got %q", current.Status)
	}
	if current.LastError != nil {
		t.Fatalf("expected recovered auth LastError to be nil, got %+v", current.LastError)
	}
	if !current.NextRefreshAfter.IsZero() {
		t.Fatalf("expected recovered auth NextRefreshAfter to be cleared, got %s", current.NextRefreshAfter)
	}
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("expected recovery to avoid overwriting store, got %d Save calls", got)
	}
}

func TestRefreshAuth_PermanentError_DoesNotReviveRemovedAuth(t *testing.T) {
	stored := &Auth{
		ID:       "test-auth-removed-before-recovery",
		Provider: "test-provider",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	store := &trackingStore{listItems: []*Auth{stored}}
	mgr := NewManager(store, nil, nil)

	used := &Auth{
		ID:       stored.ID,
		Provider: stored.Provider,
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}

	mgr.mu.Lock()
	mgr.auths[used.ID] = used.Clone()
	mgr.mu.Unlock()

	store.listHook = func() {
		mgr.mu.Lock()
		delete(mgr.auths, used.ID)
		mgr.mu.Unlock()
	}

	if mgr.recoverRotatedRefreshToken(context.Background(), used.ID, used, time.Now(), fmt.Errorf("invalid_grant")) {
		t.Fatal("expected recovery to fail after auth was removed")
	}
	if _, ok := mgr.GetByID(used.ID); ok {
		t.Fatal("expected removed auth not to be revived")
	}
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("expected no Save calls, got %d", got)
	}
}

func TestRefreshAuth_PermanentError_RecoveryMarksUnusableAccessTokenError(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{
			name: "missing access token",
			metadata: map[string]any{
				"type":          "codex",
				"refresh_token": "new-refresh-token",
				"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		},
		{
			name: "expired access token",
			metadata: map[string]any{
				"type":          "codex",
				"access_token":  "expired-access-token",
				"refresh_token": "new-refresh-token",
				"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := &Auth{
				ID:       "test-auth-unusable-recovery-" + strings.ReplaceAll(tt.name, " ", "-"),
				Provider: "test-provider",
				Status:   StatusActive,
				Metadata: tt.metadata,
			}
			store := &trackingStore{listItems: []*Auth{stored}}
			mgr := NewManager(store, nil, nil)

			auth := &Auth{
				ID:       stored.ID,
				Provider: stored.Provider,
				Status:   StatusActive,
				Metadata: map[string]any{
					"type":          "codex",
					"access_token":  "old-access-token",
					"refresh_token": "old-refresh-token",
					"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
				},
			}

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

			mgr.refreshAuth(context.Background(), auth.ID)

			current, ok := mgr.GetByID(auth.ID)
			if !ok {
				t.Fatal("expected auth to remain in memory after recovery")
			}
			if got := authRefreshToken(current); got != "new-refresh-token" {
				t.Fatalf("refresh_token = %q, want new-refresh-token", got)
			}
			if current.Status != StatusError {
				t.Fatalf("expected unusable recovered token to be marked error, got %q", current.Status)
			}
			if current.StatusMessage == "" {
				t.Fatal("expected StatusMessage to retain refresh failure diagnostics")
			}
			if current.LastError == nil || current.LastError.Message == "" {
				t.Fatal("expected LastError to retain refresh failure diagnostics")
			}
			if current.NextRefreshAfter.IsZero() {
				t.Fatal("expected NextRefreshAfter to be set for unusable recovered token")
			}
			if got := store.saveCount.Load(); got != 0 {
				t.Fatalf("expected recovery to avoid overwriting store, got %d Save calls", got)
			}
		})
	}
}

func TestRefreshAuth_PermanentError_DoesNotKeepGenericProviderActive(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-generic-provider",
		Provider: "test-provider",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "test-provider",
			"access_token":  "still-usable-access-token",
			"refresh_token": "stale-refresh-token",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mgr.RegisterExecutor(&stubExecutor{
		refreshErr: &PermanentAuthError{
			Reason: "generic permanent failure",
			Cause:  fmt.Errorf("generic_permanent_failure"),
		},
	})

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain in memory after permanent error")
	}
	if current.Status != StatusRevoked {
		t.Fatalf("expected generic provider to be marked revoked after permanent refresh failure, got %q", current.Status)
	}
	if current.StatusMessage != "credential_revoked" {
		t.Fatalf("expected StatusMessage 'credential_revoked', got %q", current.StatusMessage)
	}
}

func TestRefreshAuth_PermanentError_RecoveryPreservesDisabledState(t *testing.T) {
	stored := &Auth{
		ID:       "test-auth-disabled-recovery",
		Provider: "test-provider",
		Status:   StatusDisabled,
		Disabled: true,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
			"disabled":      true,
		},
	}
	store := &trackingStore{listItems: []*Auth{stored}}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       stored.ID,
		Provider: stored.Provider,
		Status:   StatusDisabled,
		Disabled: true,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
			"disabled":      true,
		},
	}

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

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain in memory after recovery")
	}
	if !current.Disabled {
		t.Fatal("expected recovered auth to remain disabled")
	}
	if current.Status != StatusDisabled {
		t.Fatalf("expected recovered auth to keep disabled status, got %q", current.Status)
	}
	if got, _ := current.Metadata["refresh_token"].(string); got != "new-refresh-token" {
		t.Fatalf("refresh_token = %q, want new-refresh-token", got)
	}
}

func TestRefreshAuth_PermanentError_RecoveryPreservesQuotaExceededState(t *testing.T) {
	stored := &Auth{
		ID:       "test-auth-quota-recovery",
		Provider: "test-provider",
		Status:   StatusError,
		Quota:    QuotaState{Exceeded: true},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	store := &trackingStore{listItems: []*Auth{stored}}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       stored.ID,
		Provider: stored.Provider,
		Status:   StatusError,
		Quota:    QuotaState{Exceeded: true},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}

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

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain in memory after recovery")
	}
	if !current.Quota.Exceeded {
		t.Fatal("expected recovered auth to preserve quota exceeded state")
	}
	if current.Status != StatusError {
		t.Fatalf("expected recovered auth to keep quota status, got %q", current.Status)
	}
	if got, _ := current.Metadata["refresh_token"].(string); got != "new-refresh-token" {
		t.Fatalf("refresh_token = %q, want new-refresh-token", got)
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

func TestRefreshAuth_Success_ResumesUnauthorizedSuspendedModels(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-refresh-resume",
		Provider: "test-provider",
		Status:   StatusError,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
		ModelStates: map[string]*ModelState{
			"codex-mini": {
				Unavailable:    true,
				Status:         StatusError,
				StatusMessage:  "unauthorized",
				NextRetryAfter: time.Now().Add(30 * time.Minute),
			},
		},
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(30 * time.Minute),
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mgr.RegisterExecutor(&stubExecutor{})

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain after refresh")
	}
	if current.Unavailable {
		t.Fatal("expected auth to no longer be unavailable after successful refresh")
	}
	if current.Status != StatusActive {
		t.Fatalf("expected auth status Active, got %q", current.Status)
	}
	if !current.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be cleared, got %s", current.NextRetryAfter)
	}
	state, exists := current.ModelStates["codex-mini"]
	if !exists || state == nil {
		t.Fatal("expected model state to exist")
	}
	if state.Unavailable {
		t.Fatal("expected model to no longer be unavailable after refresh")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected model NextRetryAfter to be cleared, got %s", state.NextRetryAfter)
	}
	if state.StatusMessage != "" {
		t.Fatalf("expected model StatusMessage to be cleared, got %q", state.StatusMessage)
	}
}

func TestRefreshAuth_Success_DoesNotResumeNonUnauthorizedModels(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-refresh-no-resume-quota",
		Provider: "test-provider",
		Status:   StatusError,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
			"expired":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
		ModelStates: map[string]*ModelState{
			"codex-mini": {
				Unavailable:    true,
				Status:         StatusError,
				StatusMessage:  "unauthorized",
				NextRetryAfter: time.Now().Add(30 * time.Minute),
			},
			"codex-pro": {
				Unavailable:    true,
				Status:         StatusError,
				StatusMessage:  "quota",
				NextRetryAfter: time.Now().Add(30 * time.Minute),
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: time.Now().Add(30 * time.Minute),
				},
			},
		},
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(30 * time.Minute),
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mgr.RegisterExecutor(&stubExecutor{})

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain after refresh")
	}

	quotaState := current.ModelStates["codex-pro"]
	if quotaState == nil {
		t.Fatal("expected codex-pro model state to exist")
	}
	if !quotaState.Unavailable {
		t.Fatal("expected quota-suspended model to remain unavailable")
	}
	if quotaState.StatusMessage != "quota" {
		t.Fatalf("expected quota model StatusMessage to remain 'quota', got %q", quotaState.StatusMessage)
	}
}

func TestMarkResult_401_Invalidated_SetsRevokedStatus(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-revoked-401",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "some-token",
			"refresh_token": "some-refresh-token",
		},
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID: auth.ID,
		Model:  "codex-mini",
		Error: &Error{
			Message:    "Your authentication token has been invalidated. Please try signing in again.",
			HTTPStatus: 401,
		},
	})

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to exist")
	}
	if current.Status != StatusRevoked {
		t.Fatalf("expected status revoked, got %q", current.Status)
	}
	if current.StatusMessage != "credential_revoked" {
		t.Fatalf("expected status_message 'credential_revoked', got %q", current.StatusMessage)
	}
}

func TestMarkResult_401_Expired_NotRevokedStatus(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-expired-401",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "some-token",
			"refresh_token": "some-refresh-token",
		},
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID: auth.ID,
		Model:  "codex-mini",
		Error: &Error{
			Message:    "token_expired",
			HTTPStatus: 401,
		},
	})

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to exist")
	}
	if current.Status == StatusRevoked {
		t.Fatal("expected status NOT to be revoked for simple token expiration")
	}
}

func TestRefreshAuth_PermanentError_SetsRevokedStatus(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)

	auth := &Auth{
		ID:       "test-auth-permanent-revoked",
		Provider: "test-provider",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old-access-token",
			"refresh_token": "old-refresh-token",
		},
	}

	ctx := WithSkipPersist(context.Background())
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	mgr.RegisterExecutor(&stubExecutor{
		refreshErr: &PermanentAuthError{
			Reason: "credential revoked or expired",
			Cause:  fmt.Errorf("invalid_grant"),
		},
	})

	mgr.refreshAuth(context.Background(), auth.ID)

	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected auth to remain after permanent refresh failure")
	}
	if current.Status != StatusRevoked {
		t.Fatalf("expected status revoked, got %q", current.Status)
	}
	if current.StatusMessage != "credential_revoked" {
		t.Fatalf("expected status_message 'credential_revoked', got %q", current.StatusMessage)
	}
}

func TestClearAuthStateOnSuccess_DoesNotClearRevoked(t *testing.T) {
	auth := &Auth{
		Status:        StatusRevoked,
		StatusMessage: "credential_revoked",
		Unavailable:   true,
	}
	clearAuthStateOnSuccess(auth, time.Now())
	if auth.Status != StatusRevoked {
		t.Fatalf("expected revoked status to be preserved, got %q", auth.Status)
	}
	if auth.StatusMessage != "credential_revoked" {
		t.Fatalf("expected status_message to be preserved, got %q", auth.StatusMessage)
	}
}

func TestClearQuotaAuthState_DoesNotClearRevoked(t *testing.T) {
	auth := &Auth{
		Status:        StatusRevoked,
		StatusMessage: "credential_revoked",
		Unavailable:   true,
	}
	changed := clearQuotaAuthState(auth, time.Now())
	if changed {
		t.Fatal("expected no change for revoked auth")
	}
	if auth.Status != StatusRevoked {
		t.Fatalf("expected revoked status to be preserved, got %q", auth.Status)
	}
}

func TestIsCredentialRevokedMessage(t *testing.T) {
	tests := []struct {
		message  string
		expected bool
	}{
		{"Your authentication token has been invalidated. Please try signing in again.", true},
		{"Token has been revoked", true},
		{"Your account has been suspended", true},
		{"Your account has been banned", true},
		{"token_expired", false},
		{"rate limit exceeded", false},
		{"internal server error", false},
		{"", false},
	}
	for _, tt := range tests {
		var err *Error
		if tt.message != "" {
			err = &Error{Message: tt.message}
		}
		got := isCredentialRevokedMessage(err)
		if got != tt.expected {
			t.Errorf("isCredentialRevokedMessage(%q) = %v, want %v", tt.message, got, tt.expected)
		}
	}
}
