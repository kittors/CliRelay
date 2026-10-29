package authfiles

import (
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// cooldown_reason says why something is held back, so it appears exactly when a
// restriction is holding something back: an error with nothing left to wait for
// is still listed, but nothing is cooling down.
func TestBuildRestrictionPayloadCarriesTheCooldownReason(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	later := now.Add(10 * time.Minute)
	cases := []struct {
		name  string
		state *coreauth.ModelState
		want  string
	}{
		{
			name: "throttled",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: later,
				LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit"},
			},
			want: coreauth.CooldownReasonRateLimited,
		},
		{
			name: "allowance used up",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: later,
				LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "daily quota exceeded"},
				Quota:     coreauth.QuotaState{Exceeded: true, NextRecoverAt: later},
			},
			want: coreauth.CooldownReasonQuotaExhausted,
		},
		{
			name: "allowance waiting for a probe",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true,
				LastError: &coreauth.Error{HTTPStatus: http.StatusPaymentRequired},
				Quota:     coreauth.QuotaState{Exceeded: true, RecoveryRequired: true},
			},
			want: coreauth.CooldownReasonQuotaExhausted,
		},
		{
			name: "rejected credential",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: later,
				LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid api key"},
			},
			want: coreauth.CooldownReasonUnauthorized,
		},
		{
			name: "token being renewed",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: now.Add(30 * time.Second),
				LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Code: "token_expired"},
			},
			want: coreauth.CooldownReasonTokenRefreshing,
		},
		{
			name: "dead credential",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: later,
				LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Code: "token_revoked"},
			},
			want: coreauth.CooldownReasonReauthRequired,
		},
		{
			name: "upstream overloaded",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: later,
				LastError: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable},
			},
			want: coreauth.CooldownReasonUpstreamError,
		},
		{
			name: "model missing upstream",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: later,
				LastError: &coreauth.Error{HTTPStatus: http.StatusNotFound, Message: `{"error":{"code":"model_not_found"}}`},
			},
			want: coreauth.CooldownReasonModelUnavailable,
		},
		{
			// The error is on record but its wait is over: shown, not cooling.
			name: "wait already over",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, NextRetryAfter: now.Add(-time.Minute),
				LastError: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable},
			},
			want: "",
		},
		{
			// An error with no wait attached: nothing is held back.
			name: "error without a wait",
			state: &coreauth.ModelState{
				Status: coreauth.StatusError, Unavailable: true,
				LastError: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &coreauth.Auth{ModelStates: map[string]*coreauth.ModelState{"gpt-5": tc.state}}
			restrictions := BuildRestrictionPayload(auth, now)
			if len(restrictions) != 1 {
				t.Fatalf("restrictions = %#v, want one", restrictions)
			}
			got, present := restrictions[0]["cooldown_reason"]
			if tc.want == "" {
				if present {
					t.Fatalf("cooldown_reason = %v, want it absent: %#v", got, restrictions[0])
				}
				return
			}
			if got != tc.want {
				t.Fatalf("cooldown_reason = %v, want %q: %#v", got, tc.want, restrictions[0])
			}
		})
	}
}

// The account-level line carries the reason too, so a panel that only reads the
// first restriction still explains the badge.
func TestBuildRestrictionPayloadCarriesTheReasonForTheAccountScope(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
		LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit"},
	}
	restrictions := BuildRestrictionPayload(auth, now)
	if len(restrictions) != 1 || restrictions[0]["scope"] != "auth" || restrictions[0]["cooldown_reason"] != coreauth.CooldownReasonRateLimited {
		t.Fatalf("restrictions = %#v, want one auth-scope line with the rate_limited reason", restrictions)
	}
}

