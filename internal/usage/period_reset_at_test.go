package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

func TestPeriodResetAtLocation(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	cases := []struct {
		name   string
		period quota.Period
		now    time.Time
		want   time.Time
	}{
		{"day, mid afternoon", quota.PeriodDay, time.Date(2026, 10, 30, 15, 4, 5, 0, cst), time.Date(2026, 10, 31, 0, 0, 0, 0, cst)},
		{"day, one nanosecond before midnight", quota.PeriodDay, time.Date(2026, 10, 30, 23, 59, 59, 999999999, cst), time.Date(2026, 10, 31, 0, 0, 0, 0, cst)},
		{"day, exactly midnight starts a new day", quota.PeriodDay, time.Date(2026, 10, 31, 0, 0, 0, 0, cst), time.Date(2026, 11, 1, 0, 0, 0, 0, cst)},
		{"day, last day of the year", quota.PeriodDay, time.Date(2026, 12, 31, 12, 0, 0, 0, cst), time.Date(2027, 1, 1, 0, 0, 0, 0, cst)},
		{"day, leap day", quota.PeriodDay, time.Date(2028, 2, 28, 12, 0, 0, 0, cst), time.Date(2028, 2, 29, 0, 0, 0, 0, cst)},
		// 2026-10-30 is a Friday, so its week ends at Monday 11-02 00:00.
		{"week, friday", quota.PeriodWeek, time.Date(2026, 10, 30, 15, 0, 0, 0, cst), time.Date(2026, 11, 2, 0, 0, 0, 0, cst)},
		{"week, monday morning", quota.PeriodWeek, time.Date(2026, 10, 26, 0, 0, 1, 0, cst), time.Date(2026, 11, 2, 0, 0, 0, 0, cst)},
		{"week, sunday night", quota.PeriodWeek, time.Date(2026, 11, 1, 23, 59, 0, 0, cst), time.Date(2026, 11, 2, 0, 0, 0, 0, cst)},
		{"week, across the year end", quota.PeriodWeek, time.Date(2026, 12, 30, 9, 0, 0, 0, cst), time.Date(2027, 1, 4, 0, 0, 0, 0, cst)},
		{"month, mid month", quota.PeriodMonth, time.Date(2026, 10, 30, 15, 0, 0, 0, cst), time.Date(2026, 11, 1, 0, 0, 0, 0, cst)},
		{"month, december", quota.PeriodMonth, time.Date(2026, 12, 5, 0, 0, 0, 0, cst), time.Date(2027, 1, 1, 0, 0, 0, 0, cst)},
		{"month, february of a leap year", quota.PeriodMonth, time.Date(2028, 2, 29, 23, 0, 0, 0, cst), time.Date(2028, 3, 1, 0, 0, 0, 0, cst)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := periodResetAtLocation(tc.period, tc.now, cst)
			if !ok || !got.Equal(tc.want) {
				t.Fatalf("periodResetAtLocation(%s, %s) = %s, %v; want %s", tc.period, tc.now, got, ok, tc.want)
			}
		})
	}
}

// The instant is read in the project zone, whatever zone the caller's clock is in.
func TestPeriodResetAtLocationFollowsTheProjectZoneNotTheCaller(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	// 20:30 UTC on the 30th is 04:30 on the 31st in CST, whose day ends at the 1st.
	now := time.Date(2026, 10, 30, 20, 30, 0, 0, time.UTC)
	got, ok := periodResetAtLocation(quota.PeriodDay, now, cst)
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, cst)
	if !ok || !got.Equal(want) {
		t.Fatalf("day end = %s, %v; want %s", got, ok, want)
	}
	// The same instant in UTC would end the day four and a half hours sooner.
	utc, _ := periodResetAtLocation(quota.PeriodDay, now, time.UTC)
	if utc.Equal(got) {
		t.Fatalf("UTC and CST day ends are the same instant (%s); the zone is not being used", got)
	}
}

func TestPeriodResetAtLocationKeepsRealDayLengthsAcrossDST(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	// US clocks went forward on 2026-03-08 (a 23-hour day) and back on 2026-11-01
	// (25 hours): the edge is the next local midnight, not now plus 24 hours.
	spring := time.Date(2026, 3, 8, 0, 30, 0, 0, newYork)
	got, ok := periodResetAtLocation(quota.PeriodDay, spring, newYork)
	if want := time.Date(2026, 3, 9, 0, 0, 0, 0, newYork); !ok || !got.Equal(want) {
		t.Fatalf("spring-forward day end = %s, %v; want %s", got, ok, want)
	}
	if got.Sub(spring) != 22*time.Hour+30*time.Minute {
		t.Fatalf("spring-forward 00:30 to midnight = %s, want 22h30m (the day has 23 hours)", got.Sub(spring))
	}
	autumn := time.Date(2026, 11, 1, 0, 30, 0, 0, newYork)
	got, ok = periodResetAtLocation(quota.PeriodDay, autumn, newYork)
	if want := time.Date(2026, 11, 2, 0, 0, 0, 0, newYork); !ok || !got.Equal(want) {
		t.Fatalf("fall-back day end = %s, %v; want %s", got, ok, want)
	}
	if got.Sub(autumn) != 24*time.Hour+30*time.Minute {
		t.Fatalf("fall-back 00:30 to midnight = %s, want 24h30m (the day has 25 hours)", got.Sub(autumn))
	}
	// The week that holds the spring change still ends at a local Monday midnight.
	week, ok := periodResetAtLocation(quota.PeriodWeek, spring, newYork)
	if want := time.Date(2026, 3, 9, 0, 0, 0, 0, newYork); !ok || !week.Equal(want) {
		t.Fatalf("week end = %s, %v; want %s", week, ok, want)
	}
}

