package xai

import (
	"math"
	"testing"
	"time"
)

// liveWeeklyBillingBody is a verbatim capture of a SuperGrok account's
// /billing?format=credits response. It is the shape every claim below rests on:
// product shares sum to the pool-wide percentage (16 + 3 = 19), untouched
// products omit usagePercent entirely, and both period ends are present.
const liveWeeklyBillingBody = `{
  "config": {
    "currentPeriod": {
      "type": "USAGE_PERIOD_TYPE_WEEKLY",
      "start": "2026-08-20T06:45:51.606435+00:00",
      "end": "2026-08-27T06:45:51.606435+00:00"
    },
    "creditUsagePercent": 19.0,
    "onDemandCap": {"val": 0},
    "onDemandUsed": {"val": 0},
    "productUsage": [
      {"product": "GrokBuild", "usagePercent": 16.0},
      {"product": "GrokChat", "usagePercent": 3.0},
      {"product": "GrokAppBuilder"},
      {"product": "GrokImagine"}
    ],
    "isUnifiedBillingUser": true,
    "prepaidBalance": {"val": 0},
    "billingPeriodStart": "2026-08-20T06:45:51.606435+00:00",
    "billingPeriodEnd": "2026-08-27T06:45:51.606435+00:00"
  }
}`

func TestParseWeeklyBillingSeparatesAttributableFromPoolWide(t *testing.T) {
	weekly, ok := ParseWeeklyBilling([]byte(liveWeeklyBillingBody))
	if !ok {
		t.Fatal("ParseWeeklyBilling rejected a live response body")
	}
	if weekly.RemainingPercent != 81 {
		t.Fatalf("pool-wide remaining=%v, want 81", weekly.RemainingPercent)
	}

	// The whole point of the fix: the divisor for the budget projection is the
	// 16% Grok Build burned, not the 19% the account burned everywhere.
	used, reported := weekly.AttributableUsedPercent()
	if !reported {
		t.Fatal("attributable share not reported for a body carrying GrokBuild usage")
	}
	if used != 16 {
		t.Fatalf("attributable used=%v, want 16 (Grok Chat's 3%% must not count)", used)
	}

	if want := time.Date(2026, 8, 20, 6, 45, 51, 606435000, time.UTC); !weekly.PeriodStart.Equal(want) {
		t.Fatalf("period start=%v, want %v", weekly.PeriodStart, want)
	}
	if got := weekly.WindowSeconds(); got != 604800 {
		t.Fatalf("window=%d, want 604800", got)
	}
	if weekly.Exhausted() {
		t.Fatal("81% remaining must not read as exhausted")
	}
}

func TestParseWeeklyBillingFlagsProductsAndUsageReporting(t *testing.T) {
	weekly, ok := ParseWeeklyBilling([]byte(liveWeeklyBillingBody))
	if !ok {
		t.Fatal("ParseWeeklyBilling rejected a live response body")
	}
	byName := make(map[string]WeeklyProductBilling, len(weekly.Products))
	for _, product := range weekly.Products {
		byName[product.Name] = product
	}
	for _, tc := range []struct {
		name         string
		attributable bool
		hasUsage     bool
		remaining    float64
	}{
		{"GrokBuild", true, true, 84},
		{"GrokChat", false, true, 97},
		{"GrokAppBuilder", false, false, 100},
		{"GrokImagine", false, false, 100},
	} {
		product, ok := byName[tc.name]
		if !ok {
			t.Fatalf("%s missing from parsed products", tc.name)
		}
		if product.Attributable != tc.attributable {
			t.Fatalf("%s attributable=%v, want %v", tc.name, product.Attributable, tc.attributable)
		}
		if product.HasUsage != tc.hasUsage {
			t.Fatalf("%s hasUsage=%v, want %v", tc.name, product.HasUsage, tc.hasUsage)
		}
		if product.RemainingPercent != tc.remaining {
			t.Fatalf("%s remaining=%v, want %v", tc.name, product.RemainingPercent, tc.remaining)
		}
	}
}

