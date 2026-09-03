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
