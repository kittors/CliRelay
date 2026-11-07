package retryhint

import (
	"strings"
	"testing"
)

func TestClassifyMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    Kind
	}{
		// English throttling the scheduler has always recognised.
		{"google generic exhaustion", "Resource has been exhausted (e.g. check quota).", KindRateLimit},
		{"google status", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, KindRateLimit},
		{"rate limit", "rate limit exceeded", KindRateLimit},
		{"too many requests", "Too Many Requests", KindRateLimit},
		{"openai tpm", "Rate limit reached for gpt-4 on tokens per min (TPM): Limit 10000", KindRateLimit},
		{"gemini free tier body", `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details.","status":"RESOURCE_EXHAUSTED"}}`, KindRateLimit},

		// English exhausted allowances.
		{"quota exhausted reason", `{"error":{"details":[{"reason":"QUOTA_EXHAUSTED"}]}}`, KindQuota},
		{"quota reset delay", `{"error":{"details":[{"metadata":{"quotaResetDelay":"2h1m1s"}}]}}`, KindQuota},
		{"quota limit", "Quota limit 'Tokens per minute' exceeded", KindQuota},
		{"daily quota", "daily quota reached", KindQuota},
		{"usage balance", "usage balance exhausted", KindQuota},
		{"insufficient quota", `{"error":{"type":"insufficient_quota","message":"You exceeded your current quota"}}`, KindQuota},
		{"insufficient balance", "Insufficient balance, please top up", KindQuota},

		// The word "quota" inside Google's generic hint does not make it a quota.
		{"generic hint mentioning quota", `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`, KindRateLimit},

		// Throttling in wordings only layer 2 knows.
		{"rate_limit_error type", `{"type":"error","error":{"type":"rate_limit_error"}}`, KindRateLimit},
		{"requests per minute", "You can make 3 requests per minute", KindRateLimit},
		{"throttled", "Request was throttled", KindRateLimit},
		{"concurrency", "Concurrency limit reached", KindRateLimit},
		{"rpm", "Exceeded RPM for this key", KindRateLimit},
		{"tpm in parentheses", "(TPM) exceeded", KindRateLimit},

		// Chinese relays.
		{"cn too many requests", "请求过多，请稍后再试", KindRateLimit},
		{"cn too frequent", "请求过于频繁", KindRateLimit},
		{"cn load saturated", "当前分组负载已饱和，请稍后再试", KindRateLimit},
		{"cn rate limit", "触发速率限制", KindRateLimit},
		{"cn throttle word", "账号被限流", KindRateLimit},
		{"cn per minute", "每分钟请求数超过上限", KindRateLimit},
		{"cn concurrency", "并发数已达上限", KindRateLimit},
		{"cn quota", "额度已用尽，请充值", KindQuota},
		{"cn balance", "余额不足", KindQuota},
		{"cn quota allocation", "配额已耗尽", KindQuota},
		{"cn per minute allowance", "每分钟额度已用完", KindQuota},
		{"cn throttle and allowance", "请求过多：令牌额度不足", KindQuota},
		{"cn escaped unicode", `{"error":{"message":"\u8bf7\u6c42\u8fc7\u591a"}}`, KindRateLimit},
		{"cn escaped unicode quota", `{"error":{"message":"\u989d\u5ea6\u5df2\u7528\u5c3d"}}`, KindQuota},
		{"cn inside a json body", `{"error":{"message":"请求过多","type":"new_api_error"}}`, KindRateLimit},

		// Neither.
		{"empty", "", KindUnknown},
		{"blank", "  \n", KindUnknown},
		{"unrelated", "upstream exploded", KindUnknown},
		{"rpm inside another word", "serpmo and xtpmx", KindUnknown},
		{"unrelated chinese", "上游服务异常", KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyMessage(tc.message); got != tc.want {
				t.Fatalf("ClassifyMessage(%q) = %d, want %d", tc.message, got, tc.want)
			}
		})
	}
}

// legacyIsTransient is the classifier the scheduler used before this package
// existed, kept verbatim as the oracle for the parity tests below.
func legacyIsTransient(message string) bool {
	body := strings.ToLower(message)
	if body == "" {
		return false
	}
	for _, explicit := range []string{
		"quota_exhausted", "quotaresetdelay", "quota reset",
		"quota limit", "daily quota", "usage balance exhausted", "balance exhausted",
	} {
		if strings.Contains(body, explicit) {
			return false
		}
	}
	return strings.Contains(body, "resource has been exhausted") ||
		strings.Contains(body, "resource_exhausted") ||
		strings.Contains(body, "rate limit") ||
		strings.Contains(body, "too many requests")
}

