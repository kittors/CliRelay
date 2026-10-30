package retryhint

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Kind says what a 429 message is about.
type Kind int

const (
	// KindUnknown is a message that names neither: callers keep their default,
	// which is to treat it as an exhausted allowance.
	KindUnknown Kind = iota
	// KindRateLimit is short-term throttling: requests or tokens per minute,
	// too many concurrent requests, an overloaded relay. It clears by itself and
	// says nothing about the allowance left on the account.
	KindRateLimit
	// KindQuota is an exhausted allowance: quota, balance, credits. It lasts
	// until a reset time or a top-up.
	KindQuota
)

// ClassifyMessage tells throttling from an exhausted allowance by what the
// error message says.
//
// The answer comes in two layers, and the order is what keeps behaviour stable.
//
//  1. The English phrases the scheduler has always used (legacyQuotaPhrases,
//     legacyRateLimitPhrases). They are matched against the raw lower-cased
//     message exactly as before, so every message that used to be read as
//     throttling or as an exhausted quota still is, whatever else it says. The
//     Gemini free-tier body is the example that matters: it says "You exceeded
//     your current quota" and also carries status RESOURCE_EXHAUSTED, was always
//     read as throttling, and has to stay that way.
//  2. Wordings added later (Chinese relays, more English throttling terms),
//     consulted only for a message layer 1 knew nothing about. Those used to fall
//     through to "exhausted allowance"; a match here can only move them to
//     throttling, never the other way, and a message that also names an allowance
//     stays an exhausted allowance ("每分钟额度已用完" is a spent allowance even
//     though it says "per minute").
//
// Both layers are lists of phrases providers and relays are known to use, not
// patterns. A wording on neither list is KindUnknown, and callers treat it as
// they always did.
func ClassifyMessage(message string) Kind {
	lower := strings.ToLower(message)
	if strings.TrimSpace(lower) == "" {
		return KindUnknown
	}
	if containsAny(lower, legacyQuotaPhrases) {
		return KindQuota
	}
	if containsAny(lower, legacyRateLimitPhrases) {
		return KindRateLimit
	}

	text := decodedMessage(message, lower)
	if containsAny(text, quotaPhrases) {
		return KindQuota
	}
	if containsAny(text, rateLimitPhrases) || containsWord(text, "rpm") || containsWord(text, "tpm") {
		return KindRateLimit
	}
	return KindUnknown
}

// legacyQuotaPhrases name an exhausted allowance explicitly: a quota that ran
// out (quota_exhausted, a quotaResetDelay, a daily quota) as opposed to the
// generic throttle Google answers with "resource has been exhausted".
var legacyQuotaPhrases = []string{
	"quota_exhausted", "quotaresetdelay", "quota reset",
	"quota limit", "daily quota", "usage balance exhausted", "balance exhausted",
}

// legacyRateLimitPhrases name throttling in the words Google, OpenAI and most
// gateways use.
var legacyRateLimitPhrases = []string{
	"resource has been exhausted", "resource_exhausted", "rate limit", "too many requests",
}

// quotaPhrases name an exhausted allowance in wordings layer 1 does not cover.
// They keep a message that also contains a throttling phrase below from being
// read as throttling.
var quotaPhrases = []string{
	// OpenAI answers an unpaid account with 429 insufficient_quota, which is about
	// billing and not about request rate.
	"insufficient_quota", "exceeded your current quota",
	"insufficient balance", "insufficient funds", "out of credits",
	// Chinese relays (New API and its forks) word an empty account in terms of
	// 额度 (allowance), 余额 (balance) and 配额 (quota).
	"额度", "余额", "配额", "欠费", "充值", "已用尽", "已用完",
	"点数不足", "积分不足", "次数不足",
}

// rateLimitPhrases name throttling in wordings layer 1 does not cover.
var rateLimitPhrases = []string{
	"rate_limit", "requests per minute", "tokens per minute", "requests per second",
	"per minute", "throttl", "concurrency limit", "too many concurrent",
	// Chinese relays: what New API-style gateways say when an upstream channel is
	// saturated or a client calls too fast.
	"请求过多", "请求过于频繁", "请求太频繁", "请求频繁", "访问过于频繁", "访问频繁",
	"频率过高", "频率限制", "请求频率", "速率限制", "限流",
	"负载已饱和", "负载饱和", "负载过高",
	"并发数", "并发限制", "并发过高",
	"每分钟", "每秒",
}

func containsAny(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// decodedMessage is the text layer 2 searches: the lower-cased message and, when
// it is a JSON error, its decoded message strings. Some gateways escape
// non-ASCII text as \uXXXX, which a plain substring search would not see.
func decodedMessage(message, lower string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return lower
	}
	if len(trimmed) > maxBodyBytes {
		return lower
	}
	if decoded := messageText(gjson.Parse(trimmed)); decoded != "" {
		return lower + "\n" + strings.ToLower(decoded)
	}
	return lower
}

// containsWord reports whether word occurs in text not embedded in a longer
// run of letters or digits, so "rpm" matches "(RPM)" but not "serpmo".
func containsWord(text, word string) bool {
	for start := 0; ; {
		index := strings.Index(text[start:], word)
		if index < 0 {
			return false
		}
		begin := start + index
		end := begin + len(word)
		if (begin == 0 || !isWordByte(text[begin-1])) && (end == len(text) || !isWordByte(text[end])) {
			return true
		}
		start = begin + 1
	}
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}
