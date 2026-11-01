package alerting

import (
	"math"
	"testing"
	"time"
)

// wide is an account-wide window, the only kind a pool adds up.
func wide(key string, span time.Duration, used float64, resetIn, observedAgo time.Duration) WindowSample {
	w := window(key, span, used, resetIn, observedAgo)
	w.AccountWide = true
	return w
}

func accountOf(provider, id string, windows ...WindowSample) AccountSample {
	a := account(id, windows...)
	a.Provider = provider
	return a
}

// reading is one stored reading minutesAgo before the test clock.
func reading(minutesAgo int, remaining float64, resetIn time.Duration) PoolReading {
	r := PoolReading{At: timeNow().Add(-time.Duration(minutesAgo) * time.Minute), RemainingPercent: remaining}
	if resetIn != 0 {
		r.ResetAt = timeNow().Add(resetIn)
	}
	return r
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.011 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func poolOf(t *testing.T, pools []Pool, provider, window string) Pool {
	t.Helper()
	for _, p := range pools {
		if p.Provider == provider && p.WindowKey == window {
			return p
		}
	}
	t.Fatalf("no pool for %s/%s in %+v", provider, window, pools)
	return Pool{}
}

func TestBuildPoolsAveragesTheShareLeft(t *testing.T) {
	now := timeNow()
	accounts := []AccountSample{
		accountOf("codex", "a", wide("code_week", 7*day, 40, 3*day, 10*time.Minute)),
		accountOf("codex", "b", wide("code_week", 7*day, 60, 2*day, 2*time.Hour)),
		accountOf("codex", "c", wide("code_week", 7*day, 80, 5*day, 30*time.Minute)),
	}
	pools := BuildPools(accounts, nil, now)
	if len(pools) != 1 {
		t.Fatalf("pools = %+v, want one", pools)
	}
	p := pools[0]
	if p.Provider != "codex" || p.WindowKey != "code_week" || p.WindowSeconds != int64(7*day/time.Second) {
		t.Fatalf("identity = %+v", p)
	}
	if p.Accounts != 3 || p.AccountsWithData != 3 {
		t.Fatalf("accounts = %d/%d, want 3/3", p.AccountsWithData, p.Accounts)
	}
	// The mean of what is left (60, 40, 20), not of what is used.
	approx(t, "remaining", p.RemainingPercent, 40)
	if want := now.Add(2 * day); !p.EarliestResetAt.Equal(want) {
		t.Fatalf("earliest reset = %v, want %v", p.EarliestResetAt, want)
	}
	if want := now.Add(-2 * time.Hour); !p.ObservedAt.Equal(want) {
		t.Fatalf("observed at = %v, want the oldest, %v", p.ObservedAt, want)
	}
	if p.BurnPercentPerHour != nil || !p.ProjectedExhaustionAt.IsZero() {
		t.Fatalf("no history was given, yet burn = %v, projection = %v", p.BurnPercentPerHour, p.ProjectedExhaustionAt)
	}
}

func TestBuildPoolsOnlyPoolsAccountWideWindows(t *testing.T) {
	scoped := window("seven_day_opus", 7*day, 50, 2*day, 0) // AccountWide is false
	pools := BuildPools([]AccountSample{
		accountOf("claude", "a", wide("seven_day", 7*day, 10, 2*day, 0), scoped),
	}, nil, timeNow())
	if len(pools) != 1 || pools[0].WindowKey != "seven_day" {
		t.Fatalf("pools = %+v, want only the account-wide window", pools)
	}
}

func TestBuildPoolsLeavesOutStaticKeysAndCountsSilentAccounts(t *testing.T) {
	static := accountOf("codex", "key", wide("code_week", 7*day, 0, day, 0))
	static.StaticKey = true
	silent := accountOf("codex", "silent") // probed never, or the probe failed
	pools := BuildPools([]AccountSample{
		accountOf("codex", "a", wide("code_week", 7*day, 30, day, 0)),
		silent, static,
	}, nil, timeNow())
	p := poolOf(t, pools, "codex", "code_week")
	if p.Accounts != 2 || p.AccountsWithData != 1 {
		t.Fatalf("accounts = %d of %d, want 1 of 2 (the static key is not an account, the silent one is a gap)", p.AccountsWithData, p.Accounts)
	}
	approx(t, "remaining", p.RemainingPercent, 70)
}

func TestBuildPoolsCountsASharedAccountOnce(t *testing.T) {
	a := accountOf("codex", "a", wide("code_week", 7*day, 20, day, 0))
	b := accountOf("codex", "b", wide("code_week", 7*day, 20, day, 0))
	b.Subject = a.Subject // two credentials of the same physical account
	p := poolOf(t, BuildPools([]AccountSample{a, b}, nil, timeNow()), "codex", "code_week")
	if p.Accounts != 1 || p.AccountsWithData != 1 {
		t.Fatalf("accounts = %d of %d, want 1 of 1", p.AccountsWithData, p.Accounts)
	}
}

func TestBuildPoolsTreatsARenewedWindowAsNoData(t *testing.T) {
	now := timeNow()
	stale := wide("code_week", 7*day, 95, 0, 0)
	stale.ResetAt = now.Add(-time.Hour) // renewed since the reading
	live := wide("code_week", 7*day, 50, day, 0)

	p := poolOf(t, BuildPools([]AccountSample{
		accountOf("codex", "stale", stale), accountOf("codex", "live", live),
	}, nil, now), "codex", "code_week")
	if p.Accounts != 2 || p.AccountsWithData != 1 {
		t.Fatalf("accounts = %d of %d, want 1 of 2", p.AccountsWithData, p.Accounts)
	}
	// Not 100% for the renewed account, and not its stale 5% either: it is left out.
	approx(t, "remaining", p.RemainingPercent, 50)

	only := poolOf(t, BuildPools([]AccountSample{accountOf("codex", "stale", stale)}, nil, now), "codex", "code_week")
	if only.AccountsWithData != 0 || only.RemainingPercent != 0 || only.BurnPercentPerHour != nil || !only.EarliestResetAt.IsZero() {
		t.Fatalf("a pool with no live reading must report none: %+v", only)
	}
}

func TestBuildPoolsIsSortedAndSplitByProvider(t *testing.T) {
	pools := BuildPools([]AccountSample{
		accountOf("xai", "x", wide("weekly_limit", 7*day, 10, day, 0)),
		accountOf("codex", "c", wide("code_week", 7*day, 10, day, 0), wide("code_5h", 5*time.Hour, 10, time.Hour, 0)),
		accountOf("claude", "l", wide("seven_day", 7*day, 10, day, 0)),
	}, nil, timeNow())
	var got []string
	for _, p := range pools {
		got = append(got, p.Provider+"/"+p.WindowKey)
	}
	want := []string{"claude/seven_day", "codex/code_5h", "codex/code_week", "xai/weekly_limit"}
	if len(got) != len(want) {
		t.Fatalf("pools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pools = %v, want %v", got, want)
		}
	}
}

