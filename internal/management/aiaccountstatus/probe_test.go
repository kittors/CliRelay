package aiaccountstatus

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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

func TestParseAntigravityModelsSummarizesWorstRemainingAndEarliestReset(t *testing.T) {
	body := []byte(`{"models":{
		"gemini-3-pro-high":{"quotaInfo":{"remainingFraction":0.8,"resetTime":"2026-07-20T00:00:00Z"}},
		"gemini-3.1-pro-low":{"quota_info":{"remaining_fraction":0.4,"reset_time":"2026-07-19T00:00:00Z"}},
		"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.1}}
	}}`)
	items := parseAntigravityModels(body)
	pro := quotaByKey(items, "provider:gemini3-pro")
	if pro == nil || pro.Percent == nil || *pro.Percent != 40 || pro.ResetAt == nil || pro.ResetAt.Format(time.RFC3339) != "2026-07-19T00:00:00Z" {
		t.Fatalf("pro=%+v", pro)
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
