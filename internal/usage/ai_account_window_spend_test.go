package usage

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// UTC+14 differs from UTC and from production's +08, so a bucket key computed
// in the wrong zone lands on the wrong hour or day here (see
// pg-localtime-sql-is-utc-day: production once split days at UTC).
var windowSpendTestZone = time.FixedZone("UTC+14", 14*3600)

// 2026-11-10 23:40:30 on the test zone's wall clock.
var windowSpendTestNow = time.Date(2026, 11, 10, 9, 40, 30, 0, time.UTC)

func TestWindowSpendSegmentsUseTheFinestBucketsStillKept(t *testing.T) {
	now := windowSpendTestNow
	cases := []struct {
		name  string
		start time.Time
		want  []windowSpendSegment
	}{{
		// A 5h window: minute buckets up to the next local hour, hours for the rest
		// of the local day, then days. 21:30:10 local; the start's minute counts
		// for the 50 seconds after it.
		name:  "recent start",
		start: time.Date(2026, 11, 10, 7, 30, 10, 0, time.UTC),
		want: []windowSpendSegment{
			{kind: rollupBucketQuotaMinuteUTC, from: "2026-11-10T07:30", to: "2026-11-10T08:00", firstShare: 50.0 / 60},
			{kind: rollupBucketHour, from: "2026-11-10T22", to: "2026-11-11T00", firstShare: 1},
			{kind: rollupBucketDay, from: "2026-11-11", firstShare: 1},
		},
	}, {
		// Five days back the minute buckets are gone: the start's hour bucket
		// (23:00 local) counts for the 49.5 minutes after 23:10:30.
		name:  "week-old start",
		start: time.Date(2026, 11, 5, 9, 10, 30, 0, time.UTC),
		want: []windowSpendSegment{
			{kind: rollupBucketHour, from: "2026-11-05T23", to: "2026-11-06T00", firstShare: 49.5 / 60},
			{kind: rollupBucketDay, from: "2026-11-06", firstShare: 1},
		},
	}, {
		// Past the hour buckets' 30 days only days are left; 23:40:30 local leaves
		// 19.5 minutes of the start's day.
		name:  "month-old start",
		start: now.AddDate(0, 0, -35),
		want: []windowSpendSegment{
			{kind: rollupBucketDay, from: "2026-10-06", firstShare: 19.5 / (24 * 60)},
		},
	}}
	for _, tc := range cases {
		got, ok := windowSpendSegments(tc.start, now, windowSpendTestZone)
		if !ok {
			t.Fatalf("%s: no segments", tc.name)
		}
		assertWindowSpendSegments(t, tc.name, got, tc.want)
	}

	for name, start := range map[string]time.Time{
		"future start":            now.Add(time.Minute),
		"start at now":            now,
		"older than the day keys": now.AddDate(0, 0, -400),
		"zero start":              {},
	} {
		if got, ok := windowSpendSegments(start, now, windowSpendTestZone); ok {
			t.Fatalf("%s: segments = %+v, want none", name, got)
		}
	}
}

// When DST ends the wall clock replays 01:00-02:00, and both hours land in the
// bucket keyed 01: its share has to be measured against two hours, not one.
func TestWindowSpendSegmentsSpanTheReplayedHourWhenDSTEnds(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 2026-11-01: 01:30 EDT (first pass) is 05:30 UTC, 01:30 EST is 06:30 UTC.
	for _, at := range []time.Time{
		time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC),
	} {
		start, end := localHourBucketRange(at, newYork)
		if !start.Equal(time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)) {
			t.Fatalf("hour bucket of %s = [%s, %s), want [05:00, 07:00) UTC", at, start, end)
		}
	}
	got, ok := windowSpendSegments(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), newYork)
	if !ok {
		t.Fatal("no segments")
	}
	assertWindowSpendSegments(t, "replayed hour", got, []windowSpendSegment{
		{kind: rollupBucketHour, from: "2026-11-01T01", to: "2026-11-02T00", firstShare: 0.75},
		{kind: rollupBucketDay, from: "2026-11-02", firstShare: 1},
	})
}

