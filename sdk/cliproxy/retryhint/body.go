package retryhint

import (
	"bytes"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	// maxBodyBytes skips bodies too large to be an API error. A multi-megabyte
	// HTML error page has no hint worth scanning for.
	maxBodyBytes = 64 << 10
	// maxScanDepth and maxScanNodes bound the walk through a JSON error so a
	// hostile or merely huge body cannot make it expensive.
	maxScanDepth = 4
	maxScanNodes = 512
	maxScanArray = 16
	// maxStrings caps how many string values are gathered as message text.
	maxStrings = 64
)

// Field names, lower-cased, that carry a wait in a JSON error. The meaning is
// fixed by the name, so a number is never guessed to be seconds or an epoch:
//
//   - relative seconds: Codex/xAI `resets_in_seconds`, generic `retry_after`.
//   - relative milliseconds: `retry_after_ms`.
//   - duration text: Google's RetryInfo `retryDelay` and ErrorInfo
//     `quotaResetDelay` ("34s", "0.5s").
//   - absolute time: Codex/xAI `resets_at` (epoch seconds), others in RFC 3339.
var (
	secondsFields = fieldSet(
		"retry_after", "retryafter", "retry_after_seconds", "retry_in_seconds",
		"resets_in_seconds", "reset_in_seconds", "reset_after_seconds", "resets_after_seconds",
	)
	millisFields = fieldSet(
		"retry_after_ms", "retryafterms", "retry_after_millis", "retry_after_milliseconds",
	)
	durationFields = fieldSet(
		"retrydelay", "retry_delay", "quotaresetdelay", "quota_reset_delay", "reset_delay",
	)
	instantFields = fieldSet(
		"resets_at", "reset_at", "resetsat", "resetat", "reset_time", "resettime",
		"reset_timestamp", "next_reset_at", "nextresetat", "quotaresettime", "quota_reset_time",
	)
	// messageFields hold the human readable text of an error.
	messageFields = fieldSet("message", "msg", "detail", "error", "error_description", "description")
)

func fieldSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

// hintFromBody reads a hint out of an error body: a JSON field if the body has
// one, otherwise a sentence in the message text.
func hintFromBody(body []byte, now time.Time) (Hint, bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || len(body) > maxBodyBytes {
		return Hint{}, false
	}
	if gjson.ValidBytes(body) {
		root := gjson.ParseBytes(body)
		scan := scanner{now: now}
		if hint, ok := scan.walk(root, 0); ok {
			return hint, true
		}
		return hintFromText(messageText(root), now)
	}
	return hintFromText(string(body), now)
}

type scanner struct {
	now   time.Time
	nodes int
}

// walk searches a JSON value for a field naming a wait, in document order.
func (s *scanner) walk(value gjson.Result, depth int) (Hint, bool) {
	if depth > maxScanDepth || s.nodes > maxScanNodes {
		return Hint{}, false
	}
	var (
		found  Hint
		isHint bool
	)
	switch {
	case value.IsObject():
		value.ForEach(func(key, child gjson.Result) bool {
			s.nodes++
			if hint, ok := s.fieldHint(strings.ToLower(key.String()), child); ok {
				found, isHint = hint, true
				return false
			}
			if child.IsObject() || child.IsArray() {
				if hint, ok := s.walk(child, depth+1); ok {
					found, isHint = hint, true
					return false
				}
			}
			return s.nodes <= maxScanNodes
		})
	case value.IsArray():
		seen := 0
		value.ForEach(func(_, child gjson.Result) bool {
			s.nodes++
			seen++
			if child.IsObject() || child.IsArray() {
				if hint, ok := s.walk(child, depth+1); ok {
					found, isHint = hint, true
					return false
				}
			}
			return seen < maxScanArray && s.nodes <= maxScanNodes
		})
	}
	return found, isHint
}

