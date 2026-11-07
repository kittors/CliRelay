package retryhint

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func parseBody(body string) Hint {
	return Parse(http.StatusTooManyRequests, nil, []byte(body), testNow)
}

func TestParseBodyFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"codex resets_at in epoch seconds", fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%s}}`, epochSeconds(3*time.Hour)), 3 * time.Hour},
		{"resets_at in epoch milliseconds", fmt.Sprintf(`{"error":{"resets_at":%s}}`, epochMillis(90*time.Second)), 90 * time.Second},
		{"resets_at at the top level", fmt.Sprintf(`{"resets_at":%s}`, epochSeconds(time.Minute)), time.Minute},
		{"reset_at as an rfc3339 string", fmt.Sprintf(`{"error":{"reset_at":%q}}`, testNow.Add(2*time.Hour).Format(time.RFC3339)), 2 * time.Hour},
		{"epoch given as a numeric string", fmt.Sprintf(`{"error":{"resets_at":%q}}`, epochSeconds(5*time.Minute)), 5 * time.Minute},
		{"resets_in_seconds", `{"error":{"resets_in_seconds":90}}`, 90 * time.Second},
		{"resets_in_seconds fractional", `{"error":{"resets_in_seconds":2.5}}`, 2500 * time.Millisecond},
		{"retry_after number", `{"retry_after":12}`, 12 * time.Second},
		{"retryAfter string", `{"error":{"retryAfter":"30"}}`, 30 * time.Second},
		{"retry_after_ms", `{"error":{"retry_after_ms":2500}}`, 2500 * time.Millisecond},
		{"gemini retryDelay in RetryInfo", `{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"34s"}]}}`, 34 * time.Second},
		{"gemini quotaResetDelay in ErrorInfo", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"quotaResetDelay":"34.5s"}}]}}`, 34500 * time.Millisecond},
		{"retryDelay as a bare number of seconds", `{"retryDelay":7}`, 7 * time.Second},
		{"field names are case insensitive", `{"Error":{"RETRY_AFTER":8}}`, 8 * time.Second},
		{"a stale reset_at does not hide a later retryDelay", fmt.Sprintf(`{"error":{"reset_at":%s,"details":[{"retryDelay":"15s"}]}}`, epochSeconds(-time.Hour)), 15 * time.Second},
		{"first usable field in document order", `{"retry_after":4,"error":{"retryDelay":"9s"}}`, 4 * time.Second},
		{"structured field outranks message text", `{"retry_after":4,"error":{"message":"Please try again in 99s"}}`, 4 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBody(tc.body)
			if got.Delay != tc.want {
				t.Fatalf("Parse() = %+v, want %v", got, tc.want)
			}
			if got.Source != SourceBodyField && !strings.Contains(tc.name, "outranks") {
				t.Fatalf("source = %s, want %s", got.Source, SourceBodyField)
			}
		})
	}
}

func TestParseBodyAbsoluteHintsReportTheResetMoment(t *testing.T) {
	got := parseBody(fmt.Sprintf(`{"error":{"resets_at":%s}}`, epochSeconds(time.Hour)))
	if !got.ResetAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("ResetAt = %v, want %v", got.ResetAt, testNow.Add(time.Hour))
	}
	relative := parseBody(`{"error":{"resets_in_seconds":60}}`)
	if !relative.ResetAt.IsZero() {
		t.Fatalf("a relative hint has no reset moment, got %v", relative.ResetAt)
	}
}

// An unreadable, expired or implausible field yields no hint; the scheduler
// then keeps its own cooldown.
func TestParseBodyWithoutAUsableHint(t *testing.T) {
	deep := `{"a":{"b":{"c":{"d":{"e":{"retry_after":5}}}}}}`
	cases := []struct {
		name string
		body string
	}{
		{"empty", ``},
		{"whitespace", "  \n\t "},
		{"not json and no wording", `<html><body>429 Too Many Requests</body></html>`},
		{"json without a hint", `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`},
		{"null field", `{"retry_after":null}`},
		{"boolean field", `{"retry_after":true}`},
		{"object field", `{"retry_after":{"seconds":3}}`},
		{"negative seconds", `{"error":{"resets_in_seconds":-5}}`},
		{"zero seconds", `{"error":{"resets_in_seconds":0}}`},
		{"text where seconds belong", `{"error":{"resets_in_seconds":"soon"}}`},
		{"seconds beyond the cap", `{"error":{"resets_in_seconds":999999999}}`},
		{"milliseconds beyond the cap", `{"retry_after_ms":9999999999999}`},
		{"resets_at in the past", fmt.Sprintf(`{"error":{"resets_at":%s}}`, epochSeconds(-time.Minute))},
		{"resets_at that is a small number, not an epoch", `{"error":{"resets_at":60}}`},
		{"resets_at far in the future", fmt.Sprintf(`{"error":{"resets_at":%s}}`, epochSeconds(400*24*time.Hour))},
		{"retryDelay that is not a duration", `{"retryDelay":"after lunch"}`},
		{"negative retryDelay", `{"retryDelay":"-3s"}`},
		{"field nested deeper than the scan goes", deep},
		{"truncated json", `{"error":{"retry_after":5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseBody(tc.body); got.Valid() {
				t.Fatalf("Parse() = %+v, want no hint", got)
			}
		})
	}
}

func TestParseBodyIsBounded(t *testing.T) {
	// A body past the cap is not an API error; skipping it also keeps a hostile
	// upstream from making every failure expensive.
	oversized := `{"retry_after":5,"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	if got := parseBody(oversized); got.Valid() {
		t.Fatalf("oversized body gave %+v, want no hint", got)
	}

	var wide strings.Builder
	wide.WriteString(`{"items":[`)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			wide.WriteString(",")
		}
		wide.WriteString(`{"n":1}`)
	}
	wide.WriteString(`],"retry_after":5}`)
	// The scan stops after a bounded number of nodes; whether it reaches the
	// trailing field is not the point, finishing without blowing up is.
	_ = parseBody(wide.String())
}

func TestParseBodyReadsEscapedUnicodeMessages(t *testing.T) {
	// Some gateways encode every non-ASCII character as \uXXXX, so the raw bytes
	// hold no Chinese at all: "请在30秒后重试".
	body := `{"error":{"message":"\u8bf7\u572830\u79d2\u540e\u91cd\u8bd5"}}`
	got := parseBody(body)
	if got.Delay != 30*time.Second || got.Source != SourceMessageText {
		t.Fatalf("Parse() = %+v, want 30s from message text", got)
	}
}
