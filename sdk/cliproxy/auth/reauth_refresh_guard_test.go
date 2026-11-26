package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type staticRefreshLead time.Duration

func (d staticRefreshLead) RefreshLead() *time.Duration {
	v := time.Duration(d)
	return &v
}

func TestCodexOAuthReauthRequiredStopsAutoRefresh(t *testing.T) {
	now := time.Now()
	expiredAt := now.Add(-2 * time.Hour).Format(time.RFC3339)
	lastRefresh := now.Add(-24 * time.Hour).Format(time.RFC3339)

	for _, code := range []string{"token_revoked", "invalid_refresh_token", "refresh_token_invalidated", "refresh_token_expired"} {
		t.Run(code, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			account := codexOAuthAccount("codex-dead", "dead-refresh-token")
			account.Runtime = staticRefreshLead(5 * 24 * time.Hour)
			account.Metadata["expired"] = expiredAt
			account.Metadata["last_refresh"] = lastRefresh
			registerAuth(t, m, account)

			// Sanity: before 401 reauth_required, an expired account is due for refresh.
			before, _ := m.GetByID("codex-dead")
			if !m.shouldRefresh(before, now) {
				t.Fatal("setup: expected expired codex credential to be due for refresh")
			}

			codex401(m, "codex-dead", "gpt-5-codex", `{"error":{"code":"`+code+`","message":"dead token"}}`)
			after, _ := m.GetByID("codex-dead")
			if health := codexHealth(t, after); health["status"] != codexOAuthHealthStatusReauthRequired || health["reauth_reason"] != code {
				t.Fatalf("health = %#v, want reauth_required / %s", health, code)
			}
			// Even hours later after any backoff expires, reauth_required must stop auto-refresh.
			if m.shouldRefresh(after, now.Add(2*time.Hour)) {
				t.Fatalf("shouldRefresh = true for reauth_required (%s) credential", code)
			}
			state := after.ModelStates["gpt-5-codex"]
			if reason := CooldownReasonOf(state.LastError, state.Quota); reason != CooldownReasonReauthRequired {
				t.Fatalf("cooldown reason = %q, want %q", reason, CooldownReasonReauthRequired)
			}
		})
	}
}

func TestRefreshAuth_PermanentError_CodexMarksReauthRequiredAndStopsLooping(t *testing.T) {
	store := &trackingStore{}
	m := NewManager(store, nil, nil)
	now := time.Now()

	account := &Auth{
		ID:       "codex-expired-refresh",
		Provider: "codex",
		Status:   StatusActive,
		Runtime:  staticRefreshLead(5 * 24 * time.Hour),
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "user@example.com",
			"access_token":  "expired-access-token",
			"refresh_token": "dead-refresh-token",
			"expired":       now.Add(-time.Hour).Format(time.RFC3339),
			"last_refresh":  now.Add(-48 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, err := m.Register(WithSkipPersist(context.Background()), account); err != nil {
		t.Fatalf("Register: %v", err)
	}
	m.RegisterExecutor(&stubExecutor{
		id: "codex",
		refreshErr: &PermanentAuthError{
			Reason: "credential revoked or expired",
			Cause:  fmt.Errorf("token refresh failed with status 401: {\"error\":{\"code\":\"invalid_refresh_token\"}}"),
		},
	})

	m.refreshAuth(context.Background(), account.ID)

	got, ok := m.GetByID(account.ID)
	if !ok {
		t.Fatal("expected auth to remain registered")
	}
	if got.Status != StatusError {
		t.Fatalf("Status = %q, want error", got.Status)
	}
	health := codexHealth(t, got)
	if health["status"] != codexOAuthHealthStatusReauthRequired || health["reauth_reason"] != "invalid_refresh_token" {
		t.Fatalf("health = %#v, want reauth_required / invalid_refresh_token", health)
	}
	if store.saveCount.Load() == 0 {
		t.Fatal("expected runtime reauth_required health to be persisted for cluster peers")
	}
	// After refreshFailureBackoff passes, shouldRefresh must stay false for the dead token.
	if m.shouldRefresh(got, now.Add(2*time.Hour)) {
		t.Fatal("shouldRefresh = true after permanent refresh failure")
	}
}

func TestRefreshAuth_PermanentError_UsableAccessTokenStopsRetryingDeadRefreshTokenUntilReplaced(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()

	account := &Auth{
		ID:       "codex-usable-access",
		Provider: "codex",
		Status:   StatusActive,
		Runtime:  staticRefreshLead(5 * 24 * time.Hour),
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "user@example.com",
			"access_token":  "still-valid-access-token",
			"refresh_token": "dead-refresh-token",
			"expired":       now.Add(2 * time.Hour).Format(time.RFC3339),
			"last_refresh":  now.Add(-48 * time.Hour).Format(time.RFC3339),
		},
	}
	registerAuth(t, m, account)
	m.RegisterExecutor(&stubExecutor{
		id: "codex",
		refreshErr: &PermanentAuthError{
			Reason: "credential revoked or expired",
			Cause:  fmt.Errorf("refresh_token_invalidated"),
		},
	})

	m.refreshAuth(context.Background(), account.ID)
	afterFail, _ := m.GetByID(account.ID)
	if afterFail.Status != StatusActive {
		t.Fatalf("Status = %q, want active while access token is still valid", afterFail.Status)
	}
	// Once NextRefreshAfter elapses, the dead refresh_token must not be retried again.
	if m.shouldRefresh(afterFail, now.Add(time.Hour)) {
		t.Fatal("shouldRefresh = true for a refresh_token that already failed permanently")
	}

	// If the remaining access token later draws a 401 token_expired, the dead
	// refresh token must transition directly to reauth_required instead of refresh_pending.
	codex401(m, account.ID, "gpt-5-codex", codexExpiredBody)
	after401, _ := m.GetByID(account.ID)
	if health := codexHealth(t, after401); health["status"] != codexOAuthHealthStatusReauthRequired {
		t.Fatalf("health = %#v, want reauth_required when refresh_token already failed permanently", health)
	}

	// Re-login with a brand new refresh token clears the permanent failure block.
	replacement := &Auth{
		ID:       account.ID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "user@example.com",
			"access_token":  "brand-new-access-token",
			"refresh_token": "brand-new-refresh-token",
			"expired":       now.Add(-time.Minute).Format(time.RFC3339),
		},
	}
	updated, err := m.Update(WithCredentialCreate(context.Background()), replacement)
	if err != nil {
		t.Fatalf("Update(re-login): %v", err)
	}
	if !m.shouldRefresh(updated, now) {
		t.Fatal("shouldRefresh = false after installing a new refresh token")
	}
}

