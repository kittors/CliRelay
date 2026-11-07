package retryhint

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxFloatSeconds keeps a fractional wait clear of Duration overflow: times
	// 1e9 it stays well under math.MaxInt64 nanoseconds (about 285 years).
	maxFloatSeconds = 9.0e9

	// An epoch reads as seconds from 2001-09-09 on and as milliseconds from
	// 2001-09-09 expressed in milliseconds on. Anything smaller is a relative
	// value. The two ranges are far apart (a seconds epoch will not reach 1e12
	// for thousands of years), so the guess cannot go wrong for a real timestamp.
	epochSecondsFloor = 1e9
	epochMillisFloor  = 1e12
	// epochMillisCeil rejects values that would overflow UnixMilli's range.
	epochMillisCeil = 1e15
)

// parseNumber reads a finite decimal number. NaN and the infinities parse as
// valid floats, so they are rejected here rather than at every caller.
func parseNumber(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// secondsToDuration converts a possibly fractional number of seconds.
func secondsToDuration(seconds float64) (time.Duration, bool) {
	if math.IsNaN(seconds) || seconds <= 0 || seconds > maxFloatSeconds {
		return 0, false
	}
	d := time.Duration(seconds * float64(time.Second))
	return d, d > 0
}

// millisToDuration converts a possibly fractional number of milliseconds.
func millisToDuration(millis float64) (time.Duration, bool) {
	if math.IsNaN(millis) || millis <= 0 || millis > maxFloatSeconds*1000 {
		return 0, false
	}
	d := time.Duration(millis * float64(time.Millisecond))
	return d, d > 0
}

// epochToTime reads an epoch given in seconds or milliseconds.
func epochToTime(value float64) (time.Time, bool) {
	switch {
	case math.IsNaN(value) || math.IsInf(value, 0) || value < epochSecondsFloor:
		return time.Time{}, false
	case value >= epochMillisCeil:
		return time.Time{}, false
	case value >= epochMillisFloor:
		return time.UnixMilli(int64(value)), true
	default:
		whole, fraction := math.Modf(value)
		return time.Unix(int64(whole), int64(fraction*1e9)), true
	}
}

// untilInstant returns the wait until a moment, or false when it has passed.
func untilInstant(at, now time.Time) (time.Duration, bool) {
	if !at.After(now) {
		return 0, false
	}
	return at.Sub(now), true
}

// parseRetryAfterValue reads one Retry-After value: whole seconds, fractional
// seconds, an HTTP date, or a Go-style duration ("30s", "1m30s"). Some gateways
// send the latter two although the standard allows only the first two.
//
// The only limit applied here is that the wait must fit a time.Duration:
// seconds are range-checked before the multiplication, which would otherwise
// wrap into a negative or tiny wait. Policy bounds (MaxDelay) are the caller's.
func parseRetryAfterValue(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		return secondsToDuration(seconds)
	}
	if when, err := http.ParseTime(raw); err == nil {
		return untilInstant(when, now)
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d, true
	}
	return 0, false
}

// parseResetValue reads the value of a rate-limit reset header. Vendors disagree
// on what it means, so the shape decides: a duration with units (OpenAI's
// "6m0s"), an RFC 3339 timestamp (Anthropic), an epoch in seconds or
// milliseconds (GitHub style), or else a relative number of seconds (the IETF
// draft's RateLimit-Reset).
func parseResetValue(raw string, now time.Time) (delay time.Duration, resetAt time.Time, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, time.Time{}, false
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d, time.Time{}, d > 0
	}
	if at, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		delay, ok = untilInstant(at, now)
		return delay, at, ok
	}
	value, isNumber := parseNumber(raw)
	if !isNumber {
		return 0, time.Time{}, false
	}
	if at, isEpoch := epochToTime(value); isEpoch {
		delay, ok = untilInstant(at, now)
		return delay, at, ok
	}
	delay, ok = secondsToDuration(value)
	return delay, time.Time{}, ok
}
