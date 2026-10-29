package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestCodexUnauthorizedCode(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{"nested code", &Error{HTTPStatus: 401, Message: `{"error":{"message":"Provided authentication token is expired.","code":"token_expired"}}`}, "token_expired"},
		{"revoked", &Error{HTTPStatus: 401, Message: `{"error":{"code":"token_revoked","message":"Encountered invalidated oauth token for user"}}`}, "token_revoked"},
		{"top level code", &Error{HTTPStatus: 401, Message: `{"code":"account_deactivated","message":"deactivated"}`}, "account_deactivated"},
		{"oauth style error string", &Error{HTTPStatus: 401, Message: `{"error":"invalid_grant","error_description":"Token has been revoked."}`}, "invalid_grant"},
		{"reused refresh token", &Error{HTTPStatus: 401, Message: `{"error":{"code":"refresh_token_reused"}}`}, "refresh_token_reused"},
		{"code carried on the error", &Error{HTTPStatus: 401, Code: "token_expired", Message: "unauthorized"}, "token_expired"},
		{"case and padding are ignored", &Error{HTTPStatus: 401, Message: `{"error":{"code":" Token_Expired "}}`}, "token_expired"},
		{"a code the upstream sends that we do not know", &Error{HTTPStatus: 401, Message: `{"error":{"code":"invalid_api_key"}}`}, ""},
		{"a code that merely contains a known one", &Error{HTTPStatus: 401, Message: `{"error":{"code":"token_expired_soon"}}`}, ""},
		{"a plain text body", &Error{HTTPStatus: 401, Message: "Unauthorized"}, ""},
		{"a known word outside a code field", &Error{HTTPStatus: 401, Message: `{"error":{"message":"token_expired"}}`}, ""},
		{"an html page", &Error{HTTPStatus: 401, Message: "<html>token_revoked</html>"}, ""},
		{"empty", &Error{HTTPStatus: 401}, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexUnauthorizedCode(tc.err); got != tc.want {
				t.Fatalf("codexUnauthorizedCode = %q, want %q", got, tc.want)
			}
		})
	}
}

func codexOAuthAccount(id string, refreshToken string) *Auth {
	meta := map[string]any{"type": "codex", "email": "codex@example.com", "access_token": "old-access-token"}
	if refreshToken != "" {
		meta["refresh_token"] = refreshToken
	}
	return &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: meta}
}

func codex401(m *Manager, id, model, body string) {
	m.MarkResult(context.Background(), Result{
		AuthID: id, Provider: "codex", Model: model,
		Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: body},
	})
}

func codexHealth(t *testing.T, auth *Auth) map[string]any {
	t.Helper()
	health, ok := auth.Metadata[CodexOAuthHealthMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("no codex_oauth_health in metadata: %#v", auth.Metadata)
	}
	return health
}

const codexExpiredBody = `{"error":{"message":"Provided authentication token is expired. Please try signing in again.","type":"invalid_request_error","code":"token_expired"}}`

func TestCodexOAuthExpiredTokenRefreshesInsteadOfParkingForHalfAnHour(t *testing.T) {
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, codexOAuthAccount("codex-1", "stable-refresh-token"))

	before := time.Now()
	codex401(m, "codex-1", "gpt-5-codex", codexExpiredBody)

	got, _ := m.GetByID("codex-1")
	if got.Status != StatusActive {
		t.Fatalf("auth.Status = %q, want active: an expired token is not an error", got.Status)
	}
	if got.Metadata["access_token"] != "old-access-token" || got.Metadata["refresh_token"] != "stable-refresh-token" {
		t.Fatalf("tokens were touched: %#v", got.Metadata)
	}
	state := got.ModelStates["gpt-5-codex"]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Status != StatusActive || state.StatusMessage != codexOAuthRefreshPendingMessage {
		t.Fatalf("model status = %q / %q, want active / %q", state.Status, state.StatusMessage, codexOAuthRefreshPendingMessage)
	}
	// Short, not the 30 minutes any other 401 starts.
	if wait := state.NextRetryAfter.Sub(before); wait < codexOAuthRefreshCooldown-time.Second || wait > codexOAuthRefreshCooldown+3*time.Second {
		t.Fatalf("cooldown = %v, want about %v", wait, codexOAuthRefreshCooldown)
	}
	health := codexHealth(t, got)
	if health["status"] != codexOAuthHealthStatusRefreshPending || health["temporary_unschedulable_reason"] != codexOAuthReasonTokenExpired {
		t.Fatalf("health = %#v, want refresh_pending / token_expired", health)
	}
	if health["last_401_code"] != "token_expired" || health["refresh_available"] != true {
		t.Fatalf("health = %#v, want the code and refresh_available recorded", health)
	}
	if !m.shouldRefresh(got, time.Now()) {
		t.Fatal("shouldRefresh = false: the refresh loop must pick the account up")
	}
	if reason := CooldownReasonOf(state.LastError, state.Quota); reason != CooldownReasonTokenRefreshing {
		t.Fatalf("reason = %q, want %q", reason, CooldownReasonTokenRefreshing)
	}
}