func TestDrainRate(t *testing.T) {
	now := timeNow()
	cases := []struct {
		name     string
		readings []PoolReading
		wantOK   bool
		want     float64
	}{
		{"steady loss over four hours", []PoolReading{reading(240, 80, 0), reading(120, 60, 0), reading(0, 40, 0)}, true, 10},
		{"idle account", []PoolReading{reading(300, 55, 0), reading(150, 55, 0), reading(0, 55, 0)}, true, 0},
		{"a single reading says nothing", []PoolReading{reading(0, 50, 0)}, false, 0},
		{"no readings", nil, false, 0},
		{"less than half an hour of history", []PoolReading{reading(20, 90, 0), reading(0, 80, 0)}, false, 0},
		{"exactly half an hour", []PoolReading{reading(30, 90, 0), reading(0, 80, 0)}, true, 20},
		{"readings older than the lookback are not used", []PoolReading{reading(400, 100, 0), reading(380, 90, 0), reading(0, 90, 0)}, false, 0},
		{"a reading from the future is not used", []PoolReading{reading(120, 50, 0), reading(-30, 10, 0)}, false, 0},
		{
			"only the cycle after a refill counts",
			[]PoolReading{reading(300, 30, 0), reading(240, 10, 0), reading(180, 100, 0), reading(90, 90, 0), reading(0, 80, 0)},
			true, 20.0 / 3, // 20 points in 180 minutes
		},
		{
			"the announced reset moving a whole window later is a new cycle",
			[]PoolReading{
				reading(240, 10, 2*time.Hour), reading(180, 10, 2*time.Hour+2*time.Second),
				reading(120, 10, 7*day), reading(0, 7, 7*day),
			},
			true, 1.5, // 3 points in 120 minutes
		},
		{
			"a reset that drifts by seconds is the same cycle",
			[]PoolReading{reading(120, 60, day), reading(60, 55, day+3*time.Second), reading(0, 50, day-2*time.Second)},
			true, 5,
		},
		{"a refilled window never counts as a negative drain", []PoolReading{reading(60, 20, 0), reading(0, 90, 0)}, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := drainRate(c.readings, now)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (rate %v)", ok, c.wantOK, got)
			}
			if ok {
				approx(t, "rate", got, c.want)
			}
		})
	}
}

