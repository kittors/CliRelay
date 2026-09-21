package auth

import (
	"testing"
	"time"
)

func TestAntigravityQuotaPoolIsolation_Selector(t *testing.T) {
	now := time.Now()
	recoverAt := now.Add(2 * time.Hour)

	// An Antigravity account where Claude is exhausted, but Gemini has not been exhausted
	auth := &Auth{
		ID:       "antigravity-test-1",
		Provider: "antigravity",
		ModelStates: map[string]*ModelState{
			"claude-3-7-sonnet": {
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: recoverAt,
				},
			},
			"gemini-2.5-pro": {
				Status: StatusActive,
			},
		},
	}

	// Update aggregated availability
	updateAggregatedAvailability(auth, now)

	// Global auth quota exceeded should be FALSE because only Claude family is exhausted, Gemini is fine
	if auth.Quota.Exceeded {
		t.Fatalf("auth.Quota.Exceeded = true, want false because Gemini pool is healthy")
	}
	if auth.Status == StatusError {
		t.Fatalf("auth.Status = StatusError, want StatusActive when not all pools exhausted")
	}

	// Calling claude-3-7-sonnet should be BLOCKED
	blockedClaude, reasonClaude, _ := isAuthBlockedForModel(auth, "claude-3-7-sonnet", now)
	if !blockedClaude {
		t.Errorf("claude-3-7-sonnet should be blocked")
	}
	if reasonClaude != blockReasonCooldown {
		t.Errorf("reasonClaude = %v, want %v", reasonClaude, blockReasonCooldown)
	}

	// Calling another Claude model (e.g. claude-3-5-sonnet) should also be BLOCKED due to same family
	blockedClaudeOther, _, _ := isAuthBlockedForModel(auth, "claude-3-5-sonnet", now)
	if !blockedClaudeOther {
		t.Errorf("claude-3-5-sonnet should be blocked because Claude family pool is exhausted")
	}

	// Calling gemini-2.5-pro should NOT be blocked!
	blockedGemini, reasonGemini, _ := isAuthBlockedForModel(auth, "gemini-2.5-pro", now)
	if blockedGemini {
		t.Errorf("gemini-2.5-pro should NOT be blocked, got reason = %v", reasonGemini)
	}

	// Calling an uninitialized Gemini model (e.g. gemini-2.5-flash) should NOT be blocked!
	blockedGeminiFlash, reasonGeminiFlash, _ := isAuthBlockedForModel(auth, "gemini-2.5-flash", now)
	if blockedGeminiFlash {
		t.Errorf("gemini-2.5-flash should NOT be blocked, got reason = %v", reasonGeminiFlash)
	}
}

func TestAntigravityQuotaPoolIsolation_BothExhausted(t *testing.T) {
	now := time.Now()
	recoverAt := now.Add(2 * time.Hour)

	// An Antigravity account where BOTH Gemini and Claude are exhausted
	auth := &Auth{
		ID:       "antigravity-test-both",
		Provider: "antigravity",
		ModelStates: map[string]*ModelState{
			"claude-3-7-sonnet": {
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: recoverAt,
				},
			},
			"gemini-2.5-pro": {
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: recoverAt,
				},
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	// When both pools are exhausted, auth.Quota.Exceeded SHOULD be true
	if !auth.Quota.Exceeded {
		t.Fatalf("auth.Quota.Exceeded = false, want true because all families are exhausted")
	}

	// Both models should be blocked
	blockedClaude, _, _ := isAuthBlockedForModel(auth, "claude-3-7-sonnet", now)
	if !blockedClaude {
		t.Errorf("claude-3-7-sonnet should be blocked")
	}
	blockedGemini, _, _ := isAuthBlockedForModel(auth, "gemini-2.5-pro", now)
	if !blockedGemini {
		t.Errorf("gemini-2.5-pro should be blocked")
	}
}

// An account that has only ever served Claude has no ModelStates entry for the
// Gemini pool. Treating "no entry" as exhausted made the whole credential report
// unavailable the moment Claude hit its weekly cap, which surfaced in the console
// as an account-wide 429 badge even though the Gemini pool was untouched.
func TestAntigravityQuotaPoolIsolation_ClaudeOnlyStatesKeepAuthAvailable(t *testing.T) {
	now := time.Now()
	recoverAt := now.Add(22 * time.Hour)

	auth := &Auth{
		ID:             "antigravity-test-claude-only",
		Provider:       "antigravity",
		Status:         StatusError,
		StatusMessage:  `{"error":{"code":429,"message":"Individual quota reached."}}`,
		Unavailable:    true,
		NextRetryAfter: recoverAt,
		ModelStates: map[string]*ModelState{
			"claude-sonnet-4.5": {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					Window:        "week",
					NextRecoverAt: recoverAt,
				},
			},
			"gpt-5.1-codex": {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					Window:        "week",
					NextRecoverAt: recoverAt,
				},
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if auth.Unavailable {
		t.Errorf("auth.Unavailable = true, want false: the Gemini pool was never exhausted")
	}
	if auth.Quota.Exceeded {
		t.Errorf("auth.Quota.Exceeded = true, want false: only the Claude/GPT pool is capped")
	}
	if auth.Status == StatusError {
		t.Errorf("auth.Status = StatusError, want StatusActive while the Gemini pool serves")
	}
	if !auth.NextRetryAfter.IsZero() {
		t.Errorf("auth.NextRetryAfter = %v, want zero so the console shows no account-wide countdown", auth.NextRetryAfter)
	}

	// The Claude pool must still be cooling down, and Gemini must still route.
	if blocked, _, _ := isAuthBlockedForModel(auth, "claude-sonnet-4.5", now); !blocked {
		t.Errorf("claude-sonnet-4.5 should stay blocked while its pool is capped")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "gemini-3-pro", now); blocked {
		t.Errorf("gemini-3-pro should not be blocked by the Claude/GPT pool")
	}
}

// Clearing auth-level unavailability is only safe for quota cooldowns. A failure
// that applies to the whole credential must keep it out of rotation.
func TestAntigravityQuotaPoolIsolation_NonQuotaFailureKeepsAuthUnavailable(t *testing.T) {
	now := time.Now()
	recoverAt := now.Add(30 * time.Minute)

	auth := &Auth{
		ID:       "antigravity-test-unauthorized",
		Provider: "antigravity",
		Status:   StatusError,
		ModelStates: map[string]*ModelState{
			"claude-sonnet-4.5": {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: recoverAt,
				},
			},
			"gemini-3-pro": {
				// 401 from upstream: no quota state, but the model cannot serve.
				Unavailable:    true,
				Status:         StatusError,
				StatusMessage:  "unauthorized",
				NextRetryAfter: recoverAt,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if !auth.Unavailable {
		t.Errorf("auth.Unavailable = false, want true: the Gemini pool is down on a non-quota error")
	}
}