func legacyNamesQuota(message string) bool {
	body := strings.ToLower(message)
	for _, explicit := range []string{
		"quota_exhausted", "quotaresetdelay", "quota reset",
		"quota limit", "daily quota", "usage balance exhausted", "balance exhausted",
	} {
		if strings.Contains(body, explicit) {
			return true
		}
	}
	return false
}

// Every message the old classifier read as throttling is still throttling, and
// every message it read as a named quota is still a quota, whatever else the
// message says. A message the old classifier did not recognise may become
// throttling only if it names no allowance.
func TestClassifyMessageKeepsEveryLegacyOutcome(t *testing.T) {
	// Fragments cover both lists of the old classifier, the wordings added since
	// and neutral filler. Every ordered combination of up to three is checked.
	type fragment struct {
		text string
		// namesAllowance marks fragments that name an exhausted allowance and so
		// must keep a message from turning into throttling.
		namesAllowance bool
	}
	fragments := []fragment{
		{"Resource has been exhausted", false},
		{"RESOURCE_EXHAUSTED", false},
		{"Rate limit reached", false},
		{"Too Many Requests", false},
		{"quota_exhausted", true},
		{"quotaResetDelay 2h", true},
		{"Quota reset", true},
		{"Quota limit exceeded", true},
		{"daily quota", true},
		{"usage balance exhausted", true},
		{"balance exhausted", true},
		{"You exceeded your current quota", true},
		{"insufficient_quota", true},
		{"insufficient balance", true},
		{"额度已用尽", true},
		{"余额不足", true},
		{"配额", true},
		{"请求过多", false},
		{"请求过于频繁", false},
		{"限流", false},
		{"每分钟", false},
		{"requests per minute", false},
		{"throttled", false},
		{"RPM", false},
		{"please try again", false},
		{"upstream error", false},
	}
	checked := 0
	var walk func(prefix []string, depth int)
	walk = func(prefix []string, depth int) {
		if len(prefix) > 0 {
			message := strings.Join(prefix, " | ")
			got := ClassifyMessage(message)
			checked++
			if legacyIsTransient(message) && got != KindRateLimit {
				t.Fatalf("legacy read %q as throttling, now %d", message, got)
			}
			if legacyNamesQuota(message) && got != KindQuota {
				t.Fatalf("legacy read %q as a named quota, now %d", message, got)
			}
			if got == KindRateLimit && !legacyIsTransient(message) {
				// A new throttling reading is allowed only when nothing in the
				// message names an allowance.
				for _, f := range fragments {
					if f.namesAllowance && strings.Contains(message, f.text) {
						t.Fatalf("%q was widened into throttling although it names an allowance (%q)", message, f.text)
					}
				}
			}
		}
		if depth == 3 {
			return
		}
		for _, f := range fragments {
			walk(append(append([]string{}, prefix...), f.text), depth+1)
		}
	}
	walk(nil, 0)
	if checked < 10000 {
		t.Fatalf("only %d combinations were checked", checked)
	}
}

func TestClassifyMessageCaseInsensitive(t *testing.T) {
	for _, message := range []string{"RATE LIMIT", "Rate Limit", "rate limit", "TOO MANY REQUESTS", "ThRoTtLeD"} {
		if got := ClassifyMessage(message); got != KindRateLimit {
			t.Fatalf("ClassifyMessage(%q) = %d, want throttling", message, got)
		}
	}
}

func TestContainsWord(t *testing.T) {
	cases := []struct {
		text, word string
		want       bool
	}{
		{"exceeded rpm for key", "rpm", true},
		{"(rpm)", "rpm", true},
		{"rpm", "rpm", true},
		{"rpm_limit", "rpm", false},
		{"xrpm", "rpm", false},
		{"rpms", "rpm", false},
		{"serpmo rpm", "rpm", true},
		{"serpmo", "rpm", false},
		{"请求rpm超限", "rpm", true},
		{"", "rpm", false},
	}
	for _, tc := range cases {
		if got := containsWord(tc.text, tc.word); got != tc.want {
			t.Fatalf("containsWord(%q, %q) = %v, want %v", tc.text, tc.word, got, tc.want)
		}
	}
}
