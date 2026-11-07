package middleware

import (
	"math/rand/v2"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

func TestWithRetryAfterRoundsToTheSmallestWholeSecondLongerThanTheWait(t *testing.T) {
	cases := []struct {
		name string
		wait time.Duration
		want string
	}{
		{"a fraction of a second", 300 * time.Millisecond, "1"},
		{"one nanosecond", time.Nanosecond, "1"},
		{"just under a second", 999 * time.Millisecond, "1"},
		// A window drops an entry only once it is older than the window, so a
		// client that sleeps exactly the wait lands on the edge and is refused
		// again. The header is therefore strictly longer than the wait.
		{"exactly one second", time.Second, "2"},
		{"just over a second", time.Second + time.Millisecond, "2"},
		{"fractional minute", 59*time.Second + 200*time.Millisecond, "60"},
		{"whole minute", time.Minute, "61"},
		{"a day", 24 * time.Hour, "86401"},
		{"a month", 31 * 24 * time.Hour, "2678401"},
		// A boundary that passed while the verdict was built must not become a
		// retry loop, nor a negative number.
		{"zero", 0, "1"},
		{"already past", -5 * time.Second, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := quotaLimitVerdict("rpm", 1, 2, "rpm_limit_exceeded", "m").withRetryAfter(tc.wait, true)
			if got := headerValue(verdict, "Retry-After"); got != tc.want {
				t.Fatalf("Retry-After for %s = %q, want %q", tc.wait, got, tc.want)
			}
		})
	}
}

func TestWithRetryAfterLeavesVerdictsWithoutABoundaryAlone(t *testing.T) {
	verdict := quotaLimitVerdict("total", 5, 5, "total_quota_exceeded", "m")
	before := len(verdict.headers)
	if got := verdict.withRetryAfter(time.Minute, false); got != verdict || len(got.headers) != before {
		t.Fatalf("a verdict with no boundary was changed: %+v", got.headers)
	}
	if headerValue(verdict, "Retry-After") != "" {
		t.Fatal("Retry-After written for a limit that has no boundary")
	}
	var none *quotaVerdict
	if none.withRetryAfter(time.Minute, true) != nil {
		t.Fatal("a nil verdict must stay nil")
	}
}

func headerValue(verdict *quotaVerdict, name string) string {
	for _, h := range verdict.headers {
		if h.name == name {
			return h.value
		}
	}
	return ""
}

func TestSlidingWindowRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	at := func(seconds float64) time.Time {
		return now.Add(time.Duration(seconds * float64(time.Second)))
	}
	cases := []struct {
		name   string
		events []time.Time
		limit  int
		now    time.Time
		want   time.Duration
		known  bool
	}{
		{
			// Limit 1: the earlier request passed, the refused one was counted too,
			// so both have to age out before the retry (which counts itself) fits.
			name:   "limit one waits for the refused request to age out",
			events: []time.Time{at(0), at(10)}, limit: 1, now: at(10),
			want: 60 * time.Second, known: true,
		},
		{
			name:   "limit two waits for the second oldest entry",
			events: []time.Time{at(0), at(10), at(20)}, limit: 2, now: at(20),
			want: 50 * time.Second, known: true,
		},
		{
			name:   "a burst of five against limit three waits for the third oldest",
			events: []time.Time{at(0), at(5), at(10), at(15), at(20)}, limit: 3, now: at(20),
			want: 50 * time.Second, known: true,
		},
		{
			name:   "entries older than a minute do not count",
			events: []time.Time{at(0), at(70), at(75)}, limit: 1, now: at(75),
			want: 60 * time.Second, known: true,
		},
		{
			name:   "an entry exactly a minute old is still inside the window",
			events: []time.Time{at(0), at(10), at(20)}, limit: 2, now: at(60),
			want: 10 * time.Second, known: true,
		},
		{
			name:   "within the limit there is nothing to wait for",
			events: []time.Time{at(0), at(10)}, limit: 2, now: at(10), known: false,
		},
		{
			name:   "everything has aged out",
			events: []time.Time{at(0), at(1), at(2)}, limit: 1, now: at(120), known: false,
		},
		{name: "no events", limit: 1, now: at(0), known: false},
		{name: "no limit", events: []time.Time{at(0), at(1)}, limit: 0, now: at(1), known: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &slidingWindow{events: tc.events}
			got, known := w.retryAfter(tc.limit, tc.now)
			if known != tc.known || (known && got != tc.want) {
				t.Fatalf("retryAfter = %s, %v; want %s, %v", got, known, tc.want, tc.known)
			}
		})
	}
}

func TestTokenWindowRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	at := func(seconds float64) time.Time {
		return now.Add(time.Duration(seconds * float64(time.Second)))
	}
	cases := []struct {
		name    string
		entries []tokenEntry
		limit   int64
		now     time.Time
		want    time.Duration
		known   bool
	}{
		{
			name:    "the oldest entry carries enough to get back under",
			entries: []tokenEntry{{at(0), 60}, {at(20), 60}}, limit: 100, now: at(30),
			want: 30 * time.Second, known: true,
		},
		{
			name:    "one large entry has to age out entirely",
			entries: []tokenEntry{{at(5), 500}}, limit: 100, now: at(30),
			want: 35 * time.Second, known: true,
		},
		{
			// 110 tokens: dropping the first leaves exactly 100, which is still at the
			// limit (the check refuses at sum >= limit), so the second has to go too.
			name:    "landing exactly on the limit is not enough",
			entries: []tokenEntry{{at(0), 10}, {at(5), 10}, {at(10), 90}}, limit: 100, now: at(30),
			want: 35 * time.Second, known: true,
		},
		{
			name:    "entries older than a minute do not count",
			entries: []tokenEntry{{at(0), 500}, {at(70), 80}, {at(75), 80}}, limit: 100, now: at(80),
			want: 50 * time.Second, known: true,
		},
		{
			name:    "under the limit there is nothing to wait for",
			entries: []tokenEntry{{at(0), 60}, {at(20), 39}}, limit: 100, now: at(30), known: false,
		},
		{
			name:    "everything has aged out",
			entries: []tokenEntry{{at(0), 500}}, limit: 100, now: at(120), known: false,
		},
		{name: "no entries", limit: 100, now: at(0), known: false},
		{name: "no limit", entries: []tokenEntry{{at(0), 500}}, limit: 0, now: at(1), known: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &tokenWindow{entries: tc.entries}
			got, known := w.retryAfter(tc.limit, tc.now)
			if known != tc.known || (known && got != tc.want) {
				t.Fatalf("retryAfter = %s, %v; want %s, %v", got, known, tc.want, tc.known)
			}
		})
	}
}

// The wait must be exactly the point where admission flips: refused up to and
// including it, admitted one tick later. Random histories are replayed on the
// fake clock of a synctest bubble, against the same windows the middleware uses.
func TestRPMRetryAfterIsTheExactPointAdmissionFlips(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	checked := 0
	for trial := 0; trial < 300; trial++ {
		limit := 1 + rng.IntN(5)
		gaps := randomGaps(rng, limit+1+rng.IntN(8))
		for _, slack := range []time.Duration{0, time.Nanosecond} {
			synctest.Test(t, func(t *testing.T) {
				w := &slidingWindow{}
				for _, gap := range gaps {
					time.Sleep(gap)
					w.add()
				}
				// The last add was the refused request itself (counted, then refused).
				if w.count() <= limit {
					return
				}
				checked++
				wait, known := w.retryAfter(limit, time.Now())
				if !known || wait <= 0 {
					t.Fatalf("limit %d gaps %v: no wait for a refused window (%s, %v)", limit, gaps, wait, known)
				}
				time.Sleep(wait + slack)
				w.add() // the retry counts itself before the check
				admitted := w.count() <= limit
				if wantAdmitted := slack > 0; admitted != wantAdmitted {
					t.Fatalf("limit %d gaps %v: retry after wait+%s admitted = %v, want %v", limit, gaps, slack, admitted, wantAdmitted)
				}
			})
		}
	}
	// Histories that end inside the limit prove nothing; most draws must not.
	if checked < 300 {
		t.Fatalf("only %d refused histories were checked, the draw no longer exercises the wait", checked)
	}
}