func TestPeriodResetAtLocationHasNoEndForAnchoredAndLifetimePeriods(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	for _, period := range []quota.Period{quota.PeriodFiveHour, quota.PeriodLifetime, quota.Period("fortnight"), quota.Period("")} {
		if got, ok := periodResetAtLocation(period, now, time.UTC); ok {
			t.Fatalf("period %q: got an end %s, want none", period, got)
		}
	}
}

func TestPeriodResetAtLocationDefaultsToTheLocalZone(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.Local)
	got, ok := periodResetAtLocation(quota.PeriodDay, now, nil)
	if want := time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local); !ok || !got.Equal(want) {
		t.Fatalf("nil zone: day end = %s, %v; want %s", got, ok, want)
	}
}

// The edge must be exactly where the usage queries change window: the key they
// count by differs from the last instant before it and equals it from the edge on.
func TestPeriodResetAtLocationMatchesTheWindowKeysTheUsageQueriesCountBy(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	start := time.Date(2026, 1, 1, 3, 17, 0, 0, cst)
	for i := 0; i < 400; i++ {
		now := start.Add(time.Duration(i)*19*time.Hour + 41*time.Minute)
		keys := PeriodWindowKeysAt(now, cst)
		for _, period := range []quota.Period{quota.PeriodDay, quota.PeriodWeek, quota.PeriodMonth} {
			end, ok := periodResetAtLocation(period, now, cst)
			if !ok {
				t.Fatalf("%s at %s: no end", period, now)
			}
			before := PeriodWindowKeysAt(end.Add(-time.Nanosecond), cst)
			after := PeriodWindowKeysAt(end, cst)
			switch period {
			case quota.PeriodDay:
				if before.Day != keys.Day || after.Day == keys.Day {
					t.Fatalf("day at %s: end %s does not separate key %s from %s", now, end, before.Day, after.Day)
				}
			case quota.PeriodWeek:
				if before.WeekFrom != keys.WeekFrom || after.WeekFrom == keys.WeekFrom {
					t.Fatalf("week at %s: end %s does not separate key %s from %s", now, end, before.WeekFrom, after.WeekFrom)
				}
			case quota.PeriodMonth:
				if before.MonthFrom != keys.MonthFrom || after.MonthFrom == keys.MonthFrom {
					t.Fatalf("month at %s: end %s does not separate key %s from %s", now, end, before.MonthFrom, after.MonthFrom)
				}
			}
		}
	}
}

// PeriodResetAt reads the zone the usage tables count by. Without it a daily
// limit would tell the client to come back at UTC midnight while the counters
// roll over at the operator's own.
func TestPeriodResetAtUsesTheProjectUsageZone(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	usageDBMu.Lock()
	previous := usageLoc
	usageLoc = cst
	usageDBMu.Unlock()
	t.Cleanup(func() {
		usageDBMu.Lock()
		usageLoc = previous
		usageDBMu.Unlock()
	})

	// Friday 20:30 UTC is already Saturday 04:30 in CST, on the last day of October.
	now := time.Date(2026, 10, 30, 20, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		period quota.Period
		want   time.Time
	}{
		{quota.PeriodDay, time.Date(2026, 11, 1, 0, 0, 0, 0, cst)},
		{quota.PeriodWeek, time.Date(2026, 11, 2, 0, 0, 0, 0, cst)},
		{quota.PeriodMonth, time.Date(2026, 11, 1, 0, 0, 0, 0, cst)},
	} {
		got, ok := PeriodResetAt(tc.period, now)
		if !ok || !got.Equal(tc.want) {
			t.Fatalf("PeriodResetAt(%s, %s) = %s, %v; want %s", tc.period, now, got, ok, tc.want)
		}
	}
}

// A zone whose clocks skip midnight (Sao Paulo went from 00:00 straight to 01:00
// on 2018-11-04, Havana still does) leaves the computed edge before now: time.Date
// resolves the hour that does not exist to the one before it. An answer already
// in the past would send the client away for a wait that is over, so it must be
// withheld; the caller then sends no Retry-After rather than a wrong one.
func TestPeriodResetAtLocationNeverAnswersWithAnInstantInThePast(t *testing.T) {
	for _, tc := range []struct {
		zone string
		now  func(*time.Location) time.Time
	}{
		{"America/Sao_Paulo", func(loc *time.Location) time.Time { return time.Date(2018, 11, 3, 23, 30, 0, 0, loc) }},
		{"America/Havana", func(loc *time.Location) time.Time { return time.Date(2023, 3, 11, 23, 30, 0, 0, loc) }},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatalf("load zone: %v", err)
			}
			now := tc.now(loc)
			got, ok := periodResetAtLocation(quota.PeriodDay, now, loc)
			if ok && !got.After(now) {
				t.Fatalf("day end at %s = %s, which is not after now", now, got)
			}
			if !ok && !got.IsZero() {
				t.Fatalf("no answer, but a time came back: %s", got)
			}
		})
	}
}
