package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestClaudeOAuthRateLimitHeaders_ParseWindowsAndPreferSevenDay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 23, 8, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(2 * time.Hour).Unix()
	sevenDayReset := now.Add(72*time.Hour).Unix() * 1000
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":              []string{"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":               []string{formatInt64(fiveHourReset)},
		"Anthropic-Ratelimit-Unified-5h-Utilization":         []string{"1.02"},
		"Anthropic-Ratelimit-Unified-7d-Status":              []string{"allowed_warning"},
		"Anthropic-Ratelimit-Unified-7d-Reset":               []string{formatInt64(sevenDayReset)},
		"Anthropic-Ratelimit-Unified-7d-Surpassed-Threshold": []string{"true"},
	}

	windows, decision := parseClaudeOAuthRateLimitHeaders(headers, now)
	if decision == nil {
		t.Fatal("decision = nil, want seven day exhaustion")
	}
	if decision.window != "7d" || decision.minutes != 10080 || decision.reason != claudeOAuthReasonAnthropic7D {
		t.Fatalf("decision = %#v, want seven day decision", decision)
	}
	if !decision.resetAt.Equal(now.Add(72 * time.Hour)) {
		t.Fatalf("decision.resetAt = %v, want %v", decision.resetAt, now.Add(72*time.Hour))
	}
	fiveHour, ok := windows["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("five_hour window missing in %#v", windows)
	}
	if fiveHour["status"] != "rejected" || fiveHour["exceeded"] != true || fiveHour["utilization"] != 1.02 {
		t.Fatalf("five_hour = %#v, want rejected/exceeded/utilization", fiveHour)
	}
}

func TestClaudeOAuthRateLimitHeaders_RejectsOutOfRangeReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 23, 8, 0, 0, 0, time.UTC)
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":  []string{formatInt64(now.Add(7 * time.Hour).Unix())},
	}

	windows, decision := parseClaudeOAuthRateLimitHeaders(headers, now)
	if decision != nil {
		t.Fatalf("decision = %#v, want nil for out-of-range reset", decision)
	}
	fiveHour, ok := windows["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("five_hour window missing in %#v", windows)
	}
	if _, ok := fiveHour["reset_at"]; ok {
		t.Fatalf("reset_at = %v, want omitted for out-of-range reset", fiveHour["reset_at"])
	}
}

func TestManagerMarkResult_ClaudeOAuth401WithRefreshTokenIsTemporaryAndRefreshable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	model := "claude-sonnet-4-5"
	auth := &Auth{
		ID:       "claude-oauth",
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "claude@example.com",
			"access_token":  "old-access-token",
			"refresh_token": "stable-refresh-token",
		},
	}
	if _, err := m.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	before := time.Now()
	m.MarkResult(ctx, Result{
		AuthID:   "claude-oauth",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid oauth token"},
	})

	got, ok := m.GetByID("claude-oauth")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got.Status != StatusActive {
		t.Fatalf("auth.Status = %q, want active", got.Status)
	}
	if !got.Unavailable {
		t.Fatal("auth.Unavailable = false, want true while OAuth refresh is pending")
	}
	if got.Metadata["refresh_token"] != "stable-refresh-token" || got.Metadata["access_token"] != "old-access-token" {
		t.Fatalf("tokens were mutated: %#v", got.Metadata)
	}
	if !m.shouldRefresh(got, time.Now()) {
		t.Fatal("shouldRefresh = false, want true for claude_oauth refresh_pending")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Status != StatusActive || state.StatusMessage != claudeOAuthRefreshPendingMessage {
		t.Fatalf("model status = %q/%q, want active/%q", state.Status, state.StatusMessage, claudeOAuthRefreshPendingMessage)
	}
	if state.NextRetryAfter.Before(before.Add(claudeOAuth401Cooldown-time.Second)) ||
		state.NextRetryAfter.After(before.Add(claudeOAuth401Cooldown+time.Second)) {
		t.Fatalf("NextRetryAfter = %v, want about %v", state.NextRetryAfter, before.Add(claudeOAuth401Cooldown))
	}
	health := mustClaudeOAuthHealth(t, got)
	if health["status"] != claudeOAuthHealthStatusRefreshPending {
		t.Fatalf("health.status = %v, want refresh_pending", health["status"])
	}
	if health["refresh_available"] != true || health["temporary_unschedulable_reason"] != claudeOAuthReasonOAuth401 {
		t.Fatalf("health = %#v, want refresh_available and oauth_401 reason", health)
	}
	if _, ok := health["access_token"]; ok {
		t.Fatalf("health leaked access_token: %#v", health)
	}
	if _, ok := health["refresh_token"]; ok {
		t.Fatalf("health leaked refresh_token: %#v", health)
	}
}