// The refresh loop's own backoff decides when the next refresh may start;
// retrying before that only draws the same 401.
func TestCodexOAuthExpiredTokenWaitsForTheRefreshBackoff(t *testing.T) {
	cases := []struct {
		name        string
		nextRefresh time.Duration
		want        time.Duration
	}{
		{"no backoff", 0, codexOAuthRefreshCooldown},
		{"backoff shorter than the cooldown", 10 * time.Second, codexOAuthRefreshCooldown},
		{"backoff longer than the cooldown", 5 * time.Minute, 5 * time.Minute},
		{"backoff beyond the reauth cooldown is capped", 3 * time.Hour, codexOAuthReauthCooldown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			account := codexOAuthAccount("codex-1", "stable-refresh-token")
			if tc.nextRefresh > 0 {
				account.NextRefreshAfter = time.Now().Add(tc.nextRefresh)
			}
			registerAuth(t, m, account)
			before := time.Now()
			codex401(m, "codex-1", "gpt-5-codex", codexExpiredBody)
			got, _ := m.GetByID("codex-1")
			wait := got.ModelStates["gpt-5-codex"].NextRetryAfter.Sub(before)
			if wait < tc.want-2*time.Second || wait > tc.want+3*time.Second {
				t.Fatalf("cooldown = %v, want about %v", wait, tc.want)
			}
			// The schedule of the refresh itself is not ours to move.
			if tc.nextRefresh > 0 && got.NextRefreshAfter.IsZero() {
				t.Fatal("the refresh backoff was cleared")
			}
		})
	}
}

func TestCodexOAuthExpiredTokenWithoutARefreshTokenNeedsAReLogin(t *testing.T) {
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, codexOAuthAccount("codex-1", ""))
	before := time.Now()
	codex401(m, "codex-1", "gpt-5-codex", codexExpiredBody)

	got, _ := m.GetByID("codex-1")
	if got.Status != StatusError {
		t.Fatalf("auth.Status = %q, want error: nothing can renew this token", got.Status)
	}
	state := got.ModelStates["gpt-5-codex"]
	if wait := state.NextRetryAfter.Sub(before); wait < codexOAuthReauthCooldown-time.Second || wait > codexOAuthReauthCooldown+3*time.Second {
		t.Fatalf("cooldown = %v, want about %v", wait, codexOAuthReauthCooldown)
	}
	health := codexHealth(t, got)
	if health["status"] != codexOAuthHealthStatusReauthRequired || health["reauth_reason"] != codexOAuthReasonExpiredNoRefresh {
		t.Fatalf("health = %#v, want reauth_required / %s", health, codexOAuthReasonExpiredNoRefresh)
	}
	if health["last_401_code"] != "token_expired" {
		t.Fatalf("the code the upstream sent is not kept: %#v", health)
	}
	if m.shouldRefresh(got, time.Now()) {
		t.Fatal("shouldRefresh = true with no refresh token to spend")
	}
	// The panel must not say "renewing" for an account that cannot renew.
	if reason := CooldownReasonOf(state.LastError, state.Quota); reason != CooldownReasonReauthRequired {
		t.Fatalf("reason = %q, want %q", reason, CooldownReasonReauthRequired)
	}
}

