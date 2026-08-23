package aiaccountstatus

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestParseCodexWhamQuotas(t *testing.T) {
	body := []byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":20,"reset_at":1710000000,"limit_window_seconds":604800},"secondary_window":{"used_percent":50,"reset_at":1710003600}}}`)
	quotas := parseCodexWhamQuotas(body)
	if len(quotas) < 1 {
		t.Fatalf("quotas empty")
	}
	if quotas[0].QuotaKey != "code_week" || quotas[0].Percent == nil || *quotas[0].Percent != 80 {
		t.Fatalf("primary = %+v", quotas[0])
	}
}

func TestParseClaudeUsage(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":10,"resets_at":"2026-07-16T12:00:00Z"},"seven_day":{"utilization":40,"resets_at":"2026-07-20T12:00:00Z"}}`)
	quotas := parseClaudeUsage(body)
	if len(quotas) != 2 {
		t.Fatalf("len=%d", len(quotas))
	}
	if quotas[1].QuotaKey != "seven_day" || quotas[1].Percent == nil || *quotas[1].Percent != 60 {
		t.Fatalf("seven_day=%+v", quotas[1])
	}
}

func TestParseKimiUsage(t *testing.T) {
	body := []byte(`{"usage":{"limit":100,"used":25,"remaining":75,"resetTime":"2026-07-20T00:00:00Z"}}`)
	quotas := parseKimiUsage(body)
	if len(quotas) != 1 || quotas[0].Percent == nil || *quotas[0].Percent != 75 {
		t.Fatalf("kimi=%+v", quotas)
	}
}

func TestParseKiroQuota(t *testing.T) {
	body := []byte(`{"subscriptionInfo":{"subscriptionTitle":"Pro"},"usageBreakdownList":[{"usageLimitWithPrecision":100,"currentUsageWithPrecision":40,"nextDateReset":1710000000}]}`)
	quotas := parseKiroQuota(body)
	if len(quotas) < 2 {
		t.Fatalf("kiro=%+v", quotas)
	}
}

func TestParseXAIBilling(t *testing.T) {
	body := []byte(`{"config":{"creditUsagePercent":30,"currentPeriod":{"end":"2026-07-20T00:00:00Z"}}}`)
	quotas := parseXAIBilling(body, "weekly_limit", "weekly", 604800)
	if len(quotas) != 1 || quotas[0].Percent == nil || *quotas[0].Percent != 70 {
		t.Fatalf("xai=%+v", quotas)
	}
}

func TestParseGeminiCLIQuota(t *testing.T) {
	body := []byte(`{"buckets":[{"modelId":"gemini-2.5-pro","remainingFraction":0.5,"remainingAmount":1000,"resetTime":"2026-07-20T00:00:00Z"}]}`)
	quotas := parseGeminiCLIQuota(body)
	if len(quotas) != 1 || quotas[0].Percent == nil || *quotas[0].Percent != 50 {
		t.Fatalf("gemini=%+v", quotas)
	}
}

func TestParseAntigravityModels(t *testing.T) {
	body := []byte(`{"models":{"gemini-3-flash":{"displayName":"Flash","quotaInfo":{"remainingFraction":0.25}}}}`)
	quotas := parseAntigravityModels(body)
	if len(quotas) != 1 || quotas[0].Percent == nil || *quotas[0].Percent != 25 {
		t.Fatalf("antigravity=%+v", quotas)
	}
}

func TestNormalizeProvider(t *testing.T) {
	if got := normalizeProvider("x-ai"); got != "xai" {
		t.Fatalf("got %q", got)
	}
}

func quotaByKey(items []usage.QuotaWindowDTO, key string) *usage.QuotaWindowDTO {
	for i := range items {
		if items[i].QuotaKey == key {
			return &items[i]
		}
	}
	return nil
}

