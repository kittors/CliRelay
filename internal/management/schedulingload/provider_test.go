package schedulingload

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResetDiscountFactor(t *testing.T) {
	key := "t|1"
	now := time.Now()
	cases := []struct {
		name   string
		reset  time.Time
		hasKey bool
		want   float64
	}{
		{"no reset known -> full penalty", time.Time{}, false, 1},
		{"reset already passed -> burn freely", now.Add(-time.Hour), true, 0},
		{"within full burn window -> burn freely", now.Add(3 * time.Hour), true, 0},
		{"at horizon -> full penalty", now.Add(49 * time.Hour), true, 1},
		{"beyond horizon -> full penalty", now.Add(72 * time.Hour), true, 1},
	}
	for _, tc := range cases {
		resets := map[string]time.Time{}
		if tc.hasKey {
			resets[key] = tc.reset
		}
		got := resetDiscountFactor(key, resets)
		if tc.want == 0 {
			if got != 0 {
				t.Errorf("%s: got %v want 0", tc.name, got)
			}
		} else if tc.want == 1 {
			if got != 1 {
				t.Errorf("%s: got %v want 1", tc.name, got)
			}
		}
	}
}

func TestResetDiscountFactorRampsLinearly(t *testing.T) {
	key := "t|1"
	// Midpoint between fullBurnWindow (6h) and discountHorizon (48h) is 27h,
	// which should give a factor of 0.5.
	mid := time.Now().Add(27 * time.Hour)
	got := resetDiscountFactor(key, map[string]time.Time{key: mid})
	if got < 0.45 || got > 0.55 {
		t.Errorf("midpoint factor = %v, want ~0.5", got)
	}
}

func TestApplyResetDiscountScalesRatio(t *testing.T) {
	key := "t|1"
	near := map[string]time.Time{key: time.Now().Add(time.Hour)}
	if got := applyResetDiscount(key, 0.9, near); got != 0 {
		t.Errorf("near reset: got %v want 0 (quota shed disabled)", got)
	}
	far := map[string]time.Time{key: time.Now().Add(96 * time.Hour)}
	if got := applyResetDiscount(key, 0.9, far); got != 0.9 {
		t.Errorf("far reset: got %v want 0.9 (quota shed preserved)", got)
	}
}

func TestSummarizeQuotaWindowsConvertsRemainingAndSeparatesShortAndLongWindows(t *testing.T) {
	now := time.Now()
	rem90 := 90.0
	rem20 := 20.0
	rem100 := 100.0
	shortReset := now.Add(2 * time.Hour)
	farWeeklyReset := now.Add(5 * 24 * time.Hour)

	windows := []usage.QuotaWindowDTO{
		{QuotaKey: "code_5h", Percent: &rem90, WindowSeconds: 18000, ResetAt: &shortReset},
		{QuotaKey: "code_week", Percent: &rem20, WindowSeconds: usage.WeeklyQuotaWindowSeconds, ResetAt: &farWeeklyReset},
		{QuotaKey: "review_week", Percent: &rem100, WindowSeconds: usage.WeeklyQuotaWindowSeconds, ResetAt: &shortReset},
	}

	state, ok := summarizeQuotaWindows(windows, now)
	if !ok {
		t.Fatalf("expected summarizeQuotaWindows to succeed")
	}
	if math.Abs(state.shortUsed-0.10) > 1e-6 {
		t.Fatalf("shortUsed = %v, want 0.10 (90%% remaining)", state.shortUsed)
	}
	if math.Abs(state.longUsed-0.80) > 1e-6 {
		t.Fatalf("longUsed = %v, want 0.80 (20%% remaining)", state.longUsed)
	}
	if !state.hasLongReset || !state.longResetAt.Equal(farWeeklyReset) {
		t.Fatalf("longResetAt = %v, want %v (must ignore 5h window and review_week)", state.longResetAt, farWeeklyReset)
	}
	if got := state.effectiveLoad(now); math.Abs(got-0.80) > 1e-6 {
		t.Fatalf("effectiveLoad far from weekly reset = %v, want 0.80", got)
	}
}

func TestShortWindowExhaustionNotDiscountedByUpcomingWeeklyReset(t *testing.T) {
	now := time.Now()
	rem10 := 10.0 // 5h window 90% used
	rem80 := 80.0 // weekly window 20% used, 80% remaining
	shortReset := now.Add(2 * time.Hour)
	nearWeeklyReset := now.Add(3 * time.Hour)

	p := NewProvider()
	key := loadKey("t", "seat-1")
	state, ok := summarizeQuotaWindows([]usage.QuotaWindowDTO{
		{QuotaKey: "code_5h", Percent: &rem10, WindowSeconds: 18000, ResetAt: &shortReset},
		{QuotaKey: "code_week", Percent: &rem80, WindowSeconds: usage.WeeklyQuotaWindowSeconds, ResetAt: &nearWeeklyReset},
	}, now)
	if !ok {
		t.Fatalf("expected quota state")
	}
	p.current.Store(&snapshot{
		accounts:    map[string]accountQuotaState{key: state},
		ratios:      map[string]float64{key: state.rawWorstUsed()},
		resets:      map[string]time.Time{key: state.longResetAt},
		refreshedAt: now,
	})

	auth := &coreauth.Auth{TenantID: "t", Index: "seat-1"}
	load, ok := p.QuotaLoadRatio(auth)
	if !ok || math.Abs(load-0.90) > 1e-6 {
		t.Fatalf("QuotaLoadRatio = (%v, %v), want (0.90, true) from 5h window", load, ok)
	}
	rem, urg, ok := p.QuotaResetState(auth)
	if !ok || math.Abs(rem-0.10) > 1e-6 || math.Abs(urg-1.0) > 1e-6 {
		t.Fatalf("QuotaResetState = (%v, %v, %v), want (0.10, 1.0, true)", rem, urg, ok)
	}
}
