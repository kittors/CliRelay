package usage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// SQL-side 'localtime' follows the SQLite process TZ and has no PostgreSQL
// counterpart, so keys built with it pass SQLite tests and drift from the
// usage timezone in production. Bucket against timeBucketCase edges instead.
func TestUsageSQLDoesNotUseLocaltime(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "'localtime'") {
				t.Errorf("%s: SQL uses 'localtime'; bucket against timeBucketCase edges instead", fset.Position(lit.Pos()))
			}
			return true
		})
	}
}

func TestLocalDayBoundsAtKeepsDSTDayLengths(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// DST ends 2026-11-01 02:00 EDT, so that local day runs 25 hours.
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, loc)

	bounds := localDayBoundsAt(now, 3, loc)

	want := []time.Time{
		time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC), // 10-31 00:00 EDT
		time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC),  // 11-01 00:00 EDT
		time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC),  // 11-02 00:00 EST
		time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC),  // 11-03 00:00 EST
	}
	if len(bounds) != len(want) {
		t.Fatalf("bounds = %v, want %v", bounds, want)
	}
	for i := range want {
		if !bounds[i].Equal(want[i]) {
			t.Fatalf("bounds[%d] = %s, want %s", i, bounds[i].UTC().Format(time.RFC3339), want[i].Format(time.RFC3339))
		}
	}
	if cutoff := cutoffStartUTCAtLocation(now, 3, loc); !bounds[0].Equal(cutoff) {
		t.Fatalf("first bound %s disagrees with cutoffStartUTCAtLocation %s", bounds[0].UTC().Format(time.RFC3339), cutoff.Format(time.RFC3339))
	}
}

// Regression: the AI Accounts trend stepped 24h from a UTC cutoff to name its
// slots, which repeated 11-01 after the 25-hour DST day and left no slot for
// today, so a week of trends in DST zones dropped the current day's usage.
func TestLocalDayKeysAtWalksTheLocalCalendarAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, loc)

	got := localDayKeysAt(now, 4, loc)

	want := []string{"2026-10-31", "2026-11-01", "2026-11-02", "2026-11-03"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("day keys = %v, want %v", got, want)
	}
}

func TestLocalHourBoundsAtFollowsWallClockForQuarterHourOffsets(t *testing.T) {
	loc := time.FixedZone("UTC+05:45", 5*3600+45*60)
	from := time.Date(2026, 9, 22, 18, 20, 0, 0, time.UTC) // 00:05 local
	now := time.Date(2026, 9, 22, 20, 40, 0, 0, time.UTC)  // 02:25 local

	bounds := localHourBoundsAt(from, now, loc)

	want := []time.Time{
		time.Date(2026, 9, 22, 18, 15, 0, 0, time.UTC), // 00:00 local
		time.Date(2026, 9, 22, 19, 15, 0, 0, time.UTC),
		time.Date(2026, 9, 22, 20, 15, 0, 0, time.UTC),
		time.Date(2026, 9, 22, 21, 15, 0, 0, time.UTC), // 03:00 local, exclusive end
	}
	if len(bounds) != len(want) {
		t.Fatalf("bounds = %v, want %v", bounds, want)
	}
	for i := range want {
		if !bounds[i].Equal(want[i]) {
			t.Fatalf("bounds[%d] = %s, want %s", i, bounds[i].UTC().Format(time.RFC3339), want[i].Format(time.RFC3339))
		}
	}
}

func TestTimeBucketCaseMergesRepeatedLocalHour(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	from := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC) // 00:00 EDT
	now := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC) // 02:30 EST, after 01:00 replays

	expr, args, keys := timeBucketCase("ts", localHourBoundsAt(from, now, loc), "2006-01-02 15:04")

	wantKeys := []string{"2026-11-01 00:00", "2026-11-01 01:00", "2026-11-01 02:00"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("keys = %v, want %v", keys, wantKeys)
	}
	// Both 01:00 hours (EDT then EST) map to index 1.
	wantExpr := "CASE WHEN ts < ? THEN 0 WHEN ts < ? THEN 1 WHEN ts < ? THEN 1 WHEN ts < ? THEN 2 END"
	if expr != wantExpr {
		t.Fatalf("expr = %q, want %q", expr, wantExpr)
	}
	wantArgs := []interface{}{
		"2026-11-01T05:00:00Z",
		"2026-11-01T06:00:00Z",
		"2026-11-01T07:00:00Z",
		"2026-11-01T08:00:00Z",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
}
