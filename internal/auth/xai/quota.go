package xai

import (
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	// BillingWeeklyPath is the Grok Build weekly included-usage endpoint.
	BillingWeeklyPath = "/billing?format=credits"
	// CLIWeeklyBillingURL is the production Grok Build weekly included-usage URL.
	CLIWeeklyBillingURL = CLIChatProxyBaseURL + BillingWeeklyPath
	// WeeklyExhaustedRemainingPercent is the remaining share below which the
	// weekly allowance counts as spent.
	//
	// Percentages are kept at upstream precision (see ParseWeeklyBilling), so a
	// credential can sit at 0.2% remaining — enough to pass a "> 0" check, not
	// enough to serve a request. Treating that as recovered makes the conductor
	// hand the credential out, take the 402, cool down, re-probe, and repeat
	// until the real weekly reset.
	WeeklyExhaustedRemainingPercent = 0.5
)

// WeeklyBilling describes the quota fields shared by runtime recovery probes and
// the management account-status view.
//
// Grok bills SuperGrok as one weekly credit pool shared by every product
// (isUnifiedBillingUser). RemainingPercent therefore covers Grok Chat, Imagine
// and App Builder usage as well — traffic this proxy never sees. Anything that
// has to line up an upstream percentage with locally recorded cost must use
// AttributableUsedPercent instead; see that method for why.
type WeeklyBilling struct {
	RemainingPercent float64
	PeriodStart      time.Time
	ResetAt          time.Time
	Products         []WeeklyProductBilling
}

// WeeklyProductBilling describes one product-specific weekly usage entry.
//
// Products the account has not touched come back without a usagePercent field
// at all, which is why HasUsage exists: "absent" and "reported as 0" are the
// same number but not the same fact, and only a reported value may be summed
// into an attributable total.
type WeeklyProductBilling struct {
	Name             string
	RemainingPercent float64
	HasUsage         bool
	// Attributable marks products whose weekly consumption is produced by
	// traffic this proxy forwards.
	Attributable bool
}

// attributableWeeklyProducts lists the Grok products whose weekly consumption
// this proxy actually produces, keyed by normalized product name.
//
// OAuth (SuperGrok) chat traffic all leaves through cli-chat-proxy.grok.com,
// which upstream accounts for as Grok Build. Image and video generation is sent
// to api.x.ai instead (see xaiMediaBaseURL) and is billed against API credits,
// not this weekly pool — GrokImagine stays untouched on accounts that generate
// media through the proxy, which is why it is not listed here. GrokChat and
// GrokAppBuilder are first-party surfaces the proxy has no part in.
//
// "grokcode" is the pre-rename key and is kept so accounts still reported under
// it do not silently fall back to the unattributable total.
var attributableWeeklyProducts = map[string]struct{}{
	"grokbuild": {},
	"grokcode":  {},
}

// ParseWeeklyBilling parses the Grok Build billing response's weekly usage data.
//
// Percentages are stored exactly as upstream reports them. Rounding them to
// whole percents used to look harmless because the panel prints integers, but
// the value is also the divisor of the weekly-budget projection: at 2% consumed
// a half-point of rounding moves the projected budget by a quarter.
func ParseWeeklyBilling(body []byte) (WeeklyBilling, bool) {
	cfg := gjson.GetBytes(body, "config")
	if !cfg.Exists() {
		return WeeklyBilling{}, false
	}
	current := firstBillingResult(cfg, "currentPeriod", "current_period")
	periodType := strings.ToLower(strings.TrimSpace(current.Get("type").String()))
	used := firstBillingResult(cfg, "creditUsagePercent", "credit_usage_percent")
	products := firstBillingResult(cfg, "productUsage", "product_usage")
	if !used.Exists() && !strings.Contains(periodType, "weekly") && !products.IsArray() {
		return WeeklyBilling{}, false
	}

	weekly := WeeklyBilling{RemainingPercent: 100}
	if used.Exists() {
		weekly.RemainingPercent = 100 - clampBillingPercent(used.Float())
	}
	reset := firstBillingResult(current, "end")
	if !reset.Exists() {
		reset = firstBillingResult(cfg, "billingPeriodEnd", "billing_period_end")
	}
	weekly.ResetAt = parseBillingTime(reset)
	start := firstBillingResult(current, "start")
	if !start.Exists() {
		start = firstBillingResult(cfg, "billingPeriodStart", "billing_period_start")
	}
	weekly.PeriodStart = parseBillingTime(start)

	if products.IsArray() {
		weekly.Products = make([]WeeklyProductBilling, 0)
		products.ForEach(func(_, product gjson.Result) bool {
			name := strings.TrimSpace(product.Get("product").String())
			item := WeeklyProductBilling{
				Name:             name,
				RemainingPercent: 100,
				Attributable:     IsAttributableWeeklyProduct(name),
			}
			if productUsed := firstBillingResult(product, "usagePercent", "usage_percent"); productUsed.Exists() {
				item.RemainingPercent = 100 - clampBillingPercent(productUsed.Float())
				item.HasUsage = true
			}
			weekly.Products = append(weekly.Products, item)
			return true
		})
	}
	return weekly, true
}

