package middleware

import (
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// ─── Retry-After on key-limit refusals ──────────────────────────────────────
// A refusal for a key or account limit is a 429 like the ones an upstream
// sends. A client that finds no Retry-After on it falls back on its own
// backoff, which is seconds long for a limit that comes back at midnight and
// keeps hitting it all day. The gateway knows when the limit clears, so it says.
//
// Each limit reports its own boundary:
//
//   - RPM / TPM: when enough of the last minute's entries have aged out for a
//     request to pass again (slidingWindow.retryAfter, tokenWindow.retryAfter).
//   - daily request and spending limits, and the day/week/month period limits:
//     the next boundary in the usage timezone (periodResetAtFunc).
//   - the 5h period limit: five hours after the window opened.
//
// A limit with no boundary leaves the header out: a concurrency limit clears
// when a running request ends, and a lifetime allowance never clears on its own.
// So does a count that is only an estimate (the cluster-wide window, see
// admitShared). A wrong Retry-After is worse than none, because clients obey it.

// periodResetAtFunc gives the end of the calendar window of a period. Injected
// like the usage queries, to keep this package free of the usage package; the
// default knows no boundary, so the header is left out until it is wired.
var periodResetAtFunc = func(quota.Period, time.Time) (time.Time, bool) { return time.Time{}, false }

// InitQuotaPeriodResetFunc injects the calendar window boundaries of the usage
// timezone (usage.PeriodResetAt).
func InitQuotaPeriodResetFunc(resetAt func(quota.Period, time.Time) (time.Time, bool)) {
	if resetAt != nil {
		periodResetAtFunc = resetAt
	}
}

// withRetryAfter adds a Retry-After header of the wait, in whole seconds. The
// value is the smallest whole number of seconds longer than the wait, because a
// window drops an entry only once it is older than the window and a client that
// sleeps for exactly the header must not land on the edge. It is never less than
// one, so a boundary that has just passed does not become a retry loop.
// known is false when the limit has no boundary to report; the verdict is then
// left as it is.
func (v *quotaVerdict) withRetryAfter(wait time.Duration, known bool) *quotaVerdict {
	if v == nil || !known {
		return v
	}
	seconds := int64(1)
	if wait > 0 {
		seconds = int64(wait/time.Second) + 1
	}
	v.headers = append(v.headers, quotaHeader{"Retry-After", strconv.FormatInt(seconds, 10)})
	return v
}

// windowResetWait is how long until the window a limit counts over ends.
// fiveHourStart is the subject's 5h window anchor as the usage read reported it.
func windowResetWait(window quota.Period, fiveHourStart, now time.Time) (time.Duration, bool) {
	switch window {
	case quota.PeriodFiveHour:
		// The window opens with the first spend, not at a clock boundary. An
		// unopened window has no spend and cannot be over its limit.
		if fiveHourStart.IsZero() {
			return 0, false
		}
		return fiveHourStart.Add(quota.FiveHourWindowDuration).Sub(now), true
	case quota.PeriodDay, quota.PeriodWeek, quota.PeriodMonth:
		end, ok := periodResetAtFunc(window, now)
		if !ok {
			return 0, false
		}
		return end.Sub(now), true
	default:
		return 0, false
	}
}

// retryAfter is how long until a request is admitted again under limit,
// assuming nothing else arrives. The refused request was itself counted (see
// countRequest), and so will the retry be, so the retry passes only when at
// most limit-1 of the entries now in the window remain: the (n-limit+1)th
// oldest of the n entries is the last that has to expire.
func (w *slidingWindow) retryAfter(limit int, now time.Time) (time.Duration, bool) {
	cutoff := now.Add(-windowDuration)
	w.mu.Lock()
	defer w.mu.Unlock()
	live := 0
	for live < len(w.events) && w.events[live].Before(cutoff) {
		live++
	}
	events := w.events[live:]
	if limit <= 0 || len(events) <= limit {
		return 0, false
	}
	return events[len(events)-limit].Add(windowDuration).Sub(now), true
}

// retryAfter is how long until the tokens in the window fall below limit, which
// is when a request is admitted again: the oldest entries age out until what is
// left is under the limit, and the wait ends when the last of them has. The
// tokens a request will spend are only recorded once it finishes, so the retry
// itself adds nothing at admission.
func (w *tokenWindow) retryAfter(limit int64, now time.Time) (time.Duration, bool) {
	cutoff := now.Add(-windowDuration)
	w.mu.Lock()
	defer w.mu.Unlock()
	live := 0
	for live < len(w.entries) && w.entries[live].ts.Before(cutoff) {
		live++
	}
	entries := w.entries[live:]
	var total int64
	for _, entry := range entries {
		total += entry.tokens
	}
	if limit <= 0 || total < limit {
		return 0, false
	}
	for _, entry := range entries {
		total -= entry.tokens
		if total < limit {
			return entry.ts.Add(windowDuration).Sub(now), true
		}
	}
	return 0, false
}