// Rounding to whole percents is invisible in the panel and ruinous in the
// projection: at 2.4% consumed, rounding to 2% inflates the projected budget by
// 20%.
func TestParseWeeklyBillingKeepsFractionalPrecision(t *testing.T) {
	body := []byte(`{"config":{"currentPeriod":{"type":"WEEKLY","end":"2026-08-27T00:00:00Z"},"creditUsagePercent":2.4,` +
		`"productUsage":[{"product":"GrokBuild","usagePercent":2.4}]}}`)
	weekly, ok := ParseWeeklyBilling(body)
	if !ok {
		t.Fatal("ParseWeeklyBilling rejected a fractional body")
	}
	if math.Abs(weekly.RemainingPercent-97.6) > 1e-9 {
		t.Fatalf("pool remaining=%v, want 97.6", weekly.RemainingPercent)
	}
	used, reported := weekly.AttributableUsedPercent()
	if !reported || math.Abs(used-2.4) > 1e-9 {
		t.Fatalf("attributable used=%v reported=%v, want 2.4", used, reported)
	}
}

// A body with no product breakdown must say so rather than answer 0, which the
// caller would divide by.
func TestAttributableUsedPercentUnreportedWithoutProducts(t *testing.T) {
	weekly, ok := ParseWeeklyBilling([]byte(`{"config":{"currentPeriod":{"type":"WEEKLY","end":"2026-08-27T00:00:00Z"},"creditUsagePercent":19}}`))
	if !ok {
		t.Fatal("ParseWeeklyBilling rejected a product-less body")
	}
	if used, reported := weekly.AttributableUsedPercent(); reported {
		t.Fatalf("reported=%v used=%v, want unreported", reported, used)
	}
}

// Every product the account owns being non-attributable is still an answer:
// nothing this proxy forwards has consumed the pool.
func TestAttributableUsedPercentZeroWhenOnlyOtherProductsSpent(t *testing.T) {
	weekly, ok := ParseWeeklyBilling([]byte(`{"config":{"currentPeriod":{"type":"WEEKLY","end":"2026-08-27T00:00:00Z"},` +
		`"creditUsagePercent":9,"productUsage":[{"product":"GrokBuild"},{"product":"GrokChat","usagePercent":9}]}}`))
	if !ok {
		t.Fatal("ParseWeeklyBilling rejected the body")
	}
	if used, reported := weekly.AttributableUsedPercent(); reported || used != 0 {
		t.Fatalf("used=%v reported=%v, want 0/false (GrokBuild reported no usage)", used, reported)
	}
}

func TestIsAttributableWeeklyProductNormalizesNames(t *testing.T) {
	for _, name := range []string{"GrokBuild", "grok-build", "grok_build", " GROK BUILD ", "grok-code"} {
		if !IsAttributableWeeklyProduct(name) {
			t.Fatalf("%q should be attributable", name)
		}
	}
	for _, name := range []string{"GrokChat", "GrokImagine", "GrokAppBuilder", "", "grokbuilder"} {
		if IsAttributableWeeklyProduct(name) {
			t.Fatalf("%q must not be attributable", name)
		}
	}
}

// Exhaustion is a threshold, not "> 0". A sliver of remaining allowance cannot
// serve a request, and calling it recovered makes the conductor hand out the
// credential, take a 402, cool down and re-probe in a loop.
func TestExhaustedUsesThresholdNotStrictZero(t *testing.T) {
	for _, tc := range []struct {
		remaining float64
		exhausted bool
	}{
		{0, true},
		{0.2, true},
		{0.5, false},
		{1, false},
		{81, false},
	} {
		if got := (WeeklyBilling{RemainingPercent: tc.remaining}).Exhausted(); got != tc.exhausted {
			t.Fatalf("remaining=%v exhausted=%v, want %v", tc.remaining, got, tc.exhausted)
		}
	}
}

// The cycle start readers derive is reset_at minus this width, so a period that
// is not exactly seven days long has to report its real width.
func TestWindowSecondsFollowsReportedPeriod(t *testing.T) {
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		weekly WeeklyBilling
		want   int64
	}{
		{"nominal week", WeeklyBilling{PeriodStart: start, ResetAt: start.AddDate(0, 0, 7)}, 604800},
		{"longer period", WeeklyBilling{PeriodStart: start, ResetAt: start.AddDate(0, 0, 10)}, 864000},
		// A short period still has to register as the weekly window for readers
		// that select windows by width, so it floors at a week.
		{"short period floors at a week", WeeklyBilling{PeriodStart: start, ResetAt: start.AddDate(0, 0, 3)}, 604800},
		{"missing start", WeeklyBilling{ResetAt: start.AddDate(0, 0, 7)}, 604800},
	}
	for _, tc := range cases {
		if got := tc.weekly.WindowSeconds(); got != tc.want {
			t.Fatalf("%s: window=%d, want %d", tc.name, got, tc.want)
		}
	}
}