func TestParseCodexWhamQuotasClassifiesAllWindows(t *testing.T) {
	body := []byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": 20, "limit_window_seconds": 18000},
			"secondary_window": {"used_percent": 40, "limit_window_seconds": 604800}
		},
		"code_review_rate_limit": {
			"primary_window": {"used_percent": 10, "limit_window_seconds": 18000},
			"secondary_window": {"used_percent": 30, "limit_window_seconds": 604800}
		},
		"additional_rate_limits": [{
			"limit_name": "gpt-5.3-codex-spark",
			"rate_limit": {
				"primary_window": {"used_percent": 50, "limit_window_seconds": 18000},
				"secondary_window": {"used_percent": 60, "limit_window_seconds": 604800}
			}
		}]
	}`)
	items := parseCodexWhamQuotas(body)
	checks := map[string]float64{
		"code_5h": 80, "code_week": 60,
		"review_5h": 90, "review_week": 70,
		"additional:codex_bengalfox:5h":   50,
		"additional:codex_bengalfox:week": 40,
	}
	for key, want := range checks {
		item := quotaByKey(items, key)
		if item == nil || item.Percent == nil || *item.Percent != want {
			t.Fatalf("%s = %+v, want remaining %.0f", key, item, want)
		}
	}
}

func TestParseCodexWhamQuotasNonStandardAndBlocked(t *testing.T) {
	body := []byte(`{
		"rate_limit": {
			"allowed": false,
			"primary_window": {"limit_window_seconds": 86400, "reset_after_seconds": 60}
		}
	}`)
	before := time.Now().UTC()
	items := parseCodexWhamQuotas(body)
	item := quotaByKey(items, "code_subscription_86400")
	if item == nil || item.Percent == nil || *item.Percent != 0 {
		t.Fatalf("subscription = %+v", item)
	}
	if item.ResetAt == nil || item.ResetAt.Before(before.Add(55*time.Second)) || item.ResetAt.After(before.Add(65*time.Second)) {
		t.Fatalf("reset = %v", item.ResetAt)
	}
}

func TestParseCodexResetExpirationsNestedSortedUnique(t *testing.T) {
	body := []byte(`{"data":{"items":[{"expiresAt":"2026-07-20T00:00:00Z"},{"expires_at":"2026-07-18T00:00:00Z"},{"expires_at":"2026-07-20T00:00:00Z"}]}}`)
	got := parseCodexResetExpirations(body)
	want := []string{"2026-07-18T00:00:00Z", "2026-07-20T00:00:00Z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestCodexAccountIDFallsBackToIDToken(t *testing.T) {
	claims := []byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-token"}}`)
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"id_token": token}}
	if got := codexAccountID(auth); got != "acct-token" {
		t.Fatalf("got %q", got)
	}
}

func TestParseClaudeUsageIncludesIguanaAndExtraUsage(t *testing.T) {
	body := []byte(`{
		"iguana_necktie":{"utilization":25,"resets_at":"2026-07-20T00:00:00Z"},
		"extra_usage":{"is_enabled":true,"utilization":40,"used_credits":"12","monthly_limit":"50"}
	}`)
	items := parseClaudeUsage(body)
	iguana := quotaByKey(items, "iguana_necktie")
	if iguana == nil || iguana.Percent == nil || *iguana.Percent != 75 || iguana.ResetAt == nil {
		t.Fatalf("iguana=%+v", iguana)
	}
	extra := quotaByKey(items, "extra_usage")
	if extra == nil || extra.Percent == nil || *extra.Percent != 60 || extra.Meta != "12 / 50 credits" {
		t.Fatalf("extra=%+v", extra)
	}
}

func TestParseGeminiCLIQuotaGroupsPreferredBucket(t *testing.T) {
	body := []byte(`{"buckets":[
		{"modelId":"gemini-2.5-pro-preview","remainingFraction":0.2},
		{"modelId":"gemini-2.5-pro","remainingFraction":0.5,"remainingAmount":1000,"tokenType":"input"},
		{"modelId":"gemini-2.0-flash","remainingFraction":0.9}
	]}`)
	items := parseGeminiCLIQuota(body)
	if len(items) != 2 {
		t.Fatalf("items=%+v", items)
	}
	preferred := quotaByKey(items, "model:gemini-2.5-pro:input")
	if preferred == nil || preferred.Percent == nil || *preferred.Percent != 50 || preferred.Meta != "tokenType=input · 1000 tokens" {
		t.Fatalf("preferred=%+v", preferred)
	}
	if quotaByKey(items, "model:gemini-2.0-flash") != nil {
		t.Fatal("ignored Gemini 2.0 bucket should not be returned")
	}
}

func TestParseKimiUsageNestedWindows(t *testing.T) {
	body := []byte(`{"usages":[{"scope":"FEATURE_CODING","detail":{"limit":100,"used":25},"limits":[
		{"window":{"duration":5,"timeUnit":"TIME_UNIT_HOUR"},"detail":{"limit":20,"remaining":10}},
		{"window":{"duration":1,"timeUnit":"TIME_UNIT_WEEK"},"detail":{"limit":100,"remaining":70}}
	]}]}`)
	items := parseKimiUsage(body)
	fiveHour := quotaByKey(items, "code_5h")
	weekly := quotaByKey(items, "code_week")
	if fiveHour == nil || fiveHour.Percent == nil || *fiveHour.Percent != 50 {
		t.Fatalf("fiveHour=%+v", fiveHour)
	}
	if weekly == nil || weekly.Percent == nil || *weekly.Percent != 75 {
		t.Fatalf("weekly=%+v", weekly)
	}
}

