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
		Runtime:  staticRefreshLead(24 * time.Hour),
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
	// refresh_token_reused can occur from an external concurrent refresh while
	// the upstream session and remaining access_token are still alive.
	m.RegisterExecutor(&stubExecutor{
		id: "codex",
		refreshErr: &PermanentAuthError{
			Reason: "credential revoked or expired",
			Cause:  fmt.Errorf("refresh_token_reused"),
		},
	})

	m.refreshAuth(context.Background(), account.ID)
	afterFail, _ := m.GetByID(account.ID)
	if afterFail.Status != StatusActive {
		t.Fatalf("Status = %q, want active while access token is still valid after refresh_token_reused", afterFail.Status)
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

func TestRefreshAuth_SessionRevokedError_ImmediatelyMarksReauthRequiredPersistsAndBlocksScheduling(t *testing.T) {
	for _, tc := range []struct {
		code       string
		wantReason string
	}{
		{"invalid_refresh_token", "invalid_refresh_token"},
		{"refresh_token_invalidated", "refresh_token_invalidated"},
		{"refresh_token_expired", "refresh_token_expired"},
		{"token_revoked", "token_revoked"},
		{"account_deactivated", "account_deactivated"},
		{"app_session_terminated", "app_session_terminated"},
		{"unknown_future_401_code", "refresh_failed"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			store := &trackingStore{}
			m := NewManager(store, nil, nil)
			now := time.Now()

			// Even when the local expired timestamp is still days away (as when
			// proactive refresh runs early), OpenAI session-revocation errors
			// invalidate the session immediately and must not stay StatusActive.
			account := &Auth{
				ID:       "codex-session-dead",
				Provider: "codex",
				Status:   StatusActive,
				Runtime:  staticRefreshLead(24 * time.Hour),
				Metadata: map[string]any{
					"type":          "codex",
					"email":         "user@example.com",
					"access_token":  "locally-unexpired-access-token",
					"refresh_token": "revoked-refresh-token",
					"expired":       now.Add(5 * 24 * time.Hour).Format(time.RFC3339),
					"last_refresh":  now.Add(-5 * 24 * time.Hour).Format(time.RFC3339),
				},
				ModelStates: map[string]*ModelState{
					"gpt-5.4":       {Status: StatusActive},
					"gpt-5.3-codex": {Status: StatusActive},
				},
			}
			healthy := &Auth{
				ID:       "codex-healthy",
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{
					"type":          "codex",
					"email":         "healthy@example.com",
					"access_token":  "healthy-access-token",
					"refresh_token": "healthy-refresh-token",
					"expired":       now.Add(9 * 24 * time.Hour).Format(time.RFC3339),
				},
			}
			registerAuth(t, m, account)
			registerAuth(t, m, healthy)
			m.RegisterExecutor(&stubExecutor{
				id: "codex",
				refreshErr: &PermanentAuthError{
					Reason: "credential revoked or expired",
					Cause:  fmt.Errorf("token refresh failed with status 401: {\"error\":{\"code\":%q}}", tc.code),
				},
			})

			m.refreshAuth(context.Background(), account.ID)

			got, ok := m.GetByID(account.ID)
			if !ok {
				t.Fatal("expected auth to remain registered")
			}
			if got.Status != StatusError || !got.Unavailable {
				t.Fatalf("status=%q unavailable=%v, want error + unavailable", got.Status, got.Unavailable)
			}
			health := codexHealth(t, got)
			if health["status"] != codexOAuthHealthStatusReauthRequired || health["reauth_reason"] != tc.wantReason {
				t.Fatalf("health = %#v, want reauth_required / %s", health, tc.wantReason)
			}
			if store.saveCount.Load() == 0 {
				t.Fatal("expected reauth_required health to be persisted immediately for peer nodes")
			}
			if m.shouldRefresh(got, now.Add(2*time.Hour)) {
				t.Fatal("shouldRefresh = true after session revocation")
			}

			// Blocked across every model, both immediately and well after 30 minutes.
			for _, checkAt := range []time.Time{now.Add(time.Second), now.Add(2 * time.Hour)} {
				for _, model := range []string{"gpt-5.4", "gpt-5.3-codex", "gpt-5.4-mini", ""} {
					block := authBlockForModel(got, model, checkAt)
					if !block.blocked {
						t.Fatalf("authBlockForModel(%q, +%v) = unblocked, want blocked", model, checkAt.Sub(now))
					}
					if reason := CooldownReasonOf(block.lastErr, QuotaState{}); reason != CooldownReasonReauthRequired {
						t.Fatalf("block reason(%q) = %q, want %q", model, reason, CooldownReasonReauthRequired)
					}
				}
			}

			// Selection skips the revoked credential and routes straight to healthy.
			available, err := getAvailableAuths([]*Auth{got, healthy}, "codex", "gpt-5.4-mini", now.Add(2*time.Hour))
			if err != nil {
				t.Fatalf("getAvailableAuths: %v", err)
			}
			if len(available) != 1 || available[0].ID != healthy.ID {
				t.Fatalf("available = %+v, want only %s", available, healthy.ID)
			}

			// Pinned sessions escape immediately instead of hitting 401.
			held, pinReason := m.PinnedAuthHeldBack(account.ID, "gpt-5.4-mini")
			if !held || pinReason != CooldownReasonReauthRequired {
				t.Fatalf("PinnedAuthHeldBack = (%v, %q), want (true, %q)", held, pinReason, CooldownReasonReauthRequired)
			}

			// Routing simulator reports reauth_required with HTTP 401.
			simBlocked, simUntil, simCause, simStatus := candidateBlockOf(got, "gpt-5.4-mini", now.Add(2*time.Hour))
			if simBlocked != SimulatedBlockUnavailable || !simUntil.IsZero() || simCause != CooldownReasonReauthRequired || simStatus != http.StatusUnauthorized {
				t.Fatalf("candidateBlockOf = (%q, %v, %q, %d), want (unavailable, zero, reauth_required, 401)", simBlocked, simUntil, simCause, simStatus)
			}

			// Simulate a restart / peer node reload where only persisted metadata
			// (no in-memory LastError or ModelStates) is loaded from DB.
			reloaded := &Auth{
				ID:       account.ID,
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{
					"type":                      "codex",
					"email":                     "user@example.com",
					"access_token":              "locally-unexpired-access-token",
					"refresh_token":             "revoked-refresh-token",
					CodexOAuthHealthMetadataKey: cloneCodexOAuthHealth(got),
				},
			}
			if block := authBlockForModel(reloaded, "gpt-5.4", now.Add(5*time.Hour)); !block.blocked || CooldownReasonOf(block.lastErr, QuotaState{}) != CooldownReasonReauthRequired {
				t.Fatalf("reloaded credential was not blocked from persisted codex_oauth_health: %+v", block)
			}

			// Re-login clears the block cleanly.
			recovered, err := m.Update(WithCredentialCreate(context.Background()), &Auth{
				ID:       account.ID,
				Provider: "codex",
				Status:   StatusActive,
				Metadata: map[string]any{
					"type":          "codex",
					"email":         "user@example.com",
					"access_token":  "fresh-access-token",
					"refresh_token": "fresh-refresh-token",
				},
			})
			if err != nil {
				t.Fatalf("re-login Update: %v", err)
			}
			if block := authBlockForModel(recovered, "gpt-5.4", now.Add(5*time.Hour)); block.blocked {
				t.Fatalf("re-logged-in credential remained blocked: %+v", block)
			}
		})
	}
}