func TestBuildModelCooldownPayload(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	until := now.Add(20 * time.Minute)
	auth := &coreauth.Auth{ModelStates: map[string]*coreauth.ModelState{
		"b-overloaded": {
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: until,
			LastError: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable},
		},
		"a-quota": {
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: until,
			LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "daily quota exceeded"},
			Quota:     coreauth.QuotaState{Exceeded: true, NextRecoverAt: until},
		},
		"c-without-status": {
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: until,
			LastError: &coreauth.Error{Message: "dial tcp: connection refused"},
		},
		"d-healthy": {Status: coreauth.StatusActive},
		"e-elapsed": {
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: now.Add(-time.Minute),
			LastError: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable},
		},
	}}

	got := BuildModelCooldownPayload(auth, now)
	if len(got) != 3 {
		t.Fatalf("payload = %#v, want the three models still cooling", got)
	}
	// One line per model, in model order: a per-model action needs every model,
	// not the de-duplicated restriction summary.
	for i, model := range []string{"a-quota", "b-overloaded", "c-without-status"} {
		if got[i]["model"] != model {
			t.Fatalf("payload[%d].model = %v, want %s: %#v", i, got[i]["model"], model, got)
		}
		if until, ok := got[i]["until"].(time.Time); !ok || !until.Equal(now.Add(20*time.Minute)) {
			t.Fatalf("payload[%d].until = %#v", i, got[i]["until"])
		}
	}
	if got[0]["reason"] != coreauth.CooldownReasonQuotaExhausted || got[0]["quota_exceeded"] != true || got[0]["http_status"] != http.StatusTooManyRequests {
		t.Fatalf("quota line = %#v", got[0])
	}
	if got[1]["reason"] != coreauth.CooldownReasonUpstreamError || got[1]["quota_exceeded"] != false || got[1]["http_status"] != http.StatusServiceUnavailable {
		t.Fatalf("overload line = %#v", got[1])
	}
	// A transport error has no status and no reason code to name; the model is
	// still held back and can still be released.
	if _, ok := got[2]["reason"]; ok {
		t.Fatalf("a failure without a status invented a reason: %#v", got[2])
	}
	if _, ok := got[2]["http_status"]; ok {
		t.Fatalf("a failure without a status reports one: %#v", got[2])
	}

	if BuildModelCooldownPayload(nil, now) != nil || BuildModelCooldownPayload(&coreauth.Auth{}, now) != nil {
		t.Fatal("no auth or no cooling model must list nothing")
	}
	healthy := &coreauth.Auth{ModelStates: map[string]*coreauth.ModelState{"m": {Status: coreauth.StatusActive}}}
	if BuildModelCooldownPayload(healthy, now) != nil {
		t.Fatal("a healthy account must list nothing")
	}
}

func TestBuildEntryIncludesModelCooldowns(t *testing.T) {
	now := time.Now()
	cooling := &coreauth.Auth{
		ID: "cooling", Provider: "codex", Attributes: map[string]string{"runtime_only": "true"},
		ModelStates: map[string]*coreauth.ModelState{"gpt-5": {
			Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
			LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit"},
		}},
	}
	entry := BuildEntry(cooling, EntryOptions{})
	cooldowns, ok := entry["model_cooldowns"].([]map[string]any)
	if !ok || len(cooldowns) != 1 || cooldowns[0]["model"] != "gpt-5" || cooldowns[0]["reason"] != coreauth.CooldownReasonRateLimited {
		t.Fatalf("model_cooldowns = %#v", entry["model_cooldowns"])
	}

	healthy := &coreauth.Auth{ID: "healthy", Provider: "codex", Attributes: map[string]string{"runtime_only": "true"}}
	if _, present := BuildEntry(healthy, EntryOptions{})["model_cooldowns"]; present {
		t.Fatal("a healthy account must not carry model_cooldowns")
	}
}

// The Codex sign-in health is shown the way Claude's is: what the scheduler
// decided, never the credential.
func TestBuildEntryIncludesSanitizedCodexOAuthHealth(t *testing.T) {
	auth := &coreauth.Auth{
		ID:         "codex-oauth",
		Provider:   "codex",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata: map[string]any{
			"email":         "codex@example.com",
			"access_token":  "metadata-access-token",
			"refresh_token": "metadata-refresh-token",
			coreauth.CodexOAuthHealthMetadataKey: map[string]any{
				"status":                        "reauth_required",
				"reauth_reason":                 "token_revoked",
				"last_401_code":                 "token_revoked",
				"temporary_unschedulable_until": "2026-10-30T12:30:00Z",
				"access_token":                  "health-access-token",
				"refresh_token":                 "health-refresh-token",
				"secret":                        "hidden",
			},
		},
	}
	entry := BuildEntry(auth, EntryOptions{})
	if entry == nil {
		t.Fatal("expected entry")
	}
	health, ok := entry["codex_oauth_health"].(map[string]any)
	if !ok {
		t.Fatalf("codex_oauth_health = %#v, want map", entry["codex_oauth_health"])
	}
	if health["status"] != "reauth_required" || health["reauth_reason"] != "token_revoked" || health["last_401_code"] != "token_revoked" {
		t.Fatalf("health = %#v", health)
	}
	if health["temporary_unschedulable_until"] != "2026-10-30T12:30:00Z" {
		t.Fatalf("health lost the retry time: %#v", health)
	}
	for _, leaked := range []string{"access_token", "refresh_token", "secret"} {
		if _, present := health[leaked]; present {
			t.Fatalf("health leaked %s: %#v", leaked, health)
		}
	}
	if _, present := entry["claude_oauth_health"]; present {
		t.Fatal("a Codex account reports Claude health")
	}

	plain := &coreauth.Auth{ID: "plain", Provider: "codex", Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{"email": "x@example.com"}}
	if _, present := BuildEntry(plain, EntryOptions{})["codex_oauth_health"]; present {
		t.Fatal("an account that never failed must not carry codex_oauth_health")
	}
	if CodexOAuthHealth(nil) != nil || CodexOAuthHealth(&coreauth.Auth{}) != nil {
		t.Fatal("no auth or no metadata must give no health")
	}
}
