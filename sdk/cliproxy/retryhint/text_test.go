package retryhint

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// message wraps text the way upstreams usually deliver it: inside a JSON error.
func message(t *testing.T, text string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"error": map[string]any{"message": text}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestParseMessageText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want time.Duration
	}{
		// English, as seen from OpenAI, Gemini, Claude and relays.
		{
			"openai rate limit", "Rate limit reached for gpt-4 in organization org-x on requests per min (RPM): Limit 3, Used 3, Requested 1. Please try again in 20s. Visit https://platform.openai.com",
			20 * time.Second,
		},
		{"retry in fractional seconds", "Please retry in 1.5s", 1500 * time.Millisecond},
		{"google retry in", "You exceeded your current quota. Please retry in 34.151s.", 34151 * time.Millisecond},
		{"retry after seconds", "Too many requests, retry after 30 seconds", 30 * time.Second},
		{"try again in minutes and seconds", "Rate limited. Try again in 6m0s", 6 * time.Minute},
		{"try again in milliseconds", "try again in 500ms", 500 * time.Millisecond},
		{"reset after a compound duration", "Your quota will reset after 3h19m30s", 3*time.Hour + 19*time.Minute + 30*time.Second},
		{"words for units", "Please try again in 1 minute 30 seconds", 90 * time.Second},
		{"joined with and", "try again in 1 hour and 30 minutes", 90 * time.Minute},
		{"joined with a comma", "retry in 2 minutes, 15 seconds", 2*time.Minute + 15*time.Second},
		{"resets in hours", "Usage limit reached. It resets in 2 hours.", 2 * time.Hour},
		{"resets in days", "Weekly limit hit, resets in 3 days", 72 * time.Hour},
		{"wait for", "Please wait 2 minutes before sending another request", 2 * time.Minute},
		{"upper case", "TRY AGAIN IN 10 SECONDS", 10 * time.Second},
		{"available again", "This model is available again in 45 seconds", 45 * time.Second},

		// Chinese relays.
		{"retry after, with 请在", "请在30秒后重试", 30 * time.Second},
		{"retry after, with spaces", "请求过多，请 5 分钟后再试", 5 * time.Minute},
		{"reset after a compound duration", "额度将在3小时19分30秒后重置", 3*time.Hour + 19*time.Minute + 30*time.Second},
		{"recover after hours", "账号已被限流，2个小时后恢复", 2 * time.Hour},
		{"wait", "请求过于频繁，请等待 45 秒", 45 * time.Second},
		{"wait with 约", "请等待约 2 分钟", 2 * time.Minute},
		{"joined with 又", "请1小时又30分钟后再试", 90 * time.Minute},
		{"fractional seconds", "请1.5秒后重试", 1500 * time.Millisecond},

		// Absolute times need a stated zone.
		{"utc offset", "额度已用尽，将在 2026-10-30 20:16:02 UTC+8 重置", 16*time.Minute + 2*time.Second},
		{"utc offset with minutes", "will reset at 2026-10-30 18:30:00 UTC+05:30", time.Hour},
		{"rfc3339 zulu", "quota resets at 2026-10-30T13:00:00Z", time.Hour},
		{"zulu with fractional seconds", "Resets at 2026-10-30T12:30:00.250Z", 30*time.Minute + 250*time.Millisecond},
		{"numeric offset", "available again at 2026-10-30 21:00:00 +08:00", time.Hour},
		{"numeric offset without colon", "reset 2026-10-30 21:00:00 +0800", time.Hour},
		{"bare UTC", "retry at 2026-10-30 14:00:00 UTC", 2 * time.Hour},
		{"gmt offset", "resets 2026-10-30 08:00:00 GMT-5", time.Hour},
		{"beijing time", "北京时间 2026-10-30 21:00:00 重置", time.Hour},
		{"slashes and a T", "reset after 2026/10/30T13:00:00Z", time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(http.StatusTooManyRequests, nil, message(t, tc.text), testNow)
			if got.Delay != tc.want {
				t.Fatalf("Parse(%q) = %+v, want %v", tc.text, got, tc.want)
			}
			if got.Source != SourceMessageText {
				t.Fatalf("source = %s, want %s", got.Source, SourceMessageText)
			}
		})
	}
}

