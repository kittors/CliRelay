package auth

import (
	"testing"
	"time"
)

func TestCooldownReasonOf(t *testing.T) {
	cases := []struct {
		name  string
		err   *Error
		quota QuotaState
		want  string
	}{
		{"an exhausted allowance wins over the status", &Error{HTTPStatus: 429, Message: "daily quota exceeded"}, QuotaState{Exceeded: true}, CooldownReasonQuotaExhausted},
		{"an exhausted allowance with no error recorded", nil, QuotaState{Exceeded: true}, CooldownReasonQuotaExhausted},
		{"throttling", &Error{HTTPStatus: 429, Message: "rate limit"}, QuotaState{}, CooldownReasonRateLimited},
		{"a plain 401", &Error{HTTPStatus: 401, Message: "invalid api key"}, QuotaState{}, CooldownReasonUnauthorized},
		{"an expired Codex token", &Error{HTTPStatus: 401, Code: "token_expired"}, QuotaState{}, CooldownReasonTokenRefreshing},
		{"a revoked Codex token", &Error{HTTPStatus: 401, Code: "token_revoked"}, QuotaState{}, CooldownReasonReauthRequired},
		{"a reused refresh token", &Error{HTTPStatus: 401, Code: "refresh_token_reused"}, QuotaState{}, CooldownReasonReauthRequired},
		{"an expired token that cannot refresh", &Error{HTTPStatus: 401, Code: codexOAuthReasonExpiredNoRefresh}, QuotaState{}, CooldownReasonReauthRequired},
		{"a code of another case", &Error{HTTPStatus: 401, Code: " TOKEN_REVOKED "}, QuotaState{}, CooldownReasonReauthRequired},
		{"payment required", &Error{HTTPStatus: 402}, QuotaState{}, CooldownReasonPaymentRequired},
		{"forbidden", &Error{HTTPStatus: 403}, QuotaState{}, CooldownReasonPaymentRequired},
		{"a 404 naming the model", &Error{HTTPStatus: 404, Message: `{"error":{"code":"model_not_found"}}`}, QuotaState{}, CooldownReasonModelUnavailable},
		{"a bare 404", &Error{HTTPStatus: 404, Message: "not found"}, QuotaState{}, CooldownReasonNotFound},
		{"an empty 404", &Error{HTTPStatus: 404}, QuotaState{}, CooldownReasonNotFound},
		{"timeout", &Error{HTTPStatus: 408}, QuotaState{}, CooldownReasonUpstreamError},
		{"500", &Error{HTTPStatus: 500}, QuotaState{}, CooldownReasonUpstreamError},
		{"503", &Error{HTTPStatus: 503}, QuotaState{}, CooldownReasonUpstreamError},
		{"529", &Error{HTTPStatus: 529}, QuotaState{}, CooldownReasonUpstreamError},
		{"a client error is no cooldown reason", &Error{HTTPStatus: 400}, QuotaState{}, ""},
		{"an error without a status", &Error{Message: "dial tcp: refused"}, QuotaState{}, ""},
		{"nothing recorded", nil, QuotaState{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CooldownReasonOf(tc.err, tc.quota); got != tc.want {
				t.Fatalf("CooldownReasonOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// A 404 is only called "model unavailable" when its message says so. A bare 404
// may be a sporadic routing miss, and calling it a missing model would send an
// operator looking for a model problem that is not there.
func TestIsModelUnavailableMessage(t *testing.T) {
	yes := []string{
		`{"error":{"code":"model_not_found","message":"x"}}`,
		"The model `gpt-9` does not exist or you do not have access to it.",
		"Model not found: glm-9",
		"unknown model glm-9",
		"unsupported model: glm-9",
		"The requested model is not available for your plan.",
		"The model is not supported when using Codex with a ChatGPT account.",
		"This model is not included in your plan",
		"模型不存在",
		"该模型不可用，请联系管理员",
		"您的套餐不包含该模型",
		"无权使用该模型",
	}
	no := []string{
		"",
		"  ",
		"not found",
		"404 page not found",
		"Not Found",
		`{"error":{"message":"route not found"}}`,
		"resource does not exist",          // no model mentioned
		"the endpoint is not available",    // no model mentioned
		"model endpoint temporarily moved", // mentions a model, says nothing about it being missing
		"服务不存在",                            // Chinese, no model
		"请求的资源未找到",
	}
	for _, message := range yes {
		if !isModelUnavailableMessage(message) {
			t.Errorf("%q should read as a missing model", message)
		}
	}
	for _, message := range no {
		if isModelUnavailableMessage(message) {
			t.Errorf("%q must not read as a missing model", message)
		}
	}
}

func TestActiveModelCooldowns(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	auth := &Auth{ModelStates: map[string]*ModelState{
		"b-throttled": {
			Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Minute),
			LastError: &Error{HTTPStatus: 429, Message: "rate limit"},
		},
		"a-quota": {
			Status: StatusError, Unavailable: true, NextRetryAfter: later,
			LastError: &Error{HTTPStatus: 429},
			Quota:     QuotaState{Exceeded: true, NextRecoverAt: later},
		},
		"c-upstream": {
			Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Minute),
			LastError: &Error{HTTPStatus: 503},
		},
		// A quota whose recovery waits for a probe is held back until its deadline,
		// whichever of the two times comes first.
		"d-gated": {
			Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(10 * time.Minute),
			LastError: &Error{HTTPStatus: 402},
			Quota:     QuotaState{Exceeded: true, RecoveryRequired: true, NextRecoverAt: later},
		},
		"e-no-time": {Status: StatusError, Unavailable: true, LastError: &Error{HTTPStatus: 400}},
		"f-elapsed": {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(-time.Minute), LastError: &Error{HTTPStatus: 503}},
		"g-disabled": {
			Status: StatusDisabled, Unavailable: true, NextRetryAfter: later, LastError: &Error{HTTPStatus: 503},
		},
		"h-healthy": {Status: StatusActive},
		"i-nil":     nil,
	}}
	got := ActiveModelCooldowns(auth, now)
	want := []ModelCooldown{
		{Model: "a-quota", Reason: CooldownReasonQuotaExhausted, Until: later, HTTPStatus: 429, QuotaExceeded: true},
		{Model: "b-throttled", Reason: CooldownReasonRateLimited, Until: now.Add(time.Minute), HTTPStatus: 429},
		{Model: "c-upstream", Reason: CooldownReasonUpstreamError, Until: now.Add(time.Minute), HTTPStatus: 503},
		{Model: "d-gated", Reason: CooldownReasonQuotaExhausted, Until: now.Add(10 * time.Minute), HTTPStatus: 402, QuotaExceeded: true},
	}
	if len(got) != len(want) {
		t.Fatalf("ActiveModelCooldowns listed %d models, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if ActiveModelCooldowns(nil, now) != nil || ActiveModelCooldowns(&Auth{}, now) != nil {
		t.Fatal("no auth or no model states must list nothing")
	}
}

// What is listed is exactly what routing skips, so "retry now" is offered for
// every model the scheduler is holding back and for no other.
func TestActiveModelCooldownsAgreeWithTheScheduler(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	fail(m, "relay", "throttled", relayRateLimit)
	fail(m, "relay", "revoked", &Error{HTTPStatus: 401, Message: "invalid api key"})
	fail(m, "relay", "overloaded", &Error{HTTPStatus: 503, Message: "unavailable"})
	fail(m, "relay", "quota", &Error{HTTPStatus: 429, Message: "daily quota exceeded"})
	fail(m, "relay", "bad-request", &Error{HTTPStatus: 400, Message: "bad request"})
	fail(m, "relay", "529", &Error{HTTPStatus: 529, Message: "overloaded"})

	auth, _ := m.GetByID("relay")
	listed := map[string]bool{}
	for _, cooldown := range ActiveModelCooldowns(auth, time.Now()) {
		listed[cooldown.Model] = true
	}
	for model := range auth.ModelStates {
		// A model blocked only because another one exhausted the account's quota
		// is held back by the aggregate, not by its own state; compare the
		// models' own states.
		own := &Auth{ID: auth.ID, Provider: auth.Provider, Status: StatusActive, ModelStates: map[string]*ModelState{model: auth.ModelStates[model]}}
		blocked, _, _ := isAuthBlockedForModel(own, model, time.Now())
		if blocked != listed[model] {
			t.Errorf("%s: scheduler blocks it = %v, ActiveModelCooldowns lists it = %v", model, blocked, listed[model])
		}
	}
	if len(listed) != 4 {
		t.Errorf("listed %v, want throttled, revoked, overloaded and quota", listed)
	}
}