// fieldHint interprets one named field. A field that is present but unusable
// (in the past, out of range, not a number) is skipped rather than ending the
// search, so a stale `reset_at` does not hide a later `retryDelay`.
func (s *scanner) fieldHint(name string, value gjson.Result) (Hint, bool) {
	if value.Type != gjson.Number && value.Type != gjson.String {
		return Hint{}, false
	}
	if _, ok := secondsFields[name]; ok {
		if d, ok := relativeSeconds(value, s.now); ok && withinBounds(d) {
			return Hint{Delay: d, Source: SourceBodyField}, true
		}
		return Hint{}, false
	}
	if _, ok := millisFields[name]; ok {
		if millis, ok := numericValue(value); ok {
			if d, ok := millisToDuration(millis); ok && withinBounds(d) {
				return Hint{Delay: d, Source: SourceBodyField}, true
			}
		}
		return Hint{}, false
	}
	if _, ok := durationFields[name]; ok {
		if d, ok := durationValue(value); ok && withinBounds(d) {
			return Hint{Delay: d, Source: SourceBodyField}, true
		}
		return Hint{}, false
	}
	if _, ok := instantFields[name]; ok {
		if delay, at, ok := instantValue(value, s.now); ok && withinBounds(delay) {
			return Hint{Delay: delay, ResetAt: at, Source: SourceBodyField}, true
		}
	}
	return Hint{}, false
}

// numericValue reads a JSON number, or a string holding one.
func numericValue(value gjson.Result) (float64, bool) {
	if value.Type == gjson.Number {
		return value.Float(), true
	}
	return parseNumber(value.String())
}

// relativeSeconds reads a field that holds a number of seconds. A string may
// also use the other Retry-After forms (an HTTP date, "30s").
func relativeSeconds(value gjson.Result, now time.Time) (time.Duration, bool) {
	if value.Type == gjson.Number {
		return secondsToDuration(value.Float())
	}
	return parseRetryAfterValue(value.String(), now)
}

// durationValue reads Google-style duration text ("34s", "1.5s") or a bare
// number of seconds.
func durationValue(value gjson.Result) (time.Duration, bool) {
	if value.Type == gjson.Number {
		return secondsToDuration(value.Float())
	}
	raw := strings.TrimSpace(value.String())
	if d, err := time.ParseDuration(raw); err == nil {
		return d, d > 0
	}
	if seconds, ok := parseNumber(raw); ok {
		return secondsToDuration(seconds)
	}
	return 0, false
}

// instantValue reads an absolute reset time: an epoch (seconds or milliseconds)
// or an RFC 3339 string.
func instantValue(value gjson.Result, now time.Time) (time.Duration, time.Time, bool) {
	if number, ok := numericValue(value); ok {
		at, isEpoch := epochToTime(number)
		if !isEpoch {
			return 0, time.Time{}, false
		}
		delay, ok := untilInstant(at, now)
		return delay, at, ok
	}
	if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value.String())); err == nil {
		delay, ok := untilInstant(at, now)
		return delay, at, ok
	}
	return 0, time.Time{}, false
}

// messageText gathers the human readable strings of a JSON error: the message
// fields at any depth. JSON is decoded here, so a gateway that escapes
// non-ASCII text as \uXXXX is read as the text it stands for.
func messageText(root gjson.Result) string {
	var (
		parts []string
		nodes int
	)
	var collect func(value gjson.Result, depth int)
	collect = func(value gjson.Result, depth int) {
		if depth > maxScanDepth || nodes > maxScanNodes || len(parts) >= maxStrings {
			return
		}
		switch {
		case value.IsObject():
			value.ForEach(func(key, child gjson.Result) bool {
				nodes++
				if _, ok := messageFields[strings.ToLower(key.String())]; ok && child.Type == gjson.String {
					parts = append(parts, child.String())
				} else if child.IsObject() || child.IsArray() {
					collect(child, depth+1)
				}
				return nodes <= maxScanNodes && len(parts) < maxStrings
			})
		case value.IsArray():
			seen := 0
			value.ForEach(func(_, child gjson.Result) bool {
				nodes++
				seen++
				collect(child, depth+1)
				return seen < maxScanArray && nodes <= maxScanNodes
			})
		case value.Type == gjson.String && depth == 0:
			parts = append(parts, value.String())
		}
	}
	collect(root, 0)
	return strings.Join(parts, "\n")
}