// IsAttributableWeeklyProduct reports whether a product's weekly consumption is
// produced by traffic this proxy forwards.
func IsAttributableWeeklyProduct(name string) bool {
	_, ok := attributableWeeklyProducts[normalizeWeeklyProductName(name)]
	return ok
}

// AttributableUsedPercent returns the share of the weekly pool consumed by
// products this proxy forwards to, and whether upstream reported enough to say.
//
// This is the divisor the weekly-budget projection needs. Dividing locally
// recorded cost by the pool-wide consumption instead answers a question nobody
// asked: the numerator only covers Grok Build requests, so every percent Grok
// Chat burns on the web shrinks the projected budget of an account whose budget
// did not change. Product shares sum to the pool total (16% Build + 3% Chat =
// 19% overall on a live account), so restricting the divisor to attributable
// products restores the pool-sized answer.
//
// Reports false when upstream sent no product breakdown at all; callers fall
// back to the pool-wide percentage rather than invent one.
func (w WeeklyBilling) AttributableUsedPercent() (float64, bool) {
	var used float64
	var reported bool
	for _, product := range w.Products {
		if !product.Attributable || !product.HasUsage {
			continue
		}
		used += clampBillingPercent(100 - product.RemainingPercent)
		reported = true
	}
	if !reported {
		return 0, false
	}
	return clampBillingPercent(used), true
}

// WindowSeconds returns the length of the reported billing period.
//
// Upstream sends both ends of the period, so the width does not have to be
// assumed. Readers derive the cycle start as reset_at - window, and a hard-coded
// seven days silently misplaces that start whenever a plan change shortens or
// stretches the period — which in turn misplaces the cost window the projection
// divides. Falls back to the nominal week when either end is missing, and never
// reports less than a week so the window still registers as the weekly one for
// readers that select windows by width.
func (w WeeklyBilling) WindowSeconds() int64 {
	const nominalWeek = int64(7 * 24 * 60 * 60)
	if w.PeriodStart.IsZero() || w.ResetAt.IsZero() {
		return nominalWeek
	}
	seconds := int64(w.ResetAt.Sub(w.PeriodStart).Round(time.Second) / time.Second)
	if seconds < nominalWeek {
		return nominalWeek
	}
	return seconds
}

// Exhausted reports whether the weekly allowance is spent.
func (w WeeklyBilling) Exhausted() bool {
	return w.RemainingPercent < WeeklyExhaustedRemainingPercent
}

func normalizeWeeklyProductName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstBillingResult(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if value := root.Get(path); value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func parseBillingTime(value gjson.Result) time.Time {
	if !value.Exists() {
		return time.Time{}
	}
	raw := strings.TrimSpace(value.String())
	if raw != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, raw); err == nil {
				return parsed.UTC()
			}
		}
	}
	seconds := value.Float()
	if seconds <= 0 {
		return time.Time{}
	}
	if seconds > 1e12 {
		seconds /= 1000
	}
	return time.Unix(int64(seconds), int64((seconds-float64(int64(seconds)))*float64(time.Second))).UTC()
}

func clampBillingPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