func TestManagerUpdate_ReauthClears401CooldownButKeeps429Quota(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)

	registerAuth(t, m, codexOAuthAccount("codex-1", "old-refresh-token"))
	codex401(m, "codex-1", "gpt-5-codex", `{"error":{"code":"token_revoked","message":"revoked"}}`)

	retryAfter := 2 * time.Hour
	m.MarkResult(ctx, Result{
		AuthID:     "codex-1",
		Provider:   "codex",
		Model:      "gpt-5-quota",
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &Error{Code: "usage_limit_reached", Message: "usage limit reached", HTTPStatus: http.StatusTooManyRequests},
	})

	// Plain metadata/config edit with identical tokens must NOT clear the 401 cooldown.
	sameTokenEdit := codexOAuthAccount("codex-1", "old-refresh-token")
	sameTokenEdit.ProxyID = "proxy-a"
	edited, err := m.Update(ctx, sameTokenEdit)
	if err != nil {
		t.Fatalf("plain Update: %v", err)
	}
	if edited.ModelStates["gpt-5-codex"] == nil || !edited.ModelStates["gpt-5-codex"].Unavailable {
		t.Fatal("plain config Update unexpectedly cleared 401 model cooldown")
	}

	// Explicit re-login (WithCredentialCreate) clears 401 cooldown, StatusError,
	// and stale codex_oauth_health while preserving genuine 429 quota state.
	relogin := &Auth{
		ID:       "codex-1",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "codex@example.com",
			"access_token":  "fresh-access-token",
			"refresh_token": "fresh-refresh-token",
		},
	}
	updated, err := m.Update(WithCredentialCreate(ctx), relogin)
	if err != nil {
		t.Fatalf("re-login Update: %v", err)
	}
	if updated.ModelStates["gpt-5-codex"] != nil {
		t.Fatalf("401 cooldown survived re-login: %+v", updated.ModelStates["gpt-5-codex"])
	}
	if _, hasHealth := updated.Metadata[CodexOAuthHealthMetadataKey]; hasHealth {
		t.Fatalf("stale codex_oauth_health survived re-login: %#v", updated.Metadata[CodexOAuthHealthMetadataKey])
	}
	if quotaState := updated.ModelStates["gpt-5-quota"]; quotaState == nil || !quotaState.Quota.Exceeded {
		t.Fatalf("429 quota cooldown should remain preserved: %+v", quotaState)
	}

	// Re-login of a credential that only had a 401 cooldown returns straight to StatusActive.
	registerAuth(t, m, codexOAuthAccount("codex-401-only", "old-refresh-token"))
	codex401(m, "codex-401-only", "gpt-5-codex", `{"error":{"code":"token_revoked","message":"revoked"}}`)
	recovered, err := m.Update(WithCredentialCreate(ctx), &Auth{
		ID:       "codex-401-only",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "codex@example.com",
			"access_token":  "fresh-access-token",
			"refresh_token": "fresh-refresh-token",
		},
	})
	if err != nil {
		t.Fatalf("re-login Update(401-only): %v", err)
	}
	if recovered.Status != StatusActive || recovered.Unavailable || len(recovered.ModelStates) != 0 {
		t.Fatalf("401-only re-login did not recover cleanly: status=%q unavailable=%v states=%+v", recovered.Status, recovered.Unavailable, recovered.ModelStates)
	}
}

func TestManagerUpdate_ClusterSyncNewTokensClearsStale401Cooldown(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	account := codexOAuthAccount("codex-sync", "old-refresh-token")
	account.NextRefreshAfter = time.Now().Add(15 * time.Minute)
	registerAuth(t, m, account)
	codex401(m, "codex-sync", "gpt-5-codex", `{"error":{"code":"token_revoked","message":"revoked"}}`)

	// Peer node reloads the newly signed-in credential without WithCredentialCreate.
	synced := &Auth{
		ID:       "codex-sync",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "codex@example.com",
			"access_token":  "peer-new-access-token",
			"refresh_token": "peer-new-refresh-token",
		},
	}
	updated, err := m.Update(WithSkipPersist(ctx), synced)
	if err != nil {
		t.Fatalf("cluster sync Update: %v", err)
	}
	if updated.ModelStates["gpt-5-codex"] != nil {
		t.Fatalf("401 cooldown survived peer token sync: %+v", updated.ModelStates["gpt-5-codex"])
	}
	if updated.Status != StatusActive || updated.Unavailable {
		t.Fatalf("auth remained degraded after peer token sync: status=%q unavailable=%v msg=%q", updated.Status, updated.Unavailable, updated.StatusMessage)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero after new refresh token synced", updated.NextRefreshAfter)
	}
}
