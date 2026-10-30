package retryhint

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file reads a wait out of a sentence. Gateways and relays put the time in
// the message instead of a header: "Please try again in 20s", "reset after
// 3h19m30s", "请在30秒后重试", "将在 2026-10-11 20:16:02 UTC+8 重置".
//
// Only phrases that tie a time to retrying or resetting are read. A bare
// "30 seconds" in a sentence proves nothing, so an unknown wording yields no
// hint instead of a guess.

// Anchors in English. Each ends right before the duration it introduces.
var englishAnchors = regexp.MustCompile(`(?i)` +
	`(?:try(?:ing)?\s+again|retry(?:ing)?|resets?|available\s+again|come\s+back|again)\s+(?:in|after)\s+` +
	`|wait(?:ing)?\s+(?:for\s+|another\s+)?`)

// Chinese puts the duration before the verb: "30秒后重试". The group captures
// the run of numbers and units in front of 后.
var chineseDurationBeforeVerb = regexp.MustCompile(
	`((?:\d+(?:\.\d+)?\s*(?:毫秒|秒钟|秒|分钟|分|个?小时|天|日)\s*(?:[,，、]\s*|又\s*|和\s*|零\s*)?)+)后\s*(?:再次?|可以|才能|将|会)?\s*(?:重试|再试|重置|恢复|解除|解封|可用|刷新|尝试)`)

// Chinese anchors that precede the duration: "请等待 30 秒".
var chineseAnchors = regexp.MustCompile(`(?:请)?等待\s*(?:约)?\s*`)

// hintFromText reads the first usable wait out of a message.
func hintFromText(text string, now time.Time) (Hint, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Hint{}, false
	}
	if len(text) > maxBodyBytes {
		text = text[:maxBodyBytes]
	}
	if hint, ok := relativeHintFromText(text); ok {
		return hint, true
	}
	if hint, ok := absoluteHintFromText(text, now); ok {
		return hint, true
	}
	return Hint{}, false
}

func relativeHintFromText(text string) (Hint, bool) {
	for _, loc := range englishAnchors.FindAllStringIndex(text, -1) {
		if d, ok := parseDurationRun(text[loc[1]:]); ok && withinBounds(d) {
			return Hint{Delay: d, Source: SourceMessageText}, true
		}
	}
	for _, match := range chineseDurationBeforeVerb.FindAllStringSubmatch(text, -1) {
		if d, ok := parseDurationRun(match[1]); ok && withinBounds(d) {
			return Hint{Delay: d, Source: SourceMessageText}, true
		}
	}
	for _, loc := range chineseAnchors.FindAllStringIndex(text, -1) {
		if d, ok := parseDurationRun(text[loc[1]:]); ok && withinBounds(d) {
			return Hint{Delay: d, Source: SourceMessageText}, true
		}
	}
	return Hint{}, false
}

// Units, matched exactly against the run of letters (or CJK characters) that
// follows a number. Exact matching is what keeps "12 months" from reading as
// 12 minutes.
var (
	englishUnits = map[string]time.Duration{
		"ms": time.Millisecond, "msec": time.Millisecond, "msecs": time.Millisecond,
		"millisecond": time.Millisecond, "milliseconds": time.Millisecond,
		"s": time.Second, "sec": time.Second, "secs": time.Second,
		"second": time.Second, "seconds": time.Second,
		"m": time.Minute, "min": time.Minute, "mins": time.Minute,
		"minute": time.Minute, "minutes": time.Minute,
		"h": time.Hour, "hr": time.Hour, "hrs": time.Hour,
		"hour": time.Hour, "hours": time.Hour,
		"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	}
	// Longest first: the prefix match must see 秒钟 before 秒 and 分钟 before 分.
	chineseUnits = []struct {
		unit  string
		value time.Duration
	}{
		{"毫秒", time.Millisecond},
		{"秒钟", time.Second},
		{"分钟", time.Minute},
		{"个小时", time.Hour},
		{"小时", time.Hour},
		{"秒", time.Second},
		{"分", time.Minute},
		{"天", 24 * time.Hour},
		{"日", 24 * time.Hour},
	}
)

var numberPrefix = regexp.MustCompile(`^\d+(?:\.\d+)?`)

// parseDurationRun reads a run of number-unit pairs from the start of s
// ("3h19m30s", "1 minute 30 seconds", "3小时19分30秒") and returns their sum. It
// stops at the first thing that is not another pair; trailing text is ignored.
func parseDurationRun(s string) (time.Duration, bool) {
	var (
		total time.Duration
		pairs int
	)
	for {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		number := numberPrefix.FindString(s)
		if number == "" {
			break
		}
		value, err := strconv.ParseFloat(number, 64)
		if err != nil {
			break
		}
		rest := strings.TrimLeftFunc(s[len(number):], unicode.IsSpace)
		unit, size := matchUnit(rest)
		if size == 0 {
			break
		}
		part := value * float64(unit)
		// A single pair beyond the largest believable wait is a mistake, and an
		// unchecked conversion of it could wrap.
		if part > float64(MaxDelay) {
			return 0, false
		}
		total += time.Duration(part)
		pairs++
		s = skipJoiner(rest[size:])
	}
	return total, pairs > 0 && total > 0
}

// matchUnit reads the unit at the start of s.
func matchUnit(s string) (time.Duration, int) {
	if s == "" {
		return 0, 0
	}
	if r, _ := utf8.DecodeRuneInString(s); r < utf8.RuneSelf {
		end := 0
		for end < len(s) && isASCIILetter(s[end]) {
			end++
		}
		if end == 0 {
			return 0, 0
		}
		unit, ok := englishUnits[strings.ToLower(s[:end])]
		if !ok {
			return 0, 0
		}
		return unit, end
	}
	for _, candidate := range chineseUnits {
		if strings.HasPrefix(s, candidate.unit) {
			return candidate.value, len(candidate.unit)
		}
	}
	return 0, 0
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// skipJoiner drops the words and punctuation that join two number-unit pairs
// ("1 hour and 30 minutes", "1小时又30分钟", "1m, 30s"). If what follows is not
// a number the caller's next iteration simply finds nothing and stops.
func skipJoiner(s string) string {
	trimmed := strings.TrimLeftFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == '，' || r == '、'
	})
	lower := strings.ToLower(trimmed)
	for _, word := range []string{"and ", "和", "又", "零"} {
		if strings.HasPrefix(lower, word) {
			return trimmed[len(word):]
		}
	}
	return trimmed
}