func TestParseKiroQuotaIncludesTrial(t *testing.T) {
	body := []byte(`{"subscriptionInfo":{"subscriptionTitle":"Pro"},"usageBreakdownList":[{
		"usageLimitWithPrecision":100,"currentUsageWithPrecision":40,"nextDateReset":1784505600,
		"freeTrialInfo":{"freeTrialStatus":"ACTIVE","usageLimitWithPrecision":20,"currentUsageWithPrecision":5,"freeTrialExpiry":1784592000}
	}]}`)
	items := parseKiroQuota(body)
	trial := quotaByKey(items, "trial_quota")
	if trial == nil || trial.Percent == nil || *trial.Percent != 75 || trial.ResetAt == nil {
		t.Fatalf("trial=%+v", trial)
	}
}

func TestParseXAIBillingFullSummaryAndPlan(t *testing.T) {
	weekly := []byte(`{"config":{"currentPeriod":{"type":"WEEKLY","start":"2026-07-13T00:00:00Z","end":"2026-07-20T00:00:00Z"},"creditUsagePercent":30,"productUsage":[{"product":"grok-code","usagePercent":20}]}}`)
	weeklyItems := parseXAIWeeklyBilling(weekly)
	weeklyLimit := quotaByKey(weeklyItems, "weekly_limit")
	if weeklyLimit == nil || weeklyLimit.Percent == nil || *weeklyLimit.Percent != 70 || weeklyLimit.WindowSeconds != 604800 || weeklyLimit.ResetAt == nil {
		t.Fatalf("weekly=%+v", weeklyLimit)
	}
	if weeklyLimit.Meta != "" {
		t.Fatalf("weekly meta should be empty, got %q", weeklyLimit.Meta)
	}
	product := quotaByKey(weeklyItems, "product:grok-code")
	if product == nil || product.Percent == nil || *product.Percent != 80 {
		t.Fatalf("product=%+v", product)
	}

	monthly := []byte(`{"config":{"monthlyLimit":{"val":15000},"used":{"val":5000},"onDemandCap":{"val":2000},"onDemandUsed":{"val":500},"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`)
	monthlyItems := parseXAIMonthlyBilling(monthly)
	credits := quotaByKey(monthlyItems, "monthly_credits")
	if credits == nil || credits.Percent == nil || *credits.Percent != 67 || credits.Meta != "$100.00 / $150.00" {
		t.Fatalf("credits=%+v", credits)
	}
	payGo := quotaByKey(monthlyItems, "pay_as_you_go")
	if payGo == nil || payGo.Percent == nil || *payGo.Percent != 75 || payGo.Meta != "$15.00 / $20.00" {
		t.Fatalf("payGo=%+v", payGo)
	}
	if got := resolveXAIPlan(monthly); got != "supergrok" {
		t.Fatalf("plan=%q", got)
	}
}

func TestResolveXAIPlanUsesEntitlementWhenMonthlyLimitIsZero(t *testing.T) {
	weekly := []byte(`{"config":{"currentPeriod":{"type":"WEEKLY","end":"2026-08-20T00:00:00Z"},"creditUsagePercent":12,"productUsage":[{"product":"grok-code","usagePercent":8}]}}`)
	zeroMonthly := []byte(`{"config":{"monthlyLimit":{"val":0},"used":{"val":0},"onDemandCap":{"val":0},"billingPeriodEnd":"2026-09-01T00:00:00Z"}}`)
	if got := resolveXAIPlan(zeroMonthly, weekly); got != "supergrok" {
		t.Fatalf("zero monthly + weekly entitlement plan=%q, want supergrok", got)
	}
	if got := resolveXAIPlan([]byte(`{"config":{"monthlyLimit":{"val":150000}}}`), weekly); got != "supergrok-heavy" {
		t.Fatalf("legacy heavy monthly plan=%q", got)
	}
	if got := resolveXAIPlan([]byte(`{"config":{"planType":"SuperGrok Heavy"}}`), weekly); got != "supergrok-heavy" {
		t.Fatalf("explicit heavy plan=%q", got)
	}
	if got := resolveXAIPlan(weekly); got != "" {
		t.Fatalf("weekly-only free grok plan=%q, want empty", got)
	}
	if got := resolveXAIPlan(zeroMonthly); got != "" {
		t.Fatalf("zero monthly without weekly plan=%q, want empty", got)
	}
}

