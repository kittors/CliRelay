package usagelogs

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func percentPtr(v float64) *float64 { return &v }

func quotaSeries(quotaKey string, windowSeconds int64, points ...usage.QuotaSnapshotSeriesPoint) usage.QuotaSnapshotSeries {
	return usage.QuotaSnapshotSeries{QuotaKey: quotaKey, WindowSeconds: windowSeconds, Points: points}
}

// A live SuperGrok account: 19% of the shared weekly pool spent, of which 16%
// is Grok Build (this proxy) and 3% is Grok Chat (the web app). The panel shows
// 19% consumed, but the budget projection divides Grok Build cost, so it must
// divide by 16.
func TestProjectionQuotaUsedIgnoresNonAttributableConsumption(t *testing.T) {
	t.Parallel()

	older := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	series := []usage.QuotaSnapshotSeries{
		quotaSeries("weekly_limit", 604800,
			usage.QuotaSnapshotSeriesPoint{Timestamp: older, Percent: percentPtr(88)},
			usage.QuotaSnapshotSeriesPoint{Timestamp: newer, Percent: percentPtr(81)},
		),
		quotaSeries("product:GrokBuild", 604800,
			usage.QuotaSnapshotSeriesPoint{Timestamp: older, Percent: percentPtr(90)},
			usage.QuotaSnapshotSeriesPoint{Timestamp: newer, Percent: percentPtr(84)},
		),
		// Not window-tagged by the probe, so it can never be selected even
		// though it is a product window.
		quotaSeries("product:GrokChat", 0,
			usage.QuotaSnapshotSeriesPoint{Timestamp: newer, Percent: percentPtr(97)},
		),
	}

	weekly := latestWeeklyQuotaUsedPercent(series, "weekly_limit")
	if weekly == nil || *weekly != 19 {
		t.Fatalf("pool-wide used=%v, want 19", weekly)
	}
	projection := latestProjectionQuotaUsedPercent(series, "xai")
	if projection == nil || *projection != 16 {
		t.Fatalf("projection used=%v, want 16", projection)
	}

	// The concrete regression: $128.4874 of recorded Grok Build cost.
	const cost = 128.4874
	if got := cost / (*weekly / 100); got > 700 {
		t.Fatalf("sanity: pool-wide divisor should understate the budget, got %v", got)
	}
	if got := cost / (*projection / 100); got < 800 {
		t.Fatalf("attributable divisor budget=%v, want the pool-sized ~803", got)
	}
}

// Without a product breakdown there is nothing attributable to divide by; the
// caller has to be told so it can fall back rather than divide by a zero this
// function invented.
func TestProjectionQuotaUsedNilWithoutAttributableWindow(t *testing.T) {
	t.Parallel()

	series := []usage.QuotaSnapshotSeries{
		quotaSeries("weekly_limit", 604800,
			usage.QuotaSnapshotSeriesPoint{
				Timestamp: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
				Percent:   percentPtr(81),
			},
		),
	}
	if got := latestProjectionQuotaUsedPercent(series, "xai"); got != nil {
		t.Fatalf("projection used=%v, want nil", *got)
	}
}

// Providers that bill one pool per surface keep using their primary window, so
// the projection is unchanged for them.
func TestProjectionQuotaUsedMatchesPrimaryWindowForOtherProviders(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	series := []usage.QuotaSnapshotSeries{
		quotaSeries("code_week", 604800, usage.QuotaSnapshotSeriesPoint{Timestamp: at, Percent: percentPtr(40)}),
		quotaSeries("code_5h", 18000, usage.QuotaSnapshotSeriesPoint{Timestamp: at, Percent: percentPtr(10)}),
	}
	projection := latestProjectionQuotaUsedPercent(series, "codex")
	weekly := latestWeeklyQuotaUsedPercent(series, "code_week")
	if projection == nil || weekly == nil || *projection != *weekly {
		t.Fatalf("codex projection=%v weekly=%v, want equal", projection, weekly)
	}
	if *projection != 60 {
		t.Fatalf("codex projection used=%v, want 60", *projection)
	}
}

// Several attributable windows are several slices of one pool, so the divisor
// is their sum — taking whichever was written last would drop the rest.
func TestProjectionQuotaUsedSumsAttributableWindows(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	series := []usage.QuotaSnapshotSeries{
		quotaSeries("product:GrokBuild", 604800, usage.QuotaSnapshotSeriesPoint{Timestamp: at, Percent: percentPtr(90)}),
		quotaSeries("product:grok-code", 604800, usage.QuotaSnapshotSeriesPoint{Timestamp: at, Percent: percentPtr(95)}),
	}
	got := latestProjectionQuotaUsedPercent(series, "xai")
	if got == nil || *got != 15 {
		t.Fatalf("summed projection used=%v, want 15", got)
	}
}
