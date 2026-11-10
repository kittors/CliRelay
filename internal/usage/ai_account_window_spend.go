package usage

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Spend inside an account's quota windows (CliRelay#1103).
//
// A quota card shows how much of a window is left as a share, and a share alone
// cannot tell a window that shrank from one that was used harder. Showing what
// this proxy spent inside each window lets a lowered plan show up as a smaller
// dollar figure behind the same percentage.
//
// The figure is the cost this proxy recorded for the account, priced by its own
// model price table (an estimate, not the upstream's bill), from the window's
// start (renewal time minus window length) to now. It is summed from
// usage_rollup_buckets when asked for instead of being accumulated per window:
// a window's start is only known from the latest probe and drifts between
// probes, and the weekly cycle buckets that are anchored at write time need
// realignment, rollover repair and merge passes to stay right. Summing time
// buckets against the start as it stands now leaves nothing to repair.
//
// Granularity: whole local days after the start's own day come from day
// buckets, the rest of that day from hour buckets, and the stretch before the
// first full hour from UTC minute buckets. The one bucket the start falls inside
// is prorated by the share of it inside the window. Minute buckets are kept for
// a day and hour buckets for 30 days; a window starting earlier than either
// covers its first stretch with the next coarser bucket, prorated the same way,
// so a week-old start is accurate to its hour, which is finer than the drift of
// the start itself. Request logs are not read: their retention is configurable
// and capped by row count (production keeps under five days), so they cannot
// answer for a weekly window's start.
//
// A subject is the physical upstream account, so the sum runs across every
// tenant that sent it requests, like the card's other usage figures.

// AIAccountWindowSpendRequest asks what one account spent inside one window.
type AIAccountWindowSpendRequest struct {
	SubjectID string
	// Window names the window in the result. A subject reports each quota key
	// once, so the key is enough.
	Window string
	// Start is when the window began; spend runs from here to now.
	Start time.Time
	// Covers reports whether requests for model draw on the window. Nil means
	// every request of the account does.
	Covers func(model string) bool
}

// Rollup buckets are pruned by key when cleanup runs, so each kind is trusted
// only well inside its retention.
const (
	windowSpendMinuteHorizon = rollupMinuteRetention - time.Hour
	windowSpendHourHorizon   = rollupHourRetention - 24*time.Hour
	windowSpendDayHorizon    = rollupDayRetention - 24*time.Hour
)

const (
	windowSpendDayLayout       = "2006-01-02"
	windowSpendHourLayout      = "2006-01-02T15"
	windowSpendUTCMinuteLayout = "2006-01-02T15:04"
)

// windowSpendSegment is a run of buckets of one kind with keys in [from, to);
// an empty to runs up to now. The bucket keyed from is the one the window
// starts inside, and counts for firstShare of its cost.
type windowSpendSegment struct {
	kind       string
	from, to   string
	firstShare float64
}

// QueryAIAccountWindowSpend returns subject -> window -> USD spent from each
// request's Start to now. Windows that cannot be answered — a start in the
// future or older than the day buckets go back — are left out, so a missing
// entry means "unknown" and never "nothing spent".
func QueryAIAccountWindowSpend(requests []AIAccountWindowSpendRequest) (map[string]map[string]float64, error) {
	return queryAIAccountWindowSpendAt(getReadDB(), requests, time.Now(), getUsageLocation())
}

func queryAIAccountWindowSpendAt(db *sql.DB, requests []AIAccountWindowSpendRequest, now time.Time, loc *time.Location) (map[string]map[string]float64, error) {
	out := make(map[string]map[string]float64)
	if db == nil || len(requests) == 0 {
		return out, nil
	}
	type plannedWindow struct {
		request  AIAccountWindowSpendRequest
		segments []windowSpendSegment
	}
	planned := make([]plannedWindow, 0, len(requests))
	for _, request := range requests {
		request.SubjectID = strings.TrimSpace(request.SubjectID)
		if request.SubjectID == "" || request.Window == "" {
			continue
		}
		segments, ok := windowSpendSegments(request.Start, now, loc)
		if !ok {
			continue
		}
		planned = append(planned, plannedWindow{request: request, segments: segments})
	}
	if len(planned) == 0 {
		return out, nil
	}

	// Each subject's windows overlap (a 5h window lies inside the week), so the
	// key ranges they need are merged per bucket kind before fetching: fewer,
	// disjoint ranges to scan, and each window then picks its own range below.
	ranges := make(map[windowSpendBucketKey][]windowSpendKeyRange)
	for _, window := range planned {
		for _, segment := range window.segments {
			key := windowSpendBucketKey{subject: window.request.SubjectID, kind: segment.kind}
			ranges[key] = append(ranges[key], windowSpendKeyRange{from: segment.from, to: segment.to})
		}
	}
	arms := make([]windowSpendArm, 0, len(ranges))
	for key, keyRanges := range ranges {
		for _, merged := range mergeWindowSpendKeyRanges(keyRanges) {
			arms = append(arms, windowSpendArm{windowSpendBucketKey: key, windowSpendKeyRange: merged})
		}
	}
	buckets, err := queryWindowSpendBuckets(db, arms)
	if err != nil {
		return nil, err
	}

	for _, window := range planned {
		var total float64
		for _, segment := range window.segments {
			for _, bucket := range buckets[windowSpendBucketKey{subject: window.request.SubjectID, kind: segment.kind}] {
				if bucket.start < segment.from || (segment.to != "" && bucket.start >= segment.to) {
					continue
				}
				if window.request.Covers != nil && !window.request.Covers(bucket.model) {
					continue
				}
				share := 1.0
				if bucket.start == segment.from {
					share = segment.firstShare
				}
				total += bucket.cost * share
			}
		}
		bySubject := out[window.request.SubjectID]
		if bySubject == nil {
			bySubject = make(map[string]float64)
			out[window.request.SubjectID] = bySubject
		}
		bySubject[window.request.Window] = total
	}
	return out, nil
}