// One row per family, each carrying a single representative model's real quota.
//
// gemini-2.5-pro and gemini-3.1-pro-high are both "Gemini Pro" yet draw on
// different buckets — captured from a live account, where gemini-3.x resets
// weekly and gemini-2.5-* on a 5h cycle. Aggregating them reported the minimum
// of two unrelated quotas with a countdown from the wrong one; picking the
// preferred member reports something that is actually true of a model.
func TestParseAntigravityModelsPicksOneRepresentativePerFamily(t *testing.T) {
	body := []byte(`{"models":{
		"gemini-3.1-pro-high":{"quotaInfo":{"remainingFraction":0.8,"resetTime":"2026-08-27T15:17:11Z"}},
		"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.1,"resetTime":"2026-08-20T20:17:11Z"}},
		"gemini-3-flash-agent":{"quotaInfo":{"remainingFraction":0.6,"resetTime":"2026-08-27T15:17:11Z"}},
		"claude-sonnet-4-6":{"quotaInfo":{"remainingFraction":0.9,"resetTime":"2026-08-27T15:17:11Z"}},
		"chat_20706":{"quotaInfo":{"remainingFraction":1}}
	}}`)
	items := parseAntigravityModels(body)

	pro := quotaByKey(items, "antigravity:gemini_pro")
	if pro == nil || pro.Percent == nil || *pro.Percent != 80 {
		t.Fatalf("pro=%+v, want the preferred gemini-3.1-pro-high at 80", pro)
	}
	if pro.Meta != "gemini-3.1-pro-high" {
		t.Fatalf("pro represented by %q", pro.Meta)
	}
	// The 2.5 model's 10% must not leak into the row: different bucket.
	if pro.ResetAt == nil || pro.ResetAt.Format(time.RFC3339) != "2026-08-27T15:17:11Z" {
		t.Fatalf("pro reset=%v, want the represented model's own reset", pro.ResetAt)
	}

	flash := quotaByKey(items, "antigravity:gemini_flash")
	if flash == nil || flash.Percent == nil || *flash.Percent != 60 {
		t.Fatalf("flash=%+v", flash)
	}
	claude := quotaByKey(items, "antigravity:claude")
	if claude == nil || claude.Percent == nil || *claude.Percent != 90 {
		t.Fatalf("claude=%+v", claude)
	}
	for _, item := range items {
		if strings.Contains(item.QuotaKey, "chat_") {
			t.Fatalf("internal model leaked: %+v", item)
		}
	}
}

// Preference is a ranking, not an allowlist: a family whose preferred ids are
// all absent still shows the member the upstream did send.
func TestParseAntigravityModelsFallsBackToAnyFamilyMember(t *testing.T) {
	body := []byte(`{"models":{
		"gemini-9-pro-experimental":{"quotaInfo":{"remainingFraction":0.45,"resetTime":"2026-08-27T15:17:11Z"}}
	}}`)
	pro := quotaByKey(parseAntigravityModels(body), "antigravity:gemini_pro")
	if pro == nil || pro.Percent == nil || *pro.Percent != 45 {
		t.Fatalf("pro=%+v, want the only member of the family", pro)
	}
}

// Unclassified models are unrelated to each other, so each gets its own row
// under its own name rather than being collapsed into one "other" bar.
func TestParseAntigravityModelsGivesUnclassifiedModelsTheirOwnRows(t *testing.T) {
	body := []byte(`{"models":{
		"gpt-oss-120b-medium":{"displayName":"GPT-OSS 120B (Medium)","quotaInfo":{"remainingFraction":0.5,"resetTime":"2026-08-27T15:17:11Z"}},
		"grok-5-fast":{"displayName":"Grok 5 Fast","quotaInfo":{"remainingFraction":0.25,"resetTime":"2026-08-27T15:17:11Z"}}
	}}`)
	items := parseAntigravityModels(body)
	gpt := quotaByKey(items, "antigravity:model_gpt_oss_120b_medium")
	grok := quotaByKey(items, "antigravity:model_grok_5_fast")
	if gpt == nil || gpt.QuotaLabel != "GPT-OSS 120B (Medium)" || gpt.Percent == nil || *gpt.Percent != 50 {
		t.Fatalf("gpt=%+v", gpt)
	}
	if grok == nil || grok.Percent == nil || *grok.Percent != 25 {
		t.Fatalf("grok=%+v", grok)
	}
}