func TestTPMRetryAfterIsTheExactPointAdmissionFlips(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	checked := 0
	for trial := 0; trial < 300; trial++ {
		limit := int64(50 + rng.IntN(200))
		n := 1 + rng.IntN(9)
		gaps := randomGaps(rng, n)
		tokens := make([]int64, n)
		for i := range tokens {
			tokens[i] = int64(1 + rng.IntN(120))
		}
		for _, slack := range []time.Duration{0, time.Nanosecond} {
			synctest.Test(t, func(t *testing.T) {
				w := &tokenWindow{}
				for i, gap := range gaps {
					time.Sleep(gap)
					w.add(tokens[i])
				}
				if w.sum() < limit {
					return
				}
				checked++
				wait, known := w.retryAfter(limit, time.Now())
				if !known || wait <= 0 {
					t.Fatalf("limit %d gaps %v tokens %v: no wait for a refused window (%s, %v)", limit, gaps, tokens, wait, known)
				}
				time.Sleep(wait + slack)
				admitted := w.sum() < limit
				if wantAdmitted := slack > 0; admitted != wantAdmitted {
					t.Fatalf("limit %d gaps %v tokens %v: after wait+%s admitted = %v, want %v", limit, gaps, tokens, slack, admitted, wantAdmitted)
				}
			})
		}
	}
	if checked < 300 {
		t.Fatalf("only %d refused histories were checked, the draw no longer exercises the wait", checked)
	}
}

// randomGaps draws gaps between events: mostly a few seconds, sometimes none at
// all (a burst) and sometimes long enough that older entries age out.
func randomGaps(rng *rand.Rand, n int) []time.Duration {
	gaps := make([]time.Duration, n)
	for i := range gaps {
		switch rng.IntN(5) {
		case 0:
			gaps[i] = 0
		case 1:
			gaps[i] = time.Duration(1+rng.IntN(70)) * time.Second
		default:
			gaps[i] = time.Duration(1+rng.IntN(9000)) * time.Millisecond
		}
	}
	return gaps
}

func TestWindowResetWait(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	stubPeriodReset(t, func(period quota.Period, at time.Time) (time.Time, bool) {
		switch period {
		case quota.PeriodDay:
			return at.Add(3 * time.Hour), true
		case quota.PeriodWeek:
			return at.Add(2 * 24 * time.Hour), true
		case quota.PeriodMonth:
			return at.Add(20 * 24 * time.Hour), true
		}
		return time.Time{}, false
	})
	cases := []struct {
		name          string
		window        quota.Period
		fiveHourStart time.Time
		want          time.Duration
		known         bool
	}{
		{"day", quota.PeriodDay, time.Time{}, 3 * time.Hour, true},
		{"week", quota.PeriodWeek, time.Time{}, 48 * time.Hour, true},
		{"month", quota.PeriodMonth, time.Time{}, 480 * time.Hour, true},
		{"five hours after the window opened", quota.PeriodFiveHour, now.Add(-90 * time.Minute), 210 * time.Minute, true},
		// The anchor can be older than a stale reading that is still being served.
		{"five hour window already past its end", quota.PeriodFiveHour, now.Add(-6 * time.Hour), -time.Hour, true},
		{"five hour window that never opened", quota.PeriodFiveHour, time.Time{}, 0, false},
		{"lifetime never resets", quota.PeriodLifetime, time.Time{}, 0, false},
		{"unknown window", quota.Period("fortnight"), time.Time{}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := windowResetWait(tc.window, tc.fiveHourStart, now)
			if known != tc.known || (known && got != tc.want) {
				t.Fatalf("windowResetWait = %s, %v; want %s, %v", got, known, tc.want, tc.known)
			}
		})
	}
}

func TestWindowResetWaitWithoutAWiredBoundary(t *testing.T) {
	stubPeriodReset(t, func(quota.Period, time.Time) (time.Time, bool) { return time.Time{}, false })
	for _, window := range []quota.Period{quota.PeriodDay, quota.PeriodWeek, quota.PeriodMonth} {
		if got, known := windowResetWait(window, time.Time{}, time.Now()); known {
			t.Fatalf("%s: a wait of %s was reported with no boundary to read", window, got)
		}
	}
}

func TestInitQuotaPeriodResetFuncIgnoresNil(t *testing.T) {
	marker := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	stubPeriodReset(t, func(quota.Period, time.Time) (time.Time, bool) { return marker, true })
	InitQuotaPeriodResetFunc(nil)
	if got, ok := periodResetAtFunc(quota.PeriodDay, time.Now()); !ok || !got.Equal(marker) {
		t.Fatalf("a nil injection replaced the installed function: %s, %v", got, ok)
	}
}

// stubPeriodReset installs the calendar boundary function for one test.
func stubPeriodReset(t *testing.T, fn func(quota.Period, time.Time) (time.Time, bool)) {
	t.Helper()
	previous := periodResetAtFunc
	periodResetAtFunc = fn
	t.Cleanup(func() { periodResetAtFunc = previous })
}
