package usage

import (
	"sort"
	"strings"
	"time"
)

// Monitor ranges decide three things at once: which rollup bucket kind to
// read, the series step, and what "the previous period" is. Every edge is
// computed here on the usage timezone's wall clock; rollup rows are matched by
// their local-time bucket keys and request_logs rows by UTC timestamps, so SQL
// never converts timezones (production PostgreSQL has no usable 'localtime',
// see request_log_time.go).
//
// Retention bounds the choices: minute buckets live 24h, hour buckets 30 days,
// day buckets 400 days. The previous period of a range must still be inside
// the retention of the kind it reads, which is why 6h stops at minute buckets
// (12h back) and 30d switches to day buckets (60 days back).

const (
	MonitorRange1h      = "1h"
	MonitorRange6h      = "6h"
	MonitorRange24h     = "24h"
	MonitorRangeToday   = "today"
	MonitorRange7d      = "7d"
	MonitorRange14d     = "14d"
	MonitorRange30d     = "30d"
	MonitorDefaultRange = MonitorRange24h
)

type monitorStepUnit int

const (
	monitorStepMinute monitorStepUnit = iota
	monitorStepHour
	monitorStepDay
)

// monitorWindow is a resolved range. All instants are in loc.
type monitorWindow struct {
	key        string
	bucketKind string
	unit       monitorStepUnit
	stepN      int
	loc        *time.Location
	now        time.Time
	start      time.Time // inclusive
	end        time.Time // exclusive
	prevStart  time.Time
	prevEnd    time.Time
	// edges are the series bucket starts plus the final end: len(points)+1.
	edges []time.Time
}

// NormalizeMonitorRange validates a range key; empty selects the default.
func NormalizeMonitorRange(raw string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" {
		return MonitorDefaultRange, true
	}
	switch key {
	case MonitorRange1h, MonitorRange6h, MonitorRange24h, MonitorRangeToday,
		MonitorRange7d, MonitorRange14d, MonitorRange30d:
		return key, true
	}
	return "", false
}

func resolveMonitorWindow(key string, now time.Time, loc *time.Location) monitorWindow {
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	w := monitorWindow{key: key, loc: loc, now: now}
	switch key {
	case MonitorRange1h:
		w.bucketKind, w.unit, w.stepN = rollupBucketMinute, monitorStepMinute, 1
		w.end = floorLocalMinutes(now, 1).Add(time.Minute)
		w.start = w.end.Add(-time.Hour)
		w.prevStart, w.prevEnd = w.start.Add(-time.Hour), w.start
	case MonitorRange6h:
		w.bucketKind, w.unit, w.stepN = rollupBucketMinute, monitorStepMinute, 5
		w.end = floorLocalMinutes(now, 5).Add(5 * time.Minute)
		w.start = w.end.Add(-6 * time.Hour)
		w.prevStart, w.prevEnd = w.start.Add(-6*time.Hour), w.start
	case MonitorRangeToday:
		// Today runs up to the current hour and compares with the same stretch
		// of yesterday, not with all of yesterday: a morning would otherwise
		// always look like a collapse.
		w.bucketKind, w.unit, w.stepN = rollupBucketHour, monitorStepHour, 1
		w.start = startOfLocalDay(now)
		w.end = floorLocalHours(now, 1).Add(time.Hour)
		w.prevStart = addLocalDays(w.start, -1)
		w.prevEnd = w.prevStart.Add(w.end.Sub(w.start))
	case MonitorRange7d:
		w.bucketKind, w.unit, w.stepN = rollupBucketHour, monitorStepHour, 3
		w.end = floorLocalHours(now, 3).Add(3 * time.Hour)
		w.start = w.end.Add(-7 * 24 * time.Hour)
		w.prevStart, w.prevEnd = w.start.Add(-7*24*time.Hour), w.start
	case MonitorRange14d:
		w.bucketKind, w.unit, w.stepN = rollupBucketHour, monitorStepHour, 6
		w.end = floorLocalHours(now, 6).Add(6 * time.Hour)
		w.start = w.end.Add(-14 * 24 * time.Hour)
		w.prevStart, w.prevEnd = w.start.Add(-14*24*time.Hour), w.start
	case MonitorRange30d:
		w.bucketKind, w.unit, w.stepN = rollupBucketDay, monitorStepDay, 1
		w.end = addLocalDays(startOfLocalDay(now), 1)
		w.start = addLocalDays(w.end, -30)
		w.prevStart, w.prevEnd = addLocalDays(w.start, -30), w.start
	default: // MonitorRange24h
		w.key = MonitorRange24h
		w.bucketKind, w.unit, w.stepN = rollupBucketHour, monitorStepHour, 1
		w.end = floorLocalHours(now, 1).Add(time.Hour)
		w.start = w.end.Add(-24 * time.Hour)
		w.prevStart, w.prevEnd = w.start.Add(-24*time.Hour), w.start
	}
	w.edges = monitorEdges(w.start, w.end, w.unit, w.stepN)
	return w
}

