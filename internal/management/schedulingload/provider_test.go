package schedulingload

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// providerWith serves a fixed snapshot: one account per index in the system
// tenant. refreshedAt is now, so no read triggers a refresh that would replace it.
func providerWith(accounts map[string][]usage.QuotaWindowDTO) *Provider {
	p := NewProvider()
	byAccount := make(map[string][]window, len(accounts))
	for index, dtos := range accounts {
		byAccount[loadKey("", index)] = windowsFromStatus(dtos)
	}
	p.current.Store(&snapshot{windows: byAccount, refreshedAt: time.Now()})
	return p
}

func remaining(v float64) *float64 { return &v }

func at(t time.Time) *time.Time { return &t }

func account(index string) *coreauth.Auth { return &coreauth.Auth{ID: index, Index: index} }

func near(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// The status records store the share remaining (what the panel draws), not the
// share used. Reading one as the other sent traffic to the accounts that were
// nearly spent and away from the fresh ones.
func TestQuotaLoadRatioReadsTheShareRemaining(t *testing.T) {
	week := time.Now().Add(5 * 24 * time.Hour)
	p := providerWith(map[string][]usage.QuotaWindowDTO{
		"nearly-spent": {{QuotaKey: "code_week", WindowSeconds: 604800, Percent: remaining(5), ResetAt: at(week)}},
		"fresh":        {{QuotaKey: "code_week", WindowSeconds: 604800, Percent: remaining(95), ResetAt: at(week)}},
	})
	if got, ok := p.QuotaLoadRatio(account("nearly-spent")); !ok || !near(got, 0.95) {
		t.Fatalf("nearly spent account: ratio %v (%v), want 0.95", got, ok)
	}
	if got, ok := p.QuotaLoadRatio(account("fresh")); !ok || !near(got, 0.05) {
		t.Fatalf("fresh account: ratio %v (%v), want 0.05", got, ok)
	}
}

func TestQuotaLoadRatioCountsTheWindowsThatLimitTheModel(t *testing.T) {
	week := time.Now().Add(5 * 24 * time.Hour)
	hours := time.Now().Add(3 * time.Hour)
	p := providerWith(map[string][]usage.QuotaWindowDTO{
		"claude": {
			{QuotaKey: "five_hour", WindowSeconds: 18000, Percent: remaining(70), ResetAt: at(hours)},
			{QuotaKey: "seven_day", WindowSeconds: 604800, Percent: remaining(60), ResetAt: at(week)},
			{QuotaKey: "seven_day_opus", WindowSeconds: 604800, Percent: remaining(2), ResetAt: at(week)},
			// Feature allowances say nothing about chat requests.
			{QuotaKey: "seven_day_oauth_apps", WindowSeconds: 604800, Percent: remaining(0), ResetAt: at(week)},
		},
		"gemini": {
			{QuotaKey: "model:gemini-2.5-pro", Percent: remaining(10), ResetAt: at(hours)},
		},
	})
	cases := []struct {
		auth  string
		model string
		want  float64
		ok    bool
	}{
		{"claude", "claude-opus-4-5", 0.98, true},
		// Sonnet does not draw from the Opus week.
		{"claude", "claude-sonnet-4-5", 0.40, true},
		// Without a model only the account-wide windows answer.
		{"claude", "", 0.40, true},
		{"gemini", "gemini-2.5-pro", 0.90, true},
		{"gemini", "gemini-2.5-flash", 0, false},
		{"gemini", "", 0, false},
		{"unknown", "gemini-2.5-pro", 0, false},
	}
	for _, tc := range cases {
		got, ok := p.QuotaLoadRatioForModel(account(tc.auth), tc.model)
		if ok != tc.ok || !near(got, tc.want) {
			t.Errorf("%s/%q: ratio %v (%v), want %v (%v)", tc.auth, tc.model, got, ok, tc.want, tc.ok)
		}
	}
}

// Only weekly-or-longer windows are discounted near their renewal. A five-hour
// window always renews within the full-burn window, so discounting it (as the
// soonest reset across all windows did) switched the quota signal off for every
// Codex and Claude account.
func TestLongWindowDiscountSparesShortWindows(t *testing.T) {
	now := time.Now()
	p := providerWith(map[string][]usage.QuotaWindowDTO{
		"week-renews-soon": {
			{QuotaKey: "code_5h", WindowSeconds: 18000, Percent: remaining(80), ResetAt: at(now.Add(2 * time.Hour))},
			{QuotaKey: "code_week", WindowSeconds: 604800, Percent: remaining(5), ResetAt: at(now.Add(3 * time.Hour))},
		},
		"five-hours-nearly-spent": {
			{QuotaKey: "code_5h", WindowSeconds: 18000, Percent: remaining(5), ResetAt: at(now.Add(30 * time.Minute))},
			{QuotaKey: "code_week", WindowSeconds: 604800, Percent: remaining(70), ResetAt: at(now.Add(4 * 24 * time.Hour))},
		},
	})
	// The week's 95% is about to be wiped, so only the five hours' 20% counts.
	if got, _ := p.QuotaLoadRatio(account("week-renews-soon")); !near(got, 0.20) {
		t.Fatalf("week renewing soon: ratio %v, want 0.20", got)
	}
	if got, _ := p.QuotaLoadRatio(account("five-hours-nearly-spent")); !near(got, 0.95) {
		t.Fatalf("five hours nearly spent: ratio %v, want 0.95", got)
	}
}

func TestLongWindowDiscountRampsLinearly(t *testing.T) {
	now := time.Now()
	week := func(until time.Duration) window {
		return window{key: "code_week", span: 7 * 24 * time.Hour, used: 1, resetAt: now.Add(until)}
	}
	cases := []struct {
		name string
		w    window
		want float64
	}{
		{"within the full-burn window", week(3 * time.Hour), 0},
		{"midway to the horizon", week(27 * time.Hour), 0.5},
		{"at the horizon", week(48 * time.Hour), 1},
		{"renewal unknown", window{key: "code_week", span: 7 * 24 * time.Hour, used: 1}, 1},
		{"five-hour window", window{key: "code_5h", span: 5 * time.Hour, used: 1, resetAt: now.Add(time.Hour)}, 1},
	}
	for _, tc := range cases {
		if got := longWindowDiscount(tc.w, now); math.Abs(got-tc.want) > 0.01 {
			t.Errorf("%s: discount %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestQuotaWindowsStartOverOnceTheyRenew(t *testing.T) {
	now := time.Now()
	p := providerWith(map[string][]usage.QuotaWindowDTO{
		"codex": {
			// Renewed since the probe read it 95% spent.
			{QuotaKey: "code_5h", WindowSeconds: 18000, Percent: remaining(5), ResetAt: at(now.Add(-time.Minute))},
			// Not started: no renewal time yet.
			{QuotaKey: "code_week", WindowSeconds: 604800, Percent: remaining(100)},
			{QuotaKey: "review_week", WindowSeconds: 604800, Percent: remaining(0)},
			// Usage unknown: nothing to order on.
			{QuotaKey: "code_subscription_2592000", WindowSeconds: 2592000, ResetAt: at(now.Add(time.Hour))},
		},
	})
	windows, ok := p.QuotaWindows(account("codex"), "gpt-5.6-sol")
	if !ok || len(windows) != 2 {
		t.Fatalf("windows = %+v (%v), want the 5h and weekly code windows", windows, ok)
	}
	byKey := map[string]coreauth.QuotaWindow{}
	for _, w := range windows {
		byKey[w.Key] = w
	}
	fiveHour, week := byKey["code_5h"], byKey["code_week"]
	if fiveHour.Used != 0 || week.Used != 0 {
		t.Fatalf("used = %v / %v, want both windows to start over at 0", fiveHour.Used, week.Used)
	}
	if d := fiveHour.ResetAt.Sub(now); d < 5*time.Hour-time.Minute || d > 5*time.Hour+time.Minute {
		t.Fatalf("renewed 5h window resets in %v, want about 5h", d)
	}
	if d := week.ResetAt.Sub(now); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("unstarted week resets in %v, want about a week", d)
	}
	if got, _ := p.QuotaLoadRatio(account("codex")); got != 0 {
		t.Fatalf("ratio %v, want 0 once the spent window has renewed", got)
	}
}