// retrieveUserQuotaSummary is the authoritative view: the upstream reports one
// bucket per family per window, and both the grouping and the wording are its
// own. Splitting a single bucket into several rows by model id is what made one
// quota render three times with identical numbers.
func TestParseAntigravityQuotaSummaryUsesUpstreamGrouping(t *testing.T) {
	body := []byte(`{"groups":[
		{"displayName":"Gemini Models","description":"Gemini family","buckets":[
			{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.72,"resetTime":"2026-08-19T07:00:00Z"},
			{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.51,"resetTime":"2026-08-24T07:00:00Z"}
		]},
		{"displayName":"Claude and GPT models","buckets":[
			{"bucketId":"3p-5h","window":"5h","remainingFraction":1,"resetTime":"2026-08-19T11:00:00Z"}
		]}
	]}`)
	items := parseAntigravityQuotaSummary(body)
	if len(items) != 3 {
		t.Fatalf("items=%d, want 3: %+v", len(items), items)
	}

	fiveHour := quotaByKey(items, "antigravity:gemini_5h")
	if fiveHour == nil || fiveHour.Percent == nil || *fiveHour.Percent != 72 {
		t.Fatalf("gemini 5h=%+v", fiveHour)
	}
	if fiveHour.WindowSeconds != antigravityWindowFiveHour {
		t.Fatalf("gemini 5h window=%d", fiveHour.WindowSeconds)
	}
	// The row is named after the group only; the window is rendered from
	// WindowSeconds, and the upstream's bucket wording would make the label too
	// long for a card row.
	if fiveHour.QuotaLabel != "Gemini Models" {
		t.Fatalf("gemini 5h label=%q, want the group name alone", fiveHour.QuotaLabel)
	}
	if fiveHour.Meta != "Gemini family" {
		t.Fatalf("gemini 5h meta=%q, want the group description", fiveHour.Meta)
	}

	weekly := quotaByKey(items, "antigravity:gemini_weekly")
	if weekly == nil || weekly.Percent == nil || *weekly.Percent != 51 {
		t.Fatalf("gemini weekly=%+v", weekly)
	}
	if weekly.WindowSeconds != antigravityWindowWeek {
		t.Fatalf("gemini weekly window=%d, want %d", weekly.WindowSeconds, antigravityWindowWeek)
	}

	thirdParty := quotaByKey(items, "antigravity:3p_5h")
	if thirdParty == nil || thirdParty.Percent == nil || *thirdParty.Percent != 100 {
		t.Fatalf("3p 5h=%+v", thirdParty)
	}
}

func TestAntigravityWindowSecondsFallsBackToBucketID(t *testing.T) {
	cases := []struct {
		window, bucketID string
		want             int64
	}{
		{"weekly", "gemini-weekly", antigravityWindowWeek},
		{"5h", "gemini-5h", antigravityWindowFiveHour},
		{"", "3p-weekly", antigravityWindowWeek},
		{"", "3p-5h", antigravityWindowFiveHour},
		{"24h", "", 24 * 60 * 60},
		{"", "", 0},
		{"per-request", "odd-bucket", 0},
		// The `h` inside "flash" has no digits before it; the one that matters
		// is further along.
		{"", "gemini-flash-5h", antigravityWindowFiveHour},
		{"", "high-throughput", 0},
	}
	for _, tc := range cases {
		if got := antigravityWindowSeconds(tc.window, tc.bucketID); got != tc.want {
			t.Fatalf("window(%q,%q)=%d, want %d", tc.window, tc.bucketID, got, tc.want)
		}
	}
}