// monitorEdges walks from start to end in steps. Minute and hour steps add
// absolute durations (matching how rollup keys advance); day steps follow the
// wall-clock date so a DST day still lands on local midnight.
func monitorEdges(start, end time.Time, unit monitorStepUnit, n int) []time.Time {
	edges := []time.Time{start}
	for cur := start; cur.Before(end); {
		switch unit {
		case monitorStepMinute:
			cur = cur.Add(time.Duration(n) * time.Minute)
		case monitorStepHour:
			cur = cur.Add(time.Duration(n) * time.Hour)
		default:
			cur = addLocalDays(cur, n)
		}
		if cur.After(end) {
			cur = end
		}
		edges = append(edges, cur)
	}
	return edges
}

func (w monitorWindow) stepSeconds() int64 {
	switch w.unit {
	case monitorStepMinute:
		return int64(w.stepN) * 60
	case monitorStepHour:
		return int64(w.stepN) * 3600
	default:
		return int64(w.stepN) * 86400
	}
}

func (w monitorWindow) points() int {
	if len(w.edges) < 2 {
		return 0
	}
	return len(w.edges) - 1
}

// seriesIndex returns the series bucket containing t, or -1 outside the window.
func (w monitorWindow) seriesIndex(t time.Time) int {
	if t.Before(w.start) || !t.Before(w.end) {
		return -1
	}
	i := sort.Search(len(w.edges), func(i int) bool { return w.edges[i].After(t) }) - 1
	if i < 0 || i >= w.points() {
		return -1
	}
	return i
}

func (w monitorWindow) describe() MonitorRange {
	return MonitorRange{
		Key:           w.key,
		Start:         w.start.Format(time.RFC3339),
		End:           w.end.Format(time.RFC3339),
		PreviousStart: w.prevStart.Format(time.RFC3339),
		PreviousEnd:   w.prevEnd.Format(time.RFC3339),
		StepSeconds:   w.stepSeconds(),
		Timezone:      w.loc.String(),
	}
}

// heatmapWindow covers whole local days: at least a week so short ranges still
// show a weekly rhythm, and never more than the 30 days hour buckets keep.
func (w monitorWindow) heatmapWindow() (start, end time.Time, days int) {
	days = 7
	switch w.key {
	case MonitorRange14d:
		days = 14
	case MonitorRange30d:
		days = 30
	}
	end = addLocalDays(startOfLocalDay(w.now), 1)
	return addLocalDays(end, -days), end, days
}

// rollupKeyLayout is the bucket_start format of each rollup kind; see
// rollupBucketStarts. Keys of one kind sort the same as the instants they name.
func rollupKeyLayout(kind string) string {
	switch kind {
	case rollupBucketMinute:
		return "2006-01-02T15:04"
	case rollupBucketHour:
		return "2006-01-02T15"
	default:
		return "2006-01-02"
	}
}

func rollupKey(kind string, t time.Time, loc *time.Location) string {
	return t.In(loc).Format(rollupKeyLayout(kind))
}

func parseRollupKey(kind, key string, loc *time.Location) (time.Time, bool) {
	t, err := time.ParseInLocation(rollupKeyLayout(kind), strings.TrimSpace(key), loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func floorLocalMinutes(t time.Time, n int) time.Time {
	minute := t.Minute() - t.Minute()%n
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), minute, 0, 0, t.Location())
}

func floorLocalHours(t time.Time, n int) time.Time {
	hour := t.Hour() - t.Hour()%n
	return time.Date(t.Year(), t.Month(), t.Day(), hour, 0, 0, 0, t.Location())
}

func startOfLocalDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func addLocalDays(t time.Time, days int) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day()+days, t.Hour(), t.Minute(), 0, 0, t.Location())
}