// windowSpendSegments splits [start, now] into bucket runs, finest first. It
// reports false when the window has not started or starts before the day
// buckets go back.
func windowSpendSegments(start, now time.Time, loc *time.Location) ([]windowSpendSegment, bool) {
	if loc == nil {
		loc = time.Local
	}
	start, now = start.UTC(), now.UTC()
	if start.IsZero() || !start.Before(now) || start.Before(now.Add(-windowSpendDayHorizon)) {
		return nil, false
	}
	dayStart, dayEnd := localDayBucketRange(start, loc)
	nextDayKey := dayEnd.In(loc).Format(windowSpendDayLayout)
	if start.Before(now.Add(-windowSpendHourHorizon)) {
		return []windowSpendSegment{{
			kind: rollupBucketDay, from: dayStart.In(loc).Format(windowSpendDayLayout),
			firstShare: bucketShareFrom(start, dayStart, dayEnd, now),
		}}, true
	}
	laterDays := windowSpendSegment{kind: rollupBucketDay, from: nextDayKey, firstShare: 1}
	// Hour keys of the start's own day sort below the next day's "T00".
	restOfDay := nextDayKey + "T00"
	hourStart, hourEnd := localHourBucketRange(start, loc)
	if start.Before(now.Add(-windowSpendMinuteHorizon)) {
		return []windowSpendSegment{{
			kind: rollupBucketHour, from: hourStart.In(loc).Format(windowSpendHourLayout), to: restOfDay,
			firstShare: bucketShareFrom(start, hourStart, hourEnd, now),
		}, laterDays}, true
	}
	minuteStart := start.Truncate(time.Minute)
	return []windowSpendSegment{{
		kind: rollupBucketQuotaMinuteUTC, from: minuteStart.Format(windowSpendUTCMinuteLayout),
		to:         hourEnd.UTC().Format(windowSpendUTCMinuteLayout),
		firstShare: bucketShareFrom(start, minuteStart, minuteStart.Add(time.Minute), now),
	}, {
		kind: rollupBucketHour, from: hourEnd.In(loc).Format(windowSpendHourLayout), to: restOfDay, firstShare: 1,
	}, laterDays}, true
}

// bucketShareFrom is the share of a bucket spanning [bucketStart, bucketEnd)
// that lies at or after start. A bucket still filling holds requests only up
// to now, so its share is measured against that.
func bucketShareFrom(start, bucketStart, bucketEnd, now time.Time) float64 {
	end := bucketEnd
	if now.Before(end) {
		end = now
	}
	span := end.Sub(bucketStart)
	inside := end.Sub(start)
	switch {
	case span <= 0 || inside <= 0:
		return 0
	case inside >= span:
		return 1
	}
	return float64(inside) / float64(span)
}

// localHourBucketRange returns the span of the hour bucket containing t. Hour
// buckets are keyed by the wall clock, so the hour replayed when DST ends lands
// in one bucket twice as long as the rest.
func localHourBucketRange(t time.Time, loc *time.Location) (time.Time, time.Time) {
	key := t.In(loc).Format(windowSpendHourLayout)
	floor := floorLocalHour(t, loc)
	start := floor
	for previous := start.Add(-time.Hour); previous.In(loc).Format(windowSpendHourLayout) == key; previous = previous.Add(-time.Hour) {
		start = previous
	}
	end := floor.Add(time.Hour)
	for end.In(loc).Format(windowSpendHourLayout) == key {
		end = end.Add(time.Hour)
	}
	return start.UTC(), end.UTC()
}