func TestCodexOAuthRevokedCredentialsNeedAReLogin(t *testing.T) {
	for _, code := range []string{"token_revoked", "invalid_grant", "refresh_token_reused", "account_deactivated"} {
		t.Run(code, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, codexOAuthAccount("codex-1", "stable-refresh-token"))
			before := time.Now()
			codex401(m, "codex-1", "gpt-5-codex", `{"error":{"code":"`+code+`","message":"credential is dead"}}`)

			got, _ := m.GetByID("codex-1")
			if got.Status != StatusError {
				t.Fatalf("auth.Status = %q, want error", got.Status)
			}
			if got.Metadata["refresh_token"] != "stable-refresh-token" {
				t.Fatalf("refresh token was touched: %#v", got.Metadata)
			}
			state := got.ModelStates["gpt-5-codex"]
			if wait := state.NextRetryAfter.Sub(before); wait < codexOAuthReauthCooldown-time.Second || wait > codexOAuthReauthCooldown+3*time.Second {
				t.Fatalf("cooldown = %v, want about %v", wait, codexOAuthReauthCooldown)
			}
			health := codexHealth(t, got)
			if health["status"] != codexOAuthHealthStatusReauthRequired || health["reauth_reason"] != code {
				t.Fatalf("health = %#v, want reauth_required / %s", health, code)
			}
			if m.shouldRefresh(got, time.Now()) {
				t.Fatalf("shouldRefresh = true: refreshing cannot help a %s credential", code)
			}
			if reason := CooldownReasonOf(state.LastError, state.Quota); reason != CooldownReasonReauthRequired {
				t.Fatalf("reason = %q, want %q", reason, CooldownReasonReauthRequired)
			}
		})
	}
}