func assertWindowSpendSegments(t *testing.T, name string, got, want []windowSpendSegment) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: segments = %+v, want %+v", name, got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.kind != w.kind || g.from != w.from || g.to != w.to || math.Abs(g.firstShare-w.firstShare) > 1e-9 {
			t.Fatalf("%s: segment %d = %+v, want %+v", name, i, g, w)
		}
	}
}

func initWindowSpendTestDB(t *testing.T) {
	t.Helper()
	CloseDB()
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, windowSpendTestZone); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)
}

// projectWindowSpendEvent writes one request through the live rollup
// projection, so the test reads the bucket kinds and keys the writer produces.
func projectWindowSpendEvent(t *testing.T, tenant, subject, model string, at time.Time, cost float64) {
	t.Helper()
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := projectUsageRollupTx(tx, rollupEvent{
		TenantID: tenant, AuthSubjectID: subject, Model: model, Source: "test", ChannelName: "shared-label",
		Cost: cost, At: at,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryAIAccountWindowSpendSumsTheAccountAcrossTenantsAndBuckets(t *testing.T) {
	initWindowSpendTestDB(t)
	assertWindowSpendAcrossTenantsAndBuckets(t)
}

// assertWindowSpendAcrossTenantsAndBuckets seeds one account's requests in two
// tenants and checks every window's sum, proration and filter. It runs on
// SQLite here and on PostgreSQL in the _postgres test, which production uses.
func assertWindowSpendAcrossTenantsAndBuckets(t *testing.T) {
	t.Helper()
	const (
		tenantA = "aaaaaaaa-0000-0000-0000-000000000001"
		tenantB = "bbbbbbbb-0000-0000-0000-000000000002"
		account = "authsub_window_spend"
		other   = "authsub_someone_else"
	)
	now := windowSpendTestNow
	fiveHourStart := time.Date(2026, 11, 10, 7, 30, 10, 0, time.UTC)
	weekStart := time.Date(2026, 11, 5, 9, 10, 30, 0, time.UTC)

	// Both tenants send the account requests; the account's windows count both.
	projectWindowSpendEvent(t, tenantA, account, "gpt-6.1-sol", now.Add(-30*time.Minute), 1)
	projectWindowSpendEvent(t, tenantB, account, "gpt-6.1-sol", now.Add(-90*time.Minute), 2)
	// Inside the 5h window's first minute: that minute counts for 50 of its 60s.
	projectWindowSpendEvent(t, tenantA, account, "gpt-6.1-sol", time.Date(2026, 11, 10, 7, 30, 40, 0, time.UTC), 0.6)
	// Ten minutes before the 5h window, in the same local hour bucket as its start.
	projectWindowSpendEvent(t, tenantA, account, "gpt-6.1-sol", time.Date(2026, 11, 10, 7, 20, 0, 0, time.UTC), 5)
	projectWindowSpendEvent(t, tenantB, account, "claude-opus-5-5", now.Add(-48*time.Hour), 4)
	// The week's first hour bucket (23:00 local on 11-05) counts for 49.5 of 60 minutes.
	projectWindowSpendEvent(t, tenantA, account, "gpt-6.1-sol", time.Date(2026, 11, 5, 9, 40, 0, 0, time.UTC), 3)
	// Before the week began.
	projectWindowSpendEvent(t, tenantA, account, "gpt-6.1-sol", now.Add(-6*24*time.Hour), 8)
	// Another account with the same model and labels in the same tenant.
	projectWindowSpendEvent(t, tenantA, other, "gpt-6.1-sol", now.Add(-30*time.Minute), 100)

	opusOnly := func(model string) bool { return strings.Contains(model, "opus") }
	got, err := queryAIAccountWindowSpendAt(getReadDB(), []AIAccountWindowSpendRequest{
		{SubjectID: account, Window: "code_5h", Start: fiveHourStart},
		{SubjectID: account, Window: "code_week", Start: weekStart},
		{SubjectID: account, Window: "seven_day_opus", Start: weekStart, Covers: opusOnly},
		{SubjectID: other, Window: "code_5h", Start: fiveHourStart},
		{SubjectID: "authsub_never_used", Window: "code_5h", Start: fiveHourStart},
		{SubjectID: account, Window: "not_started", Start: now.Add(time.Hour)},
	}, now, windowSpendTestZone)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]map[string]float64{
		account: {
			"code_5h":        1 + 2 + 0.6*50/60,
			"code_week":      1 + 2 + 0.6 + 5 + 4 + 3*0.825,
			"seven_day_opus": 4,
		},
		other:                {"code_5h": 100},
		"authsub_never_used": {"code_5h": 0},
	}
	for subject, windows := range want {
		for window, value := range windows {
			spent, ok := got[subject][window]
			if !ok || math.Abs(spent-value) > 1e-9 {
				t.Fatalf("%s %s = %v (present %v), want %v; all: %+v", subject, window, spent, ok, value, got)
			}
		}
	}
	if _, ok := got[account]["not_started"]; ok {
		t.Fatalf("a window that has not started has no spend to report: %+v", got[account])
	}
}

// Usage an account accumulated under its old, tenant-scoped identity is moved
// onto the shared subject when the credential loads; the window spend must then
// include it, or a split account reads short for the rest of its windows.
func TestQueryAIAccountWindowSpendIncludesUsageOfAMergedLegacySubject(t *testing.T) {
	initSharedSubjectTestDB(t)
	resetMergedLegacySubjects()
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "agy.json", "", "split@example.com")
	auth.Provider = "antigravity"
	identity := ResolveAuthSubjectIdentity(auth)
	legacyID := LegacyTenantScopedAuthSubjectID(auth)
	if identity == nil || legacyID == "" || legacyID == identity.ID {
		t.Fatalf("identity %+v / legacy %q", identity, legacyID)
	}

	now := time.Now().UTC()
	seedLegacyAuthSubjectRow(t, legacyID, "antigravity", "email", now.Add(-72*time.Hour))
	projectWindowSpendEvent(t, sharedSubjectTenantA, legacyID, "gemini-3.1-flash-lite", now.Add(-time.Hour), 2)
	projectWindowSpendEvent(t, sharedSubjectTenantB, identity.ID, "gemini-3.1-flash-lite", now.Add(-30*time.Minute), 3)

	read := func() float64 {
		t.Helper()
		got, err := queryAIAccountWindowSpendAt(getReadDB(), []AIAccountWindowSpendRequest{
			{SubjectID: identity.ID, Window: "antigravity:gemini_5h", Start: now.Add(-3 * time.Hour)},
		}, now, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		return got[identity.ID]["antigravity:gemini_5h"]
	}
	if spent := read(); math.Abs(spent-3) > 1e-9 {
		t.Fatalf("before the merge = %v, want only the shared subject's 3", spent)
	}
	if err := MergeLegacySplitAuthSubject(auth, identity); err != nil {
		t.Fatal(err)
	}
	if spent := read(); math.Abs(spent-5) > 1e-9 {
		t.Fatalf("after the merge = %v, want 5 including the legacy half", spent)
	}
}

func TestMergeWindowSpendKeyRangesUnionsOverlapsOnly(t *testing.T) {
	got := mergeWindowSpendKeyRanges([]windowSpendKeyRange{
		{from: "2026-11-10T20", to: "2026-11-11T00"},
		{from: "2026-11-05T23", to: "2026-11-06T00"},
		{from: "2026-11-10T22", to: "2026-11-11T00"},
		{from: "2026-11-06T00", to: "2026-11-06T03"},
	})
	want := []windowSpendKeyRange{
		{from: "2026-11-05T23", to: "2026-11-06T03"},
		{from: "2026-11-10T20", to: "2026-11-11T00"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("merged = %+v, want %+v", got, want)
	}
	open := mergeWindowSpendKeyRanges([]windowSpendKeyRange{{from: "2026-11-06"}, {from: "2026-11-08", to: "2026-11-09"}})
	if len(open) != 1 || open[0] != (windowSpendKeyRange{from: "2026-11-06"}) {
		t.Fatalf("open-ended merge = %+v", open)
	}
}
