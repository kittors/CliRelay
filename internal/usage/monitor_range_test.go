package usage

import (
	"testing"
	"time"
)

func TestNormalizeMonitorRange(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"":      {MonitorDefaultRange, true},
		"7D":    {MonitorRange7d, true},
		" 1h ":  {MonitorRange1h, true},
		"today": {MonitorRangeToday, true},
		"90d":   {"", false},
		"7":     {"", false},
	}
	for raw, tc := range cases {
		got, ok := NormalizeMonitorRange(raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeMonitorRange(%q) = %q,%v want %q,%v", raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResolveMonitorWindowEdges(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 7, 13, 47, 30, 0, cst)
	at := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, cst)
	}
	cases := []struct {
		key                string
		kind               string
		start, end         time.Time
		prevStart, prevEnd time.Time
		points             int
		stepSeconds        int64
	}{
		{MonitorRange1h, rollupBucketMinute, at(10, 7, 12, 48), at(10, 7, 13, 48), at(10, 7, 11, 48), at(10, 7, 12, 48), 60, 60},
		{MonitorRange6h, rollupBucketMinute, at(10, 7, 7, 50), at(10, 7, 13, 50), at(10, 7, 1, 50), at(10, 7, 7, 50), 72, 300},
		{MonitorRange24h, rollupBucketHour, at(10, 6, 14, 0), at(10, 7, 14, 0), at(10, 5, 14, 0), at(10, 6, 14, 0), 24, 3600},
		// Today compares with the same stretch of yesterday, not all of it.
		{MonitorRangeToday, rollupBucketHour, at(10, 7, 0, 0), at(10, 7, 14, 0), at(10, 6, 0, 0), at(10, 6, 14, 0), 14, 3600},
		{MonitorRange7d, rollupBucketHour, at(9, 30, 15, 0), at(10, 7, 15, 0), at(9, 23, 15, 0), at(9, 30, 15, 0), 56, 3 * 3600},
		{MonitorRange14d, rollupBucketHour, at(9, 23, 18, 0), at(10, 7, 18, 0), at(9, 9, 18, 0), at(9, 23, 18, 0), 56, 6 * 3600},
		{MonitorRange30d, rollupBucketDay, at(9, 8, 0, 0), at(10, 8, 0, 0), at(8, 9, 0, 0), at(9, 8, 0, 0), 30, 86400},
	}
	for _, tc := range cases {
		w := resolveMonitorWindow(tc.key, now, cst)
		if w.bucketKind != tc.kind {
			t.Errorf("%s bucket kind = %s, want %s", tc.key, w.bucketKind, tc.kind)
		}
		if !w.start.Equal(tc.start) || !w.end.Equal(tc.end) {
			t.Errorf("%s window = [%s, %s), want [%s, %s)", tc.key, w.start, w.end, tc.start, tc.end)
		}
		if !w.prevStart.Equal(tc.prevStart) || !w.prevEnd.Equal(tc.prevEnd) {
			t.Errorf("%s previous = [%s, %s), want [%s, %s)", tc.key, w.prevStart, w.prevEnd, tc.prevStart, tc.prevEnd)
		}
		if w.points() != tc.points {
			t.Errorf("%s points = %d, want %d", tc.key, w.points(), tc.points)
		}
		if w.stepSeconds() != tc.stepSeconds {
			t.Errorf("%s step = %d, want %d", tc.key, w.stepSeconds(), tc.stepSeconds)
		}
		if got := w.edges[len(w.edges)-1]; !got.Equal(w.end) {
			t.Errorf("%s last edge = %s, want end %s", tc.key, got, w.end)
		}
	}
}

func TestResolveMonitorWindowFloorsOnLocalWallClock(t *testing.T) {
	// Half-hour offsets are where time.Truncate on absolute time goes wrong.
	ist := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 10, 7, 13, 47, 30, 0, ist)
	w := resolveMonitorWindow(MonitorRange7d, now, ist)
	if want := time.Date(2026, 10, 7, 15, 0, 0, 0, ist); !w.end.Equal(want) {
		t.Fatalf("7d end = %s, want %s", w.end, want)
	}
	for i, edge := range w.edges {
		if edge.In(ist).Minute() != 0 || edge.In(ist).Hour()%3 != 0 {
			t.Fatalf("edge %d = %s is not on a local 3-hour boundary", i, edge.In(ist))
		}
	}
}

func TestMonitorWindowSeriesIndex(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	w := resolveMonitorWindow(MonitorRange24h, time.Date(2026, 10, 7, 13, 47, 30, 0, cst), cst)
	if got := w.seriesIndex(w.start); got != 0 {
		t.Fatalf("index(start) = %d, want 0", got)
	}
	if got := w.seriesIndex(w.end.Add(-time.Nanosecond)); got != w.points()-1 {
		t.Fatalf("index(end-1ns) = %d, want %d", got, w.points()-1)
	}
	if got := w.seriesIndex(w.end); got != -1 {
		t.Fatalf("index(end) = %d, want -1", got)
	}
	if got := w.seriesIndex(w.start.Add(-time.Nanosecond)); got != -1 {
		t.Fatalf("index(start-1ns) = %d, want -1", got)
	}
}

func TestMonitorHeatmapWindowCoversWholeLocalDays(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 7, 13, 47, 30, 0, cst)
	for key, wantDays := range map[string]int{MonitorRange1h: 7, MonitorRange7d: 7, MonitorRange14d: 14, MonitorRange30d: 30} {
		start, end, days := resolveMonitorWindow(key, now, cst).heatmapWindow()
		if days != wantDays {
			t.Errorf("%s heatmap days = %d, want %d", key, days, wantDays)
		}
		if want := time.Date(2026, 10, 8, 0, 0, 0, 0, cst); !end.Equal(want) {
			t.Errorf("%s heatmap end = %s, want %s", key, end, want)
		}
		if want := time.Date(2026, 10, 8-wantDays, 0, 0, 0, 0, cst); !start.Equal(want) {
			t.Errorf("%s heatmap start = %s, want %s", key, start, want)
		}
	}
}