// A 401 whose body is not one of the known codes keeps the handling every other
// 401 gets: a thirty minute park with no health entry.
func TestCodexOAuthUnknownUnauthorizedKeepsTheGenericPark(t *testing.T) {
	bodies := map[string]string{
		"unknown code":    `{"error":{"code":"invalid_api_key","message":"Incorrect API key"}}`,
		"plain text":      "Unauthorized",
		"empty":           "",
		"known word only": `{"error":{"message":"token_expired somewhere in prose"}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, codexOAuthAccount("codex-1", "stable-refresh-token"))
			before := time.Now()
			codex401(m, "codex-1", "gpt-5-codex", body)

			got, _ := m.GetByID("codex-1")
			if _, ok := got.Metadata[CodexOAuthHealthMetadataKey]; ok {
				t.Fatalf("a health entry was written for an unrecognised 401: %#v", got.Metadata)
			}
			state := got.ModelStates["gpt-5-codex"]
			if wait := state.NextRetryAfter.Sub(before); wait < 30*time.Minute-time.Second || wait > 30*time.Minute+3*time.Second {
				t.Fatalf("cooldown = %v, want the generic 30 minutes", wait)
			}
			if state.StatusMessage == codexOAuthRefreshPendingMessage || codexOAuthRefreshPending(got) {
				t.Fatal("an unrecognised 401 was treated as a pending refresh")
			}
			if reason := CooldownReasonOf(state.LastError, state.Quota); reason != CooldownReasonUnauthorized {
				t.Fatalf("reason = %q, want %q", reason, CooldownReasonUnauthorized)
			}
		})
	}
}

func TestCodexOAuthHealthOnlyAppliesToCodexOAuthAccounts(t *testing.T) {
	t.Run("api key account", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		account := &Auth{ID: "codex-key", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "sk-test"}}
		registerAuth(t, m, account)
		codex401(m, "codex-key", "gpt-5-codex", codexExpiredBody)
		got, _ := m.GetByID("codex-key")
		if _, ok := got.Metadata[CodexOAuthHealthMetadataKey]; ok {
			t.Fatal("an API key account got an OAuth health entry")
		}
		if state := got.ModelStates["gpt-5-codex"]; state.StatusMessage == codexOAuthRefreshPendingMessage {
			t.Fatal("an API key account was put on the refresh path")
		}
	})
	t.Run("another provider", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, &Auth{ID: "other", Provider: "openai-compatibility", Status: StatusActive, Attributes: map[string]string{"base_url": "https://x.example/v1"}})
		fail(m, "other", "m", &Error{HTTPStatus: 401, Message: codexExpiredBody})
		got, _ := m.GetByID("other")
		if _, ok := got.Metadata[CodexOAuthHealthMetadataKey]; ok {
			t.Fatal("a non-Codex account got a Codex health entry")
		}
	})
	t.Run("a non-401 failure", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, codexOAuthAccount("codex-1", "stable-refresh-token"))
		m.MarkResult(context.Background(), Result{AuthID: "codex-1", Provider: "codex", Model: "gpt-5-codex",
			Error: &Error{HTTPStatus: 500, Message: codexExpiredBody}})
		got, _ := m.GetByID("codex-1")
		if _, ok := got.Metadata[CodexOAuthHealthMetadataKey]; ok {
			t.Fatal("a 500 whose body names an expired token wrote a health entry")
		}
	})
}

func TestCodexOAuthHealthWithoutAModelParksTheAccount(t *testing.T) {
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, codexOAuthAccount("codex-1", "stable-refresh-token"))
	before := time.Now()
	codex401(m, "codex-1", "", codexExpiredBody)
	got, _ := m.GetByID("codex-1")
	if !got.Unavailable || got.Status != StatusActive {
		t.Fatalf("auth unavailable=%v status=%q, want unavailable and active", got.Unavailable, got.Status)
	}
	if wait := got.NextRetryAfter.Sub(before); wait < codexOAuthRefreshCooldown-time.Second || wait > codexOAuthRefreshCooldown+3*time.Second {
		t.Fatalf("cooldown = %v, want about %v", wait, codexOAuthRefreshCooldown)
	}
	if health := codexHealth(t, got); health["status"] != codexOAuthHealthStatusRefreshPending {
		t.Fatalf("health = %#v", health)
	}
}

// Coming back from refresh_pending: a refresh that worked, or one successful
// request, ends the state; so does an operator's reset.
func TestCodexOAuthHealthRecovers(t *testing.T) {
	markPending := func(t *testing.T) (*Manager, *Auth) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, codexOAuthAccount("codex-1", "stable-refresh-token"))
		codex401(m, "codex-1", "gpt-5-codex", codexExpiredBody)
		got, _ := m.GetByID("codex-1")
		if codexHealth(t, got)["status"] != codexOAuthHealthStatusRefreshPending {
			t.Fatal("setup: not pending")
		}
		return m, got
	}
	assertRecovered := func(t *testing.T, auth *Auth) {
		t.Helper()
		health := codexHealth(t, auth)
		if health["status"] != codexOAuthHealthStatusActive {
			t.Fatalf("health = %#v, want active", health)
		}
		for _, key := range []string{"temporary_unschedulable_until", "temporary_unschedulable_reason", "reauth_reason"} {
			if _, ok := health[key]; ok {
				t.Fatalf("health still carries %s: %#v", key, health)
			}
		}
		if codexOAuthRefreshPending(auth) {
			t.Fatal("still waiting for a refresh")
		}
	}

	t.Run("refresh succeeds", func(t *testing.T) {
		m, pending := markPending(t)
		refreshed := pending.Clone()
		refreshed.Metadata["access_token"] = "new-access-token"
		m.RegisterExecutor(&stubExecutor{id: "codex", refreshOut: refreshed})
		m.refreshAuth(context.Background(), "codex-1")
		got, _ := m.GetByID("codex-1")
		if got.Metadata["access_token"] != "new-access-token" {
			t.Fatalf("refresh result was not stored: %#v", got.Metadata)
		}
		assertRecovered(t, got)
		if codexHealth(t, got)["last_refresh_at"] == nil {
			t.Fatal("the refresh time was not recorded")
		}
	})
	t.Run("a request succeeds", func(t *testing.T) {
		m, _ := markPending(t)
		m.MarkResult(context.Background(), Result{AuthID: "codex-1", Provider: "codex", Model: "gpt-5-codex", Success: true})
		got, _ := m.GetByID("codex-1")
		assertRecovered(t, got)
	})
	t.Run("operator clears the account", func(t *testing.T) {
		m, _ := markPending(t)
		if changed, err := m.ClearQuotaStatus(context.Background(), "codex-1"); err != nil || !changed {
			t.Fatalf("ClearQuotaStatus = %v, %v", changed, err)
		}
		got, _ := m.GetByID("codex-1")
		assertRecovered(t, got)
		// The diagnostics stay: only the part that schedules is reset.
		if codexHealth(t, got)["last_401_code"] != "token_expired" {
			t.Fatalf("diagnostics were erased: %#v", codexHealth(t, got))
		}
	})
	t.Run("an account that never failed gets no entry from a success", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, codexOAuthAccount("codex-1", "stable-refresh-token"))
		m.MarkResult(context.Background(), Result{AuthID: "codex-1", Provider: "codex", Model: "gpt-5-codex", Success: true})
		got, _ := m.GetByID("codex-1")
		if _, ok := got.Metadata[CodexOAuthHealthMetadataKey]; ok {
			t.Fatalf("a healthy account was given a health entry: %#v", got.Metadata)
		}
	})
}

// The health entry is runtime state, written by failures and read by the
// scheduler; a credential file reload must not discard it or carry an old one
// over a re-login.
func TestCodexOAuthHealthIsRuntimeMetadata(t *testing.T) {
	found := false
	for _, key := range runtimeMetadataKeys {
		found = found || key == CodexOAuthHealthMetadataKey
	}
	if !found {
		t.Fatalf("%s is not in runtimeMetadataKeys, so a reload from the credential file would drop it", CodexOAuthHealthMetadataKey)
	}
}