func TestDrainRateSortsItsInput(t *testing.T) {
	rate, ok := drainRate([]PoolReading{reading(0, 40, 0), reading(240, 80, 0), reading(120, 60, 0)}, timeNow())
	if !ok {
		t.Fatal("no rate")
	}
	approx(t, "rate", rate, 10)
}

// pooled builds n accounts with the same window and a draining history of
// pointsPerHour for those listed in withHistory.
func pooled(n int, used float64, resetIn time.Duration, withHistory int, pointsPerHour float64) ([]AccountSample, PoolHistory) {
	var accounts []AccountSample
	history := PoolHistory{}
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		a := accountOf("codex", id, wide("code_week", 7*day, used, resetIn, 0))
		accounts = append(accounts, a)
		if i < withHistory {
			// Four hours of readings ending at what the sample says is left.
			left := 100 - used
			history[PoolSeries{Subject: a.Subject, Window: "code_week"}] = []PoolReading{
				reading(240, left+4*pointsPerHour, resetIn),
				reading(120, left+2*pointsPerHour, resetIn),
				reading(0, left, resetIn),
			}
		}
	}
	return accounts, history
}

func TestBuildPoolsProjectsExhaustionBeforeTheFirstReset(t *testing.T) {
	now := timeNow()
	accounts, history := pooled(3, 62, 48*time.Hour, 3, 2) // 38% left, 2 points an hour
	p := poolOf(t, BuildPools(accounts, history, now), "codex", "code_week")
	approx(t, "remaining", p.RemainingPercent, 38)
	if p.BurnPercentPerHour == nil {
		t.Fatal("no burn rate")
	}
	approx(t, "burn", *p.BurnPercentPerHour, 2)
	if want := now.Add(19 * time.Hour); p.ProjectedExhaustionAt.Sub(want).Abs() > time.Minute {
		t.Fatalf("projected exhaustion = %v, want about %v", p.ProjectedExhaustionAt, want)
	}
}

func TestBuildPoolsWithholdsAProjectionPastTheFirstReset(t *testing.T) {
	// 19 hours of capacity, but the first account refills in 10: the straight
	// line stops describing the pool there, so no exhaustion time is claimed.
	accounts, history := pooled(3, 62, 10*time.Hour, 3, 2)
	p := poolOf(t, BuildPools(accounts, history, timeNow()), "codex", "code_week")
	if p.BurnPercentPerHour == nil || !p.ProjectedExhaustionAt.IsZero() {
		t.Fatalf("burn = %v, projection = %v; want a rate and no exhaustion time", p.BurnPercentPerHour, p.ProjectedExhaustionAt)
	}
}

