package usage

import (
	"fmt"
	"strings"
	"time"
)

func cutoffStartUTCAt(now time.Time, days int) time.Time {
	return cutoffStartUTCAtLocation(now, days, getUsageLocation())
}

func cutoffStartUTCAtLocation(now time.Time, days int, loc *time.Location) time.Time {
	if days < 1 {
		days = 7
	}
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	todayStartLocal := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return todayStartLocal.AddDate(0, 0, -(days - 1)).UTC()
}

// CutoffStartUTC returns the start-of-day cutoff for the given number of days
// in the project-configured timezone, converted to UTC. Exported so that
// dashboard and other callers can reuse the same time-range semantics.
func CutoffStartUTC(days int) time.Time {
	return cutoffStartUTCAt(time.Now(), days)
}

func localDayKeyAt(t time.Time) string {
	return localDayKeyAtLocation(t, getUsageLocation())
}

func localDayKeyAtLocation(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02")
}

// LocalDayKeyAt returns the YYYY-MM-DD day key in the project-configured timezone.
func LocalDayKeyAt(t time.Time) string {
	return localDayKeyAt(t)
}

func cutoffDayKey(days int) string {
	return localDayKeyAt(CutoffStartUTC(days))
}

// Chart day and hour keys must follow the usage timezone so they line up with
// Go-built slots and usage_rollup_buckets day keys. SQL cannot key them on its
// own: SQLite's localtime modifier reads the process TZ, and PostgreSQL's
// AT TIME ZONE needs a zone name that the default time.Local does not carry.
// So bucket edges are computed here and SQL only compares stored UTC
// timestamps against them, which both drivers agree on.

// localDayBoundsAt returns days+1 local midnights in loc, oldest first: bucket i
// covers [bounds[i], bounds[i+1]) and the last element is the next local
// midnight. Built from the calendar so 23h and 25h DST days keep true edges.
func localDayBoundsAt(now time.Time, days int, loc *time.Location) []time.Time {
	if days < 1 {
		days = 7
	}
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	bounds := make([]time.Time, days+1)
	for i := range bounds {
		bounds[i] = time.Date(now.Year(), now.Month(), now.Day()-(days-1)+i, 0, 0, 0, 0, loc)
	}
	return bounds
}

// LocalDayKeys returns the day keys of the last days days in the usage
// timezone, oldest first: the slots that day-bucketed chart keys land in.
func LocalDayKeys(days int) []string {
	return localDayKeysAt(time.Now(), days, getUsageLocation())
}

func localDayKeysAt(now time.Time, days int, loc *time.Location) []string {
	bounds := localDayBoundsAt(now, days, loc)
	keys := make([]string, 0, len(bounds)-1)
	for _, start := range bounds[:len(bounds)-1] {
		keys = append(keys, start.Format("2006-01-02"))
	}
	return keys
}

// localHourBoundsAt returns local hour edges from the hour containing from to
// the hour after the one containing now.
func localHourBoundsAt(from, now time.Time, loc *time.Location) []time.Time {
	if loc == nil {
		loc = time.Local
	}
	end := floorLocalHour(now, loc).Add(time.Hour)
	var bounds []time.Time
	for edge := floorLocalHour(from, loc); !edge.After(end); edge = edge.Add(time.Hour) {
		bounds = append(bounds, edge)
	}
	return bounds
}

// floorLocalHour truncates t to the start of its hour on loc's wall clock.
// time.Truncate works on absolute time and lands mid-hour for +05:45 or +09:30.
func floorLocalHour(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	local := t.In(loc)
	return local.Add(-time.Duration(local.Minute())*time.Minute -
		time.Duration(local.Second())*time.Second -
		time.Duration(local.Nanosecond()))
}

// timeBucketCase renders a CASE expression that maps column to the index of its
// [bounds[i], bounds[i+1]) bucket, plus the keys those indexes stand for, each
// formatted in its bound's own location. Consecutive buckets with the same key
// (the hour replayed when DST ends) share an index so GROUP BY merges them as
// the wall clock reads. Callers must limit column to
// [bounds[0], bounds[len(bounds)-1]) in WHERE, and pass the returned args ahead
// of the WHERE args because the expression is rendered first.
func timeBucketCase(column string, bounds []time.Time, layout string) (string, []interface{}, []string) {
	var expr strings.Builder
	expr.WriteString("CASE")
	args := make([]interface{}, 0, len(bounds))
	keys := make([]string, 0, len(bounds))
	for i := 0; i+1 < len(bounds); i++ {
		if key := bounds[i].Format(layout); len(keys) == 0 || keys[len(keys)-1] != key {
			keys = append(keys, key)
		}
		fmt.Fprintf(&expr, " WHEN %s < ? THEN %d", column, len(keys)-1)
		args = append(args, bounds[i+1].UTC().Format(time.RFC3339))
	}
	expr.WriteString(" END")
	return expr.String(), args, keys
}