func TestManagerMarkResult_ClaudeOAuth401WithoutRefreshTokenFallsBackToUnauthorized(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	model := "claude-sonnet-4-5"
	if _, err := m.Register(ctx, &Auth{
		ID:       "claude-oauth",
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":         "claude",
			"email":        "claude@example.com",
			"access_token": "old-access-token",
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	m.MarkResult(ctx, Result{
		AuthID:   "claude-oauth",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid oauth token"},
	})

	got, ok := m.GetByID("claude-oauth")
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Status != StatusError || state.StatusMessage != "invalid oauth token" {
		t.Fatalf("model status = %q/%q, want generic unauthorized error", state.Status, state.StatusMessage)
	}
	if state.NextRetryAfter.Before(time.Now().Add(29 * time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want generic 30m unauthorized cooldown", state.NextRetryAfter)
	}
	health := mustClaudeOAuthHealth(t, got)
	if health["status"] != "unauthorized" || health["refresh_available"] != false {
		t.Fatalf("health = %#v, want unauthorized without refresh", health)
	}
}

func TestManagerMarkResult_ClaudeOAuth429UsesAnthropicFiveHourReset(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	model := "claude-sonnet-4-5"
	if _, err := m.Register(ctx, newClaudeOAuthTestAuth("claude-oauth")); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	resetAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":      []string{"rejected"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       []string{formatInt64(resetAt.Unix())},
		"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"1.00"},
	}

	m.MarkResult(ctx, Result{
		AuthID:   "claude-oauth",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Headers:  headers,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
	})

	got, ok := m.GetByID("claude-oauth")
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Quota.Window != "5h" || state.Quota.WindowMinutes != 300 {
		t.Fatalf("quota = %#v, want 5h/300", state.Quota)
	}
	if !state.NextRetryAfter.Equal(resetAt) || !state.Quota.NextRecoverAt.Equal(resetAt) {
		t.Fatalf("recover = %v/%v, want %v", state.NextRetryAfter, state.Quota.NextRecoverAt, resetAt)
	}
	health := mustClaudeOAuthHealth(t, got)
	if health["status"] != claudeOAuthHealthStatusExhausted || health["temporary_unschedulable_reason"] != claudeOAuthReasonAnthropic5H {
		t.Fatalf("health = %#v, want exhausted 5h", health)
	}
}

func TestManagerMarkResult_ClaudeOAuth429FallsBackWhenNoUsableAnthropicReset(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	model := "claude-sonnet-4-5"
	if _, err := m.Register(ctx, newClaudeOAuthTestAuth("claude-oauth")); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"},
	}

	m.MarkResult(ctx, Result{
		AuthID:   "claude-oauth",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Headers:  headers,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
	})

	got, ok := m.GetByID("claude-oauth")
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Quota.Window != "" || state.Quota.WindowMinutes != 0 {
		t.Fatalf("quota = %#v, want generic quota without Anthropic window", state.Quota)
	}
	health := mustClaudeOAuthHealth(t, got)
	if health["status"] != claudeOAuthHealthStatusRateLimited {
		t.Fatalf("health.status = %v, want rate_limited", health["status"])
	}
}

func TestManagerMarkResult_ClaudeOAuthSuccessClearsTemporaryHealth(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	model := "claude-sonnet-4-5"
	if _, err := m.Register(ctx, newClaudeOAuthTestAuth("claude-oauth")); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	m.MarkResult(ctx, Result{
		AuthID:   "claude-oauth",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid oauth token"},
	})
	m.MarkResult(ctx, Result{
		AuthID:   "claude-oauth",
		Provider: "claude",
		Model:    model,
		Success:  true,
	})

	got, ok := m.GetByID("claude-oauth")
	if !ok {
		t.Fatal("updated auth missing")
	}
	health := mustClaudeOAuthHealth(t, got)
	if health["status"] != claudeOAuthHealthStatusActive {
		t.Fatalf("health.status = %v, want active", health["status"])
	}
	if _, ok := health["temporary_unschedulable_until"]; ok {
		t.Fatalf("temporary_unschedulable_until still present in %#v", health)
	}
	if state := got.ModelStates[model]; state == nil || state.Unavailable {
		t.Fatalf("model state = %#v, want available after success", state)
	}
}

func newClaudeOAuthTestAuth(id string) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "claude@example.com",
			"access_token":  "old-access-token",
			"refresh_token": "stable-refresh-token",
		},
	}
}

func mustClaudeOAuthHealth(t *testing.T, auth *Auth) map[string]any {
	t.Helper()
	health, ok := auth.Metadata[ClaudeOAuthHealthMetadataKey].(map[string]any)
	if !ok || len(health) == 0 {
		t.Fatalf("health missing in %#v", auth.Metadata)
	}
	return health
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