// localDayBucketRange returns the span of the day bucket containing t. Where a
// DST change skips midnight the day starts at the first hour that exists.
func localDayBucketRange(t time.Time, loc *time.Location) (time.Time, time.Time) {
	local := t.In(loc)
	return firstInstantOfLocalDay(local.Year(), local.Month(), local.Day(), loc),
		firstInstantOfLocalDay(local.Year(), local.Month(), local.Day()+1, loc)
}

func firstInstantOfLocalDay(year int, month time.Month, day int, loc *time.Location) time.Time {
	midnight := time.Date(year, month, day, 0, 0, 0, 0, loc)
	key := time.Date(year, month, day, 12, 0, 0, 0, loc).Format(windowSpendDayLayout)
	// time.Date may resolve a skipped midnight to the previous day's last hour.
	for i := 0; i < 3 && midnight.In(loc).Format(windowSpendDayLayout) != key; i++ {
		midnight = midnight.Add(time.Hour)
	}
	return midnight.UTC()
}

type windowSpendBucketKey struct {
	subject, kind string
}

// windowSpendKeyRange is the bucket keys [from, to); an empty to is unbounded.
type windowSpendKeyRange struct {
	from, to string
}

type windowSpendArm struct {
	windowSpendBucketKey
	windowSpendKeyRange
}

type windowSpendBucket struct {
	model, start string
	cost         float64
}

// mergeWindowSpendKeyRanges unions overlapping or touching ranges.
func mergeWindowSpendKeyRanges(ranges []windowSpendKeyRange) []windowSpendKeyRange {
	sorted := slices.Clone(ranges)
	slices.SortFunc(sorted, func(a, b windowSpendKeyRange) int { return strings.Compare(a.from, b.from) })
	merged := make([]windowSpendKeyRange, 0, len(sorted))
	for _, next := range sorted {
		if n := len(merged); n > 0 {
			last := &merged[n-1]
			if last.to == "" || next.from <= last.to {
				if last.to != "" && (next.to == "" || next.to > last.to) {
					last.to = next.to
				}
				continue
			}
		}
		merged = append(merged, next)
	}
	return merged
}

// queryWindowSpendBuckets reads the buckets the arms name, summed over the
// dimensions spend does not need (tenant, key, end user, source, channel).
//
// One small query per subject, each a few range scans of
// idx_usage_rollup_subject_kind_start with no tenant in them. Batching every
// subject into one statement was measured on PostgreSQL and lost each time: an
// OR of 128 arms took 40 ms to plan, a VALUES list joined to the table was
// planned as a hash join over a full scan (the planner prices random heap reads
// above a scan of the whole table), and a UNION ALL went parallel and spent its
// time starting workers. A per-subject query plans in a fraction of a
// millisecond and reads only its own rows.
func queryWindowSpendBuckets(db *sql.DB, arms []windowSpendArm) (map[windowSpendBucketKey][]windowSpendBucket, error) {
	out := make(map[windowSpendBucketKey][]windowSpendBucket)
	bySubject := make(map[string][]windowSpendArm)
	subjects := make([]string, 0, len(arms))
	for _, arm := range arms {
		if _, seen := bySubject[arm.subject]; !seen {
			subjects = append(subjects, arm.subject)
		}
		bySubject[arm.subject] = append(bySubject[arm.subject], arm)
	}
	for _, subject := range subjects {
		var where strings.Builder
		args := []any{subject}
		for i, arm := range bySubject[subject] {
			if i > 0 {
				where.WriteString(" OR ")
			}
			where.WriteString("(bucket_kind = ? AND bucket_start >= ?")
			args = append(args, arm.kind, arm.from)
			if arm.to != "" {
				where.WriteString(" AND bucket_start < ?")
				args = append(args, arm.to)
			}
			where.WriteString(")")
		}
		rows, err := db.Query(`
			SELECT bucket_kind, model, bucket_start, COALESCE(SUM(cost_total), 0)
			FROM usage_rollup_buckets
			WHERE auth_subject_id = ? AND (`+where.String()+`)
			GROUP BY bucket_kind, model, bucket_start
		`, args...)
		if err != nil {
			return nil, fmt.Errorf("usage: window spend buckets: %w", err)
		}
		for rows.Next() {
			key := windowSpendBucketKey{subject: subject}
			var bucket windowSpendBucket
			if err := rows.Scan(&key.kind, &bucket.model, &bucket.start, &bucket.cost); err != nil {
				rows.Close()
				return nil, fmt.Errorf("usage: window spend buckets: %w", err)
			}
			out[key] = append(out[key], bucket)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