// An absolute time with its zone: "2026-10-11 20:16:02 UTC+8", "2026-10-11T20:16:02Z",
// "2026-10-11 20:16:02 +08:00". Groups: year, month, day, hour, minute, second,
// fraction of a second, zone.
var absoluteTime = regexp.MustCompile(`(?i)` +
	`(\d{4})[-/](\d{1,2})[-/](\d{1,2})[ T]+(\d{1,2}):(\d{2})(?::(\d{2}))?(?:\.(\d{1,9}))?` +
	`\s*[(（]?\s*` +
	`(z\b|(?:utc|gmt)(?:\s*[+-]\s*\d{1,2}(?::?\d{2})?)?|[+-]\d{2}:?\d{2}|北京时间)?`)

// resetWords say that a timestamp is when something lifts or may be retried.
var resetWords = []string{
	"reset", "retry", "available", "unlock", "recover",
	"重置", "恢复", "解除", "解封", "重试", "再试", "可用", "刷新",
}

func absoluteHintFromText(text string, now time.Time) (Hint, bool) {
	lower := strings.ToLower(text)
	mentionsReset := false
	for _, word := range resetWords {
		if strings.Contains(lower, word) {
			mentionsReset = true
			break
		}
	}
	if !mentionsReset {
		return Hint{}, false
	}
	for _, m := range absoluteTime.FindAllStringSubmatch(text, -1) {
		zone := m[8]
		if zone == "" && strings.Contains(text, "北京时间") {
			zone = "北京时间"
		}
		// A timestamp with no stated zone could be off by hours either way; a
		// wrong guess would park an account for the error, so it is not used.
		offset, ok := zoneOffset(zone)
		if !ok {
			continue
		}
		at, ok := buildInstant(m[1:8], offset)
		if !ok {
			continue
		}
		if delay, ok := untilInstant(at, now); ok && withinBounds(delay) {
			return Hint{Delay: delay, ResetAt: at, Source: SourceMessageText}, true
		}
	}
	return Hint{}, false
}

// buildInstant assembles year, month, day, hour, minute, second and the fraction
// of a second (digits after the point, possibly empty) into an instant.
func buildInstant(parts []string, offset *time.Location) (time.Time, bool) {
	numbers := make([]int, 6)
	for i := 0; i < 6; i++ {
		if parts[i] == "" {
			continue
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return time.Time{}, false
		}
		numbers[i] = n
	}
	year, month, day, hour, minute, second := numbers[0], numbers[1], numbers[2], numbers[3], numbers[4], numbers[5]
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, false
	}
	// "…:00.250Z" is 250 ms past the second; dropping it would retry early.
	nanos := 0
	if fraction := parts[6]; fraction != "" {
		n, err := strconv.Atoi(fraction + strings.Repeat("0", 9-len(fraction)))
		if err != nil {
			return time.Time{}, false
		}
		nanos = n
	}
	at := time.Date(year, time.Month(month), day, hour, minute, second, nanos, offset)
	// time.Date normalizes 31 February to March; an impossible date is a typo.
	if at.Day() != day {
		return time.Time{}, false
	}
	return at, true
}

var zoneOffsetPattern = regexp.MustCompile(`(?i)^(?:z|(?:utc|gmt)(?:\s*([+-])\s*(\d{1,2})(?::?(\d{2}))?)?|([+-])(\d{2}):?(\d{2}))$`)

// zoneOffset turns the zone text of a timestamp into a location. It returns
// false when no zone was given.
func zoneOffset(zone string) (*time.Location, bool) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return nil, false
	}
	if zone == "北京时间" {
		return time.FixedZone("UTC+8", 8*3600), true
	}
	m := zoneOffsetPattern.FindStringSubmatch(zone)
	if m == nil {
		return nil, false
	}
	sign, hours, minutes := m[1], m[2], m[3]
	if sign == "" {
		sign, hours, minutes = m[4], m[5], m[6]
	}
	if sign == "" {
		return time.UTC, true
	}
	h, _ := strconv.Atoi(hours)
	mm, _ := strconv.Atoi(minutes)
	if h > 14 || mm > 59 {
		return nil, false
	}
	seconds := h*3600 + mm*60
	if sign == "-" {
		seconds = -seconds
	}
	return time.FixedZone(zone, seconds), true
}