// The plan ladder mirrors the upstream client: paid wins, current counts only
// while the account is eligible, and an ineligible account reports its default
// allowed tier as restricted.
func TestParseAntigravityPlanType(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"paid tier wins", `{"paidTier":{"name":"Ultra"},"currentTier":{"name":"Pro"}}`, "Ultra"},
		{"paid tier id fallback", `{"paidTier":{"id":"ultra-tier"}}`, "ultra-tier"},
		{"current tier when eligible", `{"currentTier":{"name":"Pro"}}`, "Pro"},
		{"ineligible falls to default allowed tier", `{"currentTier":{"name":"Pro"},"ineligibleTiers":[{"reasonCode":"RESTRICTED"}],"allowedTiers":[{"isDefault":true,"name":"Free"}]}`, "Free (Restricted)"},
		{"ineligible without allowed tiers", `{"currentTier":{"name":"Pro"},"ineligibleTiers":[{"reasonCode":"RESTRICTED"}]}`, ""},
		{"empty", `{}`, ""},
	}
	for _, tc := range cases {
		if got := parseAntigravityPlanType(gjson.Parse(tc.body)); got != tc.want {
			t.Fatalf("%s: plan=%q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseAntigravityCodeAssistReadsProjectAndPlan(t *testing.T) {
	account := parseAntigravityCodeAssist([]byte(`{"cloudaicompanionProject":"real-project-123","paidTier":{"name":"Ultra"}}`))
	if account.projectID != "real-project-123" || account.planType != "Ultra" {
		t.Fatalf("account=%+v", account)
	}
	nested := parseAntigravityCodeAssist([]byte(`{"cloudaicompanionProject":{"id":"nested-project"}}`))
	if nested.projectID != "nested-project" {
		t.Fatalf("nested=%+v", nested)
	}
}

func TestSanitizeMsgDoesNotExposeBearerToken(t *testing.T) {
	if got := sanitizeMsg("Authorization: Bearer secret-token"); got != "upstream request failed" {
		t.Fatalf("got %q", got)
	}
}

func TestFetchXAIBillingParallelIsConcurrent(t *testing.T) {
	// Each side sleeps long enough that serial execution would exceed the budget.
	// Parallel must finish near the single-side latency and reach peak concurrency 2.
	const delay = 80 * time.Millisecond
	var current, peak atomic.Int32
	fetch := func(label string) func(context.Context) ([]byte, error) {
		return func(ctx context.Context) ([]byte, error) {
			n := current.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				current.Add(-1)
				return nil, ctx.Err()
			}
			current.Add(-1)
			return []byte(label), nil
		}
	}

	start := time.Now()
	wBody, wErr, mBody, mErr := fetchXAIBillingParallel(context.Background(), fetch("weekly"), fetch("monthly"))
	elapsed := time.Since(start)
	if wErr != nil || mErr != nil {
		t.Fatalf("errs weekly=%v monthly=%v", wErr, mErr)
	}
	if string(wBody) != "weekly" || string(mBody) != "monthly" {
		t.Fatalf("bodies=%q/%q", wBody, mBody)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak concurrency=%d want >=2 (serial would never overlap)", peak.Load())
	}
	// Serial would be ~2*delay; allow slack but require clearly sub-serial.
	if elapsed >= 2*delay-10*time.Millisecond {
		t.Fatalf("elapsed=%v looks serial (2*%v)", elapsed, delay)
	}
}

func TestFetchXAIBillingParallelPartialFailure(t *testing.T) {
	weeklyBody, weeklyErr, monthlyBody, monthlyErr := fetchXAIBillingParallel(
		context.Background(),
		func(context.Context) ([]byte, error) { return []byte(`{"config":{}}`), nil },
		func(context.Context) ([]byte, error) { return nil, fmt.Errorf("monthly down") },
	)
	if weeklyErr != nil {
		t.Fatalf("weekly err=%v", weeklyErr)
	}
	if monthlyErr == nil {
		t.Fatal("expected monthly error")
	}
	if len(weeklyBody) == 0 || len(monthlyBody) != 0 {
		t.Fatalf("weeklyBody=%q monthlyBody=%q", weeklyBody, monthlyBody)
	}
}

func TestParseClaudeUsageScopedLimitsAndRoutines(t *testing.T) {
	body := []byte(`{
		"five_hour":{"utilization":10,"resets_at":"2026-07-16T12:00:00Z"},
		"seven_day":{"utilization":40,"resets_at":"2026-07-20T12:00:00Z"},
		"seven_day_routines":{"utilization":5,"resets_at":"2026-07-20T12:00:00Z"},
		"limits":[
			{"kind":"weekly_scoped","group":"weekly","percent":30,"resets_at":"2026-07-20T12:00:00Z","scope":{"model":{"id":"claude-opus-5","display_name":"Opus"}}},
			{"kind":"weekly_scoped","group":"weekly","percent":80,"scope":{"model":{"id":"all-models","display_name":"All models"}}},
			{"kind":"five_hour","group":"session","percent":10,"scope":{"model":{"id":"claude-opus-5","display_name":"Opus"}}}
		]
	}`)
	items := parseClaudeUsage(body)
	routines := quotaByKey(items, "seven_day_cowork")
	if routines == nil || routines.Percent == nil || *routines.Percent != 95 {
		t.Fatalf("routines=%+v", routines)
	}
	opus := quotaByKey(items, "weekly_scoped_claude-opus-5")
	if opus == nil || opus.Percent == nil || *opus.Percent != 70 || opus.QuotaLabel != "claude_quota.model_weekly::Opus" || opus.ResetAt == nil {
		t.Fatalf("opus=%+v", opus)
	}
	if quotaByKey(items, "weekly_scoped_all-models") != nil {
		t.Fatal("all-models scope should be skipped")
	}
	if len(items) != 4 {
		t.Fatalf("items=%+v", items)
	}
}

func TestParseClaudeUsageScopedLimitSkippedWhenFlatWindowPresent(t *testing.T) {
	body := []byte(`{
		"seven_day_opus":{"utilization":20,"resets_at":"2026-07-20T12:00:00Z"},
		"limits":[
			{"kind":"weekly_scoped","group":"weekly","percent":30,"scope":{"model":{"id":"claude-opus-5","display_name":"Opus"}}}
		]
	}`)
	items := parseClaudeUsage(body)
	if len(items) != 1 || items[0].QuotaKey != "seven_day_opus" {
		t.Fatalf("items=%+v", items)
	}
}

func TestParseClaudePlanType(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`, "max_20x"},
		{`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_5x"}}`, "max_5x"},
		{`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_1x"}}`, "max"},
		{`{"organization":{"organization_type":"claude_pro","rate_limit_tier":"default_claude"}}`, "pro"},
		{`{"organization":{"organization_type":"claude_enterprise","rate_limit_tier":"default_claude_max_20x"}}`, "enterprise"},
		{`{"organization":{"organization_type":"claude_team"}}`, "team"},
		{`{"account":{"has_claude_max":true}}`, "max"},
		{`{"account":{"has_claude_pro":true}}`, "pro"},
		{`{}`, ""},
	}
	for _, tc := range cases {
		if got := parseClaudePlanType([]byte(tc.body)); got != tc.want {
			t.Fatalf("body=%s got=%q want=%q", tc.body, got, tc.want)
		}
	}
}

