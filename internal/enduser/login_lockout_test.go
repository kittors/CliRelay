package enduser

import (
	"testing"
	"time"
)

func TestLockPenaltyStages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		failedCount int
		wantStage   int
		wantWait    time.Duration
		wantApply   bool
	}{
		{failedCount: 0, wantStage: 0, wantWait: 0, wantApply: false},
		{failedCount: 2, wantStage: 0, wantWait: 0, wantApply: false},
		{failedCount: 3, wantStage: 1, wantWait: time.Minute, wantApply: true},
		{failedCount: 4, wantStage: 1, wantWait: time.Minute, wantApply: false},
		{failedCount: 5, wantStage: 2, wantWait: 5 * time.Minute, wantApply: true},
		{failedCount: 9, wantStage: 2, wantWait: 5 * time.Minute, wantApply: false},
		{failedCount: 10, wantStage: 3, wantWait: 15 * time.Minute, wantApply: true},
		{failedCount: 14, wantStage: 3, wantWait: 15 * time.Minute, wantApply: false},
		{failedCount: 15, wantStage: 4, wantWait: 30 * time.Minute, wantApply: true},
		{failedCount: 19, wantStage: 4, wantWait: 30 * time.Minute, wantApply: false},
		{failedCount: 20, wantStage: 5, wantWait: 60 * time.Minute, wantApply: true},
		// Past the top stage the cooldown re-arms every fifth failure, so a client
		// stuck in a retry loop cannot ratchet its own penalty on every attempt.
		{failedCount: 21, wantStage: 5, wantWait: 60 * time.Minute, wantApply: false},
		{failedCount: 25, wantStage: 5, wantWait: 60 * time.Minute, wantApply: true},
	}

	for _, tc := range cases {
		stage, wait, apply := lockPenalty(tc.failedCount)
		if stage != tc.wantStage || wait != tc.wantWait || apply != tc.wantApply {
			t.Fatalf("lockPenalty(%d) = (%d, %v, %v), want (%d, %v, %v)",
				tc.failedCount, stage, wait, apply, tc.wantStage, tc.wantWait, tc.wantApply)
		}
	}
}

// TestLockPenaltyNeverPermanent is the X2 regression anchor. The previous ladder
// returned permanent=true at 20 failures, and because the counter never decayed,
// twenty requests permanently locked any portal account until an administrator
// intervened — an attacker-triggerable denial of service.
func TestLockPenaltyNeverPermanent(t *testing.T) {
	t.Parallel()

	for count := 0; count <= 200; count++ {
		stage, wait, apply := lockPenalty(count)
		if apply && wait <= 0 {
			t.Fatalf("lockPenalty(%d) armed stage %d with a non-expiring cooldown (wait=%v)", count, stage, wait)
		}
	}
}

// TestEndUserFailureWindowIsBounded guards the decay half of the fix: a window of
// zero would restore the old "failures ever" semantics that produced the lockouts.
func TestEndUserFailureWindowIsBounded(t *testing.T) {
	t.Parallel()

	if endUserFailureWindow <= 0 || endUserFailureWindow > time.Hour {
		t.Fatalf("endUserFailureWindow = %v, want a positive window no larger than an hour", endUserFailureWindow)
	}
}
