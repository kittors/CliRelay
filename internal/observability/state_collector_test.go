package observability

import (
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAccountStateClassification(t *testing.T) {
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	earlier := now.Add(-time.Hour)
	cases := []struct {
		name string
		auth coreauth.Auth
		want string
	}{
		{"active", coreauth.Auth{Provider: "codex", Status: coreauth.StatusActive}, accountStateActive},
		{"disabled flag", coreauth.Auth{Provider: "codex", Disabled: true}, accountStateDisabled},
		{"disabled status", coreauth.Auth{Provider: "codex", Status: coreauth.StatusDisabled}, accountStateDisabled},
		{"quota until later", coreauth.Auth{Provider: "codex", Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: later}}, accountStateCooling},
		// A recovery time that has passed holds nothing back any more.
		{"quota recovered", coreauth.Auth{Provider: "codex", Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: earlier}}, accountStateActive},
		{"account backoff", coreauth.Auth{Provider: "claude", Unavailable: true, NextRetryAfter: later}, accountStateCooling},
		{"backoff over", coreauth.Auth{Provider: "claude", Unavailable: true, NextRetryAfter: earlier}, accountStateActive},
		{"error", coreauth.Auth{Provider: "gemini-cli", Status: coreauth.StatusError}, accountStateError},
		// Antigravity's quota pools are per model family; its account flag
		// does not stop the other pool from serving.
		{"antigravity pool", coreauth.Auth{Provider: "antigravity", Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: later}}, accountStateActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := tc.auth
			if got := accountState(&auth, now); got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStateCollectorReportsEveryStateAndModelCooldowns(t *testing.T) {
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	later := now.Add(30 * time.Minute)
	auths := []*coreauth.Auth{
		{Provider: "codex", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{
			"gpt-5.5":      {Unavailable: true, NextRetryAfter: later, LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests}},
			"gpt-5.5-mini": {Unavailable: true, NextRetryAfter: later, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: later}},
		}},
		{Provider: "codex", Status: coreauth.StatusActive},
		// Held back as a whole: its model states would report the same outage
		// a second time.
		{Provider: "codex", Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: later}, ModelStates: map[string]*coreauth.ModelState{
			"gpt-5.5": {Unavailable: true, NextRetryAfter: later, LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests}},
		}},
		{Provider: "claude", Disabled: true},
		nil,
	}
	e := NewExporter()
	e.now = func() time.Time { return now }
	e.SetSources(Sources{Auths: func() []*coreauth.Auth { return auths }, InFlight: func() int64 { return 7 }})

	want := map[[2]string]float64{
		{"codex", accountStateActive}:    2,
		{"codex", accountStateCooling}:   1,
		{"codex", accountStateError}:     0,
		{"codex", accountStateDisabled}:  0,
		{"claude", accountStateActive}:   0,
		{"claude", accountStateCooling}:  0,
		{"claude", accountStateError}:    0,
		{"claude", accountStateDisabled}: 1,
	}
	if got := gather(t, e, "clirelay_accounts"); len(got) != len(want) {
		t.Fatalf("account series = %v, want %d", describe(got), len(want))
	}
	for key, count := range want {
		if got := value(t, e, "clirelay_accounts", map[string]string{"provider": key[0], "state": key[1]}).value; got != count {
			t.Fatalf("accounts{%s,%s} = %v, want %v", key[0], key[1], got, count)
		}
	}
	if got := value(t, e, "clirelay_account_model_cooldowns", map[string]string{"provider": "codex", "reason": coreauth.CooldownReasonRateLimited}).value; got != 1 {
		t.Fatalf("rate-limited model cooldowns = %v, want 1 (the cooling account's must not count)", got)
	}
	if got := value(t, e, "clirelay_account_model_cooldowns", map[string]string{"provider": "codex", "reason": coreauth.CooldownReasonQuotaExhausted}).value; got != 1 {
		t.Fatalf("quota model cooldowns = %v, want 1", got)
	}
	if got := value(t, e, "clirelay_http_inflight_requests", nil).value; got != 7 {
		t.Fatalf("in-flight = %v, want 7", got)
	}
}

func TestStateCollectorWithoutSources(t *testing.T) {
	e := NewExporter()
	for _, name := range []string{"clirelay_accounts", "clirelay_http_inflight_requests"} {
		if got := gather(t, e, name); len(got) != 0 {
			t.Fatalf("%s without sources: %v", name, describe(got))
		}
	}
	// The build is always described.
	value(t, e, "clirelay_build_info", nil)
}