// weeklyUsedFromQuotas picks the cycle a card's "used this week" figure refers
// to. Providers that declare a primary key keep their exact-match behaviour;
// antigravity has none, and used to be handed whichever window happened to come
// first — a 5h window reported as the weekly number.
func TestWeeklyUsedFromQuotasPrefersWeeklyWindowWithoutPrimaryKey(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	quotas := []usage.QuotaWindowDTO{
		{QuotaKey: "antigravity:gemini_5h", Percent: pct(72), WindowSeconds: antigravityWindowFiveHour},
		{QuotaKey: "antigravity:gemini_weekly", Percent: pct(51), WindowSeconds: antigravityWindowWeek},
		{QuotaKey: "antigravity:3p_5h", Percent: pct(100), WindowSeconds: antigravityWindowFiveHour},
	}
	got := weeklyUsedFromQuotas(quotas)
	if got == nil || *got != 49 {
		t.Fatalf("used=%v, want 49 (from the weekly window)", got)
	}
}

func TestWeeklyUsedFromQuotasPrefersNarrowestWeeklyWindow(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	quotas := []usage.QuotaWindowDTO{
		{QuotaKey: "monthly", Percent: pct(90), WindowSeconds: 30 * 24 * 60 * 60},
		{QuotaKey: "weekly", Percent: pct(40), WindowSeconds: antigravityWindowWeek},
	}
	got := weeklyUsedFromQuotas(quotas)
	if got == nil || *got != 60 {
		t.Fatalf("used=%v, want 60 (weekly beats monthly)", got)
	}
}

// Only windows narrower than a week exist: the figure still has to come from
// somewhere, so the original first-window behaviour remains as the last resort.
func TestWeeklyUsedFromQuotasFallsBackWhenNoWeeklyWindow(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	quotas := []usage.QuotaWindowDTO{{QuotaKey: "only_5h", Percent: pct(30), WindowSeconds: antigravityWindowFiveHour}}
	got := weeklyUsedFromQuotas(quotas)
	if got == nil || *got != 70 {
		t.Fatalf("used=%v, want 70", got)
	}
}

// Providers that declare a primary weekly key must not start matching by window
// width: an unrelated window of the same width would silently take over.
func TestWeeklyUsedFromQuotasKeepsExactMatchForKeyedProviders(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	quotas := []usage.QuotaWindowDTO{
		{QuotaKey: "review_week", Percent: pct(10), WindowSeconds: antigravityWindowWeek},
		{QuotaKey: "code_week", Percent: pct(80), WindowSeconds: antigravityWindowWeek},
	}
	got := weeklyUsedFromQuotas(quotas, "code_week")
	if got == nil || *got != 20 {
		t.Fatalf("used=%v, want 20 (code_week, not review_week)", got)
	}
	if missing := weeklyUsedFromQuotas(quotas, "seven_day"); missing != nil {
		t.Fatalf("used=%v, want nil when the declared key is absent", missing)
	}
}