func TestParseMessageTextAbsoluteOffsets(t *testing.T) {
	// 17:30 at +05:30 is 12:00 UTC, which is "now": not in the future, so no hint.
	if got := Parse(http.StatusTooManyRequests, nil, message(t, "will reset at 2026-10-30 17:30:00 UTC+05:30"), testNow); got.Valid() {
		t.Fatalf("a moment equal to now gave %+v", got)
	}
	// 18:30 at +05:30 is 13:00 UTC.
	got := Parse(http.StatusTooManyRequests, nil, message(t, "will reset at 2026-10-30 18:30:00 UTC+05:30"), testNow)
	if got.Delay != time.Hour || !got.ResetAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("Parse() = %+v, want one hour and the matching ResetAt", got)
	}
	// 07:00 at UTC-5 is 12:00 UTC (now); 08:00 is an hour from now.
	got = Parse(http.StatusTooManyRequests, nil, message(t, "resets 2026-10-30 08:00:00 GMT-5"), testNow)
	if got.Delay != time.Hour {
		t.Fatalf("Parse() = %+v, want one hour", got)
	}
	// The fraction of a second counts: retrying early would just be refused again.
	got = Parse(http.StatusTooManyRequests, nil, message(t, "Resets at 2026-10-30T12:30:00.250Z"), testNow)
	if got.Delay != 30*time.Minute+250*time.Millisecond {
		t.Fatalf("Parse() = %+v, want 30m0.25s", got)
	}
}

// Wording that does not tie a time to retrying is not a hint, and a time with no
// stated zone is not trusted: guessing wrong parks an account for hours.
func TestParseMessageTextWithoutAUsableHint(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"no time at all", "Too many requests"},
		{"a duration that is not about retrying", "The request took 30 seconds to complete"},
		{"months are not minutes", "Please try again in 12 months"},
		{"unknown unit", "try again in 5 fortnights"},
		{"unit that only looks like one", "try again in 5 mississippi"},
		{"after without a retry word", "Your trial ends after 5 minutes of inactivity"},
		{"zero", "Please try again in 0s"},
		{"beyond the cap", "Please try again in 99999 days"},
		{"a pair larger than anything believable", "try again in 1e9 hours"},
		{"timestamp without a zone", "额度将在 2026-10-30 20:16:02 重置"},
		{"timestamp in the past", "quota reset at 2026-10-30T11:00:00Z"},
		{"timestamp without a reset word", "Request made at 2026-10-30T13:00:00Z"},
		{"impossible date", "will reset at 2026-02-31T10:00:00Z"},
		{"impossible time", "will reset at 2026-10-30T25:00:00Z"},
		{"impossible offset", "will reset at 2026-10-30 13:00:00 UTC+99"},
		{"number without unit", "try again in 30"},
		{"chinese without a verb", "30秒"},
		{"chinese verb without a number", "请稍后重试"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(http.StatusTooManyRequests, nil, message(t, tc.text), testNow); got.Valid() {
				t.Fatalf("Parse(%q) = %+v, want no hint", tc.text, got)
			}
		})
	}
}

// Plain-text and HTML bodies are scanned too; relays often answer 429 without
// JSON.
func TestParseNonJSONBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"plain text", "Too many requests. Please retry in 12s.", 12 * time.Second},
		{"chinese plain text", "请求过多，请10秒后再试", 10 * time.Second},
		{"html", "<html><body><h1>429</h1><p>Rate limited, try again in 7 seconds</p></body></html>", 7 * time.Second},
		{"json string at the top level", `"please retry in 3s"`, 3 * time.Second},
		{"no hint in html", "<html><body><h1>429 Too Many Requests</h1></body></html>", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(http.StatusTooManyRequests, nil, []byte(tc.body), testNow)
			if got.Delay != tc.want {
				t.Fatalf("Parse() = %+v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseDurationRun(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"3h19m30s", 3*time.Hour + 19*time.Minute + 30*time.Second, true},
		{"1.5s", 1500 * time.Millisecond, true},
		{"20 seconds. Visit", 20 * time.Second, true},
		{"2 hours 30 minutes left", 150 * time.Minute, true},
		{"30秒后重试", 30 * time.Second, true},
		{"1小时30分钟", 90 * time.Minute, true},
		{"500ms", 500 * time.Millisecond, true},
		{"1d", 24 * time.Hour, true},
		{"12 months", 0, false},
		{"5", 0, false},
		{"s", 0, false},
		{"0s", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseDurationRun(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseDurationRun(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
