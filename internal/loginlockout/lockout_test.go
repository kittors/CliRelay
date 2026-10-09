package loginlockout

import (
	"errors"
	"testing"
	"time"
)

func TestPenaltyStages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		failedCount int
		wantStage   int
		wantWait    time.Duration
		wantApply   bool
	}{
		{failedCount: 0, wantStage: 0, wantWait: 0, wantApply: false},
		{failedCount: 2, wantStage: 0, wantWait: 0, wantApply: false},
		// Nothing arms before 5. Production access logs show users transcribing a
		// correct generated password needing two or three attempts, so an earlier
		// rung locked people out for honest mistypes.
		{failedCount: 3, wantStage: 0, wantWait: 0, wantApply: false},
		{failedCount: 4, wantStage: 0, wantWait: 0, wantApply: false},
		{failedCount: 5, wantStage: 1, wantWait: time.Minute, wantApply: true},
		{failedCount: 9, wantStage: 1, wantWait: time.Minute, wantApply: false},
		{failedCount: 10, wantStage: 2, wantWait: 5 * time.Minute, wantApply: true},
		{failedCount: 14, wantStage: 2, wantWait: 5 * time.Minute, wantApply: false},
		{failedCount: 15, wantStage: 3, wantWait: 15 * time.Minute, wantApply: true},
		{failedCount: 19, wantStage: 3, wantWait: 15 * time.Minute, wantApply: false},
		{failedCount: 20, wantStage: 4, wantWait: 30 * time.Minute, wantApply: true},
		{failedCount: 24, wantStage: 4, wantWait: 30 * time.Minute, wantApply: false},
		{failedCount: 25, wantStage: 5, wantWait: 60 * time.Minute, wantApply: true},
		// Past the top stage the cooldown re-arms every fifth failure, so a client
		// stuck in a retry loop cannot ratchet its own penalty on every attempt.
		{failedCount: 26, wantStage: 5, wantWait: 60 * time.Minute, wantApply: false},
		{failedCount: 30, wantStage: 5, wantWait: 60 * time.Minute, wantApply: true},
	}

	for _, tc := range cases {
		stage, wait, apply := Penalty(tc.failedCount)
		if stage != tc.wantStage || wait != tc.wantWait || apply != tc.wantApply {
			t.Fatalf("Penalty(%d) = (%d, %v, %v), want (%d, %v, %v)",
				tc.failedCount, stage, wait, apply, tc.wantStage, tc.wantWait, tc.wantApply)
		}
	}
}

// TestPenaltyNeverPermanent is the X2 regression anchor. A previous ladder
// returned a permanent lock at 20 failures, and because the counter never
// decayed, twenty requests permanently locked any account until an
// administrator intervened — an attacker-triggerable denial of service.
func TestPenaltyNeverPermanent(t *testing.T) {
	t.Parallel()

	for count := 0; count <= 200; count++ {
		stage, wait, apply := Penalty(count)
		if apply && wait <= 0 {
			t.Fatalf("Penalty(%d) armed stage %d with a non-expiring cooldown (wait=%v)", count, stage, wait)
		}
	}
}

// The warning the sign-in form shows ("N attempts left") is only honest if it
// matches the ladder that actually locks. Walk the ladder failure by failure
// and check the count lands exactly on the next armed rung.
func TestRemainingBeforeCooldownMatchesPenalty(t *testing.T) {
	t.Parallel()

	for count := 0; count <= 200; count++ {
		remaining := RemainingBeforeCooldown(count)
		if remaining < 1 {
			t.Fatalf("RemainingBeforeCooldown(%d) = %d, want at least 1", count, remaining)
		}
		for step := 1; step < remaining; step++ {
			if _, _, apply := Penalty(count + step); apply {
				t.Fatalf("RemainingBeforeCooldown(%d) = %d, but failure %d already arms a cooldown",
					count, remaining, count+step)
			}
		}
		if _, _, apply := Penalty(count + remaining); !apply {
			t.Fatalf("RemainingBeforeCooldown(%d) = %d, but failure %d arms nothing",
				count, remaining, count+remaining)
		}
	}
}

func TestFailureWindowIsBounded(t *testing.T) {
	t.Parallel()

	// A window of zero would restore the "failures ever" semantics that locked
	// accounts on mistypes spread across months.
	if FailureWindow <= 0 || FailureWindow > time.Hour {
		t.Fatalf("FailureWindow = %v, want a positive window no larger than an hour", FailureWindow)
	}
}

func TestCooldownErrorCarriesRemainingTime(t *testing.T) {
	t.Parallel()

	now := time.Now()
	err := NewCooldownError(now.Add(5*time.Minute), now)
	if !errors.Is(err, ErrCooldown) {
		t.Fatalf("error = %v, want it to satisfy errors.Is(err, ErrCooldown)", err)
	}
	if err.RetryAfter != 5*time.Minute {
		t.Fatalf("RetryAfter = %v, want 5m", err.RetryAfter)
	}

	// An already-elapsed deadline must never render as "retry after 0s" while
	// the lock is still being reported.
	if past := NewCooldownError(now.Add(-time.Hour), now); past.RetryAfter < time.Second {
		t.Fatalf("RetryAfter = %v, want at least 1s", past.RetryAfter)
	}
}

func TestFailureErrorIsAWrongPasswordEvenWhenItArmsACooldown(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("invalid credentials")
	now := time.Now()

	warning := NewFailure(sentinel, 3, time.Time{}, now)
	if !errors.Is(warning, sentinel) {
		t.Fatalf("failure = %v, want errors.Is(err, sentinel)", warning)
	}
	if warning.Remaining != 2 || warning.Cooldown != nil {
		t.Fatalf("failure after 3 = {Remaining: %d, Cooldown: %v}, want {2, nil}", warning.Remaining, warning.Cooldown)
	}

	// The failure that trips the lock is still a guess: the API layer must keep
	// charging its throttle for it, so it may not hide behind the cooldown.
	armed := NewFailure(sentinel, 5, now.Add(time.Minute), now)
	if !errors.Is(armed, sentinel) {
		t.Fatalf("arming failure = %v, want errors.Is(err, sentinel)", armed)
	}
	if errors.Is(armed, ErrCooldown) {
		t.Fatalf("arming failure satisfies ErrCooldown; callers that only know the sentinels would stop charging the guess")
	}
	if armed.Cooldown == nil || armed.Cooldown.RetryAfter != time.Minute {
		t.Fatalf("arming failure Cooldown = %v, want a one-minute cooldown", armed.Cooldown)
	}
}