func TestBuildPoolsDoesNotProjectWithoutAResetOrWithoutAPace(t *testing.T) {
	// No announced reset: there is nothing to be "before".
	accounts, history := pooled(2, 62, 0, 2, 2)
	p := poolOf(t, BuildPools(accounts, history, timeNow()), "codex", "code_week")
	if p.BurnPercentPerHour == nil || !p.ProjectedExhaustionAt.IsZero() {
		t.Fatalf("burn = %v, projection = %v; want a rate and no exhaustion time", p.BurnPercentPerHour, p.ProjectedExhaustionAt)
	}
	// An idle pool has a rate, zero, and no exhaustion.
	accounts, history = pooled(2, 62, 48*time.Hour, 2, 0)
	p = poolOf(t, BuildPools(accounts, history, timeNow()), "codex", "code_week")
	if p.BurnPercentPerHour == nil || *p.BurnPercentPerHour != 0 || !p.ProjectedExhaustionAt.IsZero() {
		t.Fatalf("burn = %v, projection = %v; want zero and no exhaustion time", p.BurnPercentPerHour, p.ProjectedExhaustionAt)
	}
}

func TestBuildPoolsDoesNotProjectAnEmptyPool(t *testing.T) {
	accounts, history := pooled(2, 100, 48*time.Hour, 2, 2)
	p := poolOf(t, BuildPools(accounts, history, timeNow()), "codex", "code_week")
	if p.RemainingPercent != 0 || !p.ProjectedExhaustionAt.IsZero() {
		t.Fatalf("remaining = %v, projection = %v; an empty pool is not 'about to run out'", p.RemainingPercent, p.ProjectedExhaustionAt)
	}
}

func TestBuildPoolsNeedsHistoryFromEnoughAccounts(t *testing.T) {
	// One account's pace is not the pool's: with 1 of 4 there is no rate.
	accounts, history := pooled(4, 50, 48*time.Hour, 1, 5)
	p := poolOf(t, BuildPools(accounts, history, timeNow()), "codex", "code_week")
	if p.BurnPercentPerHour != nil {
		t.Fatalf("burn = %v from 1 of 4 accounts, want none", *p.BurnPercentPerHour)
	}
	// Half of them is enough.
	accounts, history = pooled(4, 50, 48*time.Hour, 2, 5)
	p = poolOf(t, BuildPools(accounts, history, timeNow()), "codex", "code_week")
	if p.BurnPercentPerHour == nil {
		t.Fatal("no rate from 2 of 4 accounts")
	}
}

func TestPoolHistoryScope(t *testing.T) {
	static := accountOf("codex", "key", wide("code_week", 7*day, 0, day, 0))
	static.StaticKey = true
	scoped := window("seven_day_opus", 7*day, 0, day, 0)
	a := accountOf("claude", "a", wide("seven_day", 7*day, 0, day, 0), scoped)
	b := accountOf("codex", "b", wide("code_week", 7*day, 0, day, 0), wide("code_5h", 5*time.Hour, 0, hour, 0))
	noSubject := accountOf("codex", "n", wide("code_week", 7*day, 0, day, 0))
	noSubject.Subject = ""

	subjects, windows := PoolHistoryScope([]AccountSample{static, a, b, noSubject})
	if len(subjects) != 2 || subjects[0] != "subject-a" || subjects[1] != "subject-b" {
		t.Fatalf("subjects = %v, want a and b only", subjects)
	}
	if len(windows) != 3 || windows[0] != "code_5h" || windows[1] != "code_week" || windows[2] != "seven_day" {
		t.Fatalf("windows = %v, want the three account-wide keys, sorted", windows)
	}
}

func TestCommonSpanAndLabelAreDeterministic(t *testing.T) {
	if got := commonSpan(map[int64]int{0: 5, 604800: 1, 2592000: 1}); got != 2592000 {
		t.Fatalf("span = %d, want the longer on a tie and never the unsaid 0", got)
	}
	if got := commonSpan(map[int64]int{0: 3}); got != 0 {
		t.Fatalf("span = %d, want 0 when nobody says", got)
	}
	if got := commonLabel(map[string]int{"b week": 2, "a week": 2, "c": 1}); got != "a week" {
		t.Fatalf("label = %q, want the alphabetically first on a tie", got)
	}
}