// The card composes "<group> · <window>" itself, so the label must carry the
// group name alone. Pairing it with the upstream's bucket wording produced
// "Gemini Models · Weekly Limit Remaining", which is too long for a card row
// and cannot be localised.
func TestParseAntigravityQuotaSummaryLabelsRowsByGroupOnly(t *testing.T) {
	body := []byte(`{"groups":[
		{"displayName":"Gemini Models","description":"Models within this group: Gemini Flash, Gemini Pro","buckets":[
			{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.51,"displayName":"Weekly Limit Remaining"},
			{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.72,"displayName":"Five Hour Limit Remaining","description":"Quota available"}
		]}
	]}`)
	items := parseAntigravityQuotaSummary(body)
	if len(items) != 2 {
		t.Fatalf("items=%d, want 2: %+v", len(items), items)
	}
	for _, item := range items {
		if item.QuotaLabel != "Gemini Models" {
			t.Fatalf("label=%q, want the group name alone", item.QuotaLabel)
		}
	}

	weekly := quotaByKey(items, "antigravity:gemini_weekly")
	if weekly == nil || weekly.Meta != "Models within this group: Gemini Flash, Gemini Pro" {
		t.Fatalf("weekly meta=%+v, want the group description", weekly)
	}
	// A bucket that describes itself gets both, so the hint says which models
	// the group covers and what state this particular window is in.
	fiveHour := quotaByKey(items, "antigravity:gemini_5h")
	if fiveHour == nil || fiveHour.Meta != "Models within this group: Gemini Flash, Gemini Pro · Quota available" {
		t.Fatalf("5h meta=%+v, want group and bucket descriptions joined", fiveHour)
	}
}

// Only the products this proxy feeds carry the weekly window. That window is
// what the budget projection selects on, so tagging Grok Chat with it would let
// the projection anchor on consumption produced somewhere else entirely.
func TestParseXAIWeeklyBillingWindowsOnlyAttributableProducts(t *testing.T) {
	body := []byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-20T06:45:51Z","end":"2026-08-27T06:45:51Z"},` +
		`"creditUsagePercent":19,"productUsage":[{"product":"GrokBuild","usagePercent":16},{"product":"GrokChat","usagePercent":3},{"product":"GrokImagine"}]}}`)
	items := parseXAIWeeklyBilling(body)

	build := quotaByKey(items, "product:GrokBuild")
	if build == nil || build.Percent == nil || *build.Percent != 84 {
		t.Fatalf("build=%+v", build)
	}
	if build.WindowSeconds != 604800 || build.ResetAt == nil {
		t.Fatalf("attributable product must carry the weekly window: %+v", build)
	}

	for _, key := range []string{"product:GrokChat", "product:GrokImagine"} {
		item := quotaByKey(items, key)
		if item == nil {
			t.Fatalf("%s missing", key)
		}
		if item.WindowSeconds != 0 || item.ResetAt != nil {
			t.Fatalf("%s must stay display-only, got window=%d reset=%v", key, item.WindowSeconds, item.ResetAt)
		}
	}

	// The pool-wide window keeps reporting total consumption; it is what the
	// "weekly quota used" figure shows.
	weekly := quotaByKey(items, "weekly_limit")
	if weekly == nil || weekly.Percent == nil || *weekly.Percent != 81 || weekly.WindowSeconds != 604800 {
		t.Fatalf("weekly=%+v", weekly)
	}
}

// Percentages reach the panel unrounded now; the displayed value is formatted
// at the edge instead.
func TestParseXAIWeeklyBillingKeepsFractionalPercent(t *testing.T) {
	body := []byte(`{"config":{"currentPeriod":{"type":"WEEKLY","end":"2026-08-27T00:00:00Z"},"creditUsagePercent":2.4,` +
		`"productUsage":[{"product":"GrokBuild","usagePercent":2.4}]}}`)
	items := parseXAIWeeklyBilling(body)
	weekly := quotaByKey(items, "weekly_limit")
	if weekly == nil || weekly.Percent == nil || *weekly.Percent != 97.6 {
		t.Fatalf("weekly percent=%+v, want 97.6", weekly)
	}
	if weekly.Value != "98%" {
		t.Fatalf("weekly display value=%q, want 98%%", weekly.Value)
	}
}