func TestCodexOAuth401ReauthBlocksAllModelsBeyond30Minutes(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()

	account := codexOAuthAccount("codex-revoked-runtime", "dead-refresh-token")
	account.ModelStates = map[string]*ModelState{
		"gpt-5.4":       {Status: StatusActive},
		"gpt-5.3-codex": {Status: StatusActive},
	}
	registerAuth(t, m, account)

	codex401(m, account.ID, "gpt-5.4", `{"error":{"code":"token_revoked","message":"Encountered invalidated oauth token for user"}}`)

	got, _ := m.GetByID(account.ID)
	if !got.Unavailable || got.Status != StatusError {
		t.Fatalf("auth unavailable=%v status=%q, want unavailable + error", got.Unavailable, got.Status)
	}
	if other := got.ModelStates["gpt-5.3-codex"]; other == nil || !other.Unavailable {
		t.Fatalf("existing sibling model state was not held back: %+v", other)
	}

	// Even 45 minutes later (past the 30-minute timer) and on a brand-new model,
	// selection must still refuse the dead credential instead of probing with live traffic.
	for _, model := range []string{"gpt-5.4", "gpt-5.3-codex", "gpt-5.2"} {
		if block := authBlockForModel(got, model, now.Add(45*time.Minute)); !block.blocked {
			t.Fatalf("model %q became schedulable again after 30m while still reauth_required", model)
		}
	}

	// Operator ClearQuotaStatus lifts the reauth block explicitly if desired.
	if changed, err := m.ClearQuotaStatus(context.Background(), account.ID); err != nil || !changed {
		t.Fatalf("ClearQuotaStatus = (%v, %v), want (true, nil)", changed, err)
	}
	cleared, _ := m.GetByID(account.ID)
	if block := authBlockForModel(cleared, "gpt-5.4", now.Add(45*time.Minute)); block.blocked {
		t.Fatalf("operator-cleared credential remained blocked: %+v", block)
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
