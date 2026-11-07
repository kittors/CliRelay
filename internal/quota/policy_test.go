package quota

import (
	"errors"
	"strings"
	"testing"
)

func ruleLimits(metric Metric, window Period, value float64) Matrix {
	m := Matrix{}
	m.Set(metric, window, value)
	return m
}

func TestNormalizeAccessPolicyStripsColumnCellsAndKeepsPolicyCells(t *testing.T) {
	limits := Matrix{}
	limits.Set(MetricCost, PeriodDay, 10)           // 账号固定列承载
	limits.Set(MetricRequests, PeriodDay, 100)      // 账号固定列承载
	limits.Set(MetricRequests, PeriodWeek, 500)     // 策略承载
	limits.Set(MetricTokens, PeriodLifetime, 1_000) // 策略承载
	out, err := NormalizeAccessPolicy(AccessPolicy{Limits: limits}, AccountColumnLayout)
	if err != nil {
		t.Fatal(err)
	}
	if out.Limits.Cost.Day != 0 || out.Limits.Requests.Day != 0 {
		t.Fatalf("column cells must not be duplicated into the policy: %+v", out.Limits)
	}
	if out.Limits.Requests.Week != 500 || out.Limits.Tokens.Lifetime != 1000 {
		t.Fatalf("policy cells lost: %+v", out.Limits)
	}

	// owned key 只有美元四周期列，请求次数与累计消费都在策略里。
	out, err = NormalizeAccessPolicy(AccessPolicy{Limits: limits}, OwnedKeyColumnLayout)
	if err != nil {
		t.Fatal(err)
	}
	if out.Limits.Cost.Day != 0 || out.Limits.Requests.Day != 100 {
		t.Fatalf("owned key layout = %+v", out.Limits)
	}
}

func TestColumnLayoutMergeIsSingleSourcePerCell(t *testing.T) {
	columns, policy := Matrix{}, Matrix{}
	columns.Set(MetricCost, PeriodWeek, 7)
	columns.Set(MetricTokens, PeriodDay, 999) // 账号没有这列：应被忽略
	policy.Set(MetricCost, PeriodWeek, 123)   // 账号这一格以列为准：应被忽略
	policy.Set(MetricTokens, PeriodDay, 50)
	merged := AccountColumnLayout.Merge(columns, policy)
	if merged.Cost.Week != 7 || merged.Tokens.Day != 50 {
		t.Fatalf("merged = %+v", merged)
	}
	if !ProfileOwnsCell(MetricCost, PeriodDay) || ProfileOwnsCell(MetricCost, PeriodLifetime) {
		t.Fatal("ProfileOwnsCell mismatch")
	}
	if ProfileColumnLayout.Has(MetricCost, PeriodLifetime) || !AccountColumnLayout.Has(MetricCost, PeriodLifetime) {
		t.Fatal("lifetime cost column layout mismatch")
	}
}

func TestNormalizeAccessPolicyRules(t *testing.T) {
	policy := AccessPolicy{ModelRules: []ModelRule{{
		Name:   "  Opus  ",
		Models: []string{" claude-opus-* ", "CLAUDE-OPUS-*", "claude-opus-4-5"},
		Limits: ruleLimits(MetricCost, PeriodWeek, 99.5),
	}}}
	out, err := NormalizeAccessPolicy(policy, AccountColumnLayout)
	if err != nil {
		t.Fatal(err)
	}
	rule := out.ModelRules[0]
	if !strings.HasPrefix(rule.ID, "r-") || rule.Name != "Opus" {
		t.Fatalf("rule = %+v", rule)
	}
	if len(rule.Models) != 2 || rule.Models[0] != "claude-opus-*" || rule.Models[1] != "claude-opus-4-5" {
		t.Fatalf("models must be trimmed and de-duplicated case-insensitively: %+v", rule.Models)
	}
	if rule.Limits.Cost.Week != 100 {
		t.Fatalf("limit must be normalized: %+v", rule.Limits)
	}

	invalid := []struct {
		name   string
		policy AccessPolicy
		field  string
	}{
		{"no models", AccessPolicy{ModelRules: []ModelRule{{Limits: ruleLimits(MetricCost, PeriodDay, 1)}}}, "model-rules[0].models"},
		{"star alone", AccessPolicy{ModelRules: []ModelRule{{Models: []string{"*"}, Limits: ruleLimits(MetricCost, PeriodDay, 1)}}}, "model-rules[0].models[0]"},
		{"no limits", AccessPolicy{ModelRules: []ModelRule{{Models: []string{"gpt-5"}}}}, "model-rules[0].limits"},
		{"negative", AccessPolicy{ModelRules: []ModelRule{{Models: []string{"gpt-5"}, Limits: ruleLimits(MetricTokens, PeriodDay, -1)}}}, "model-rules[0].limits"},
		{"duplicate id", AccessPolicy{ModelRules: []ModelRule{
			{ID: "a", Models: []string{"gpt-5"}, Limits: ruleLimits(MetricCost, PeriodDay, 1)},
			{ID: "a", Models: []string{"gpt-4o"}, Limits: ruleLimits(MetricCost, PeriodDay, 1)},
		}}, "model-rules[1].id"},
	}
	for _, tc := range invalid {
		_, err := NormalizeAccessPolicy(tc.policy, AccountColumnLayout)
		var verr *PolicyValidationError
		if !errors.As(err, &verr) || verr.Field != tc.field || !errors.Is(err, ErrInvalidAccessPolicy) {
			t.Fatalf("%s: err = %v, want field %s", tc.name, err, tc.field)
		}
	}
}

func TestNormalizeAccessPolicyRoutes(t *testing.T) {
	out, err := NormalizeAccessPolicy(AccessPolicy{ModelRoutes: []ModelRoute{{
		From: []string{"claude-opus-*"}, To: " claude-sonnet-4-5 ",
	}}}, AccountColumnLayout)
	if err != nil {
		t.Fatal(err)
	}
	if out.ModelRoutes[0].To != "claude-sonnet-4-5" || out.ModelRoutes[0].ID == "" {
		t.Fatalf("route = %+v", out.ModelRoutes[0])
	}
	invalid := []struct {
		name  string
		route ModelRoute
		field string
	}{
		{"empty target", ModelRoute{From: []string{"a"}}, "model-routes[0].to"},
		{"pattern target", ModelRoute{From: []string{"a"}, To: "b-*"}, "model-routes[0].to"},
		{"self match", ModelRoute{From: []string{"claude-*"}, To: "claude-sonnet-4-5"}, "model-routes[0].to"},
		{"no source", ModelRoute{To: "b"}, "model-routes[0].from"},
	}
	for _, tc := range invalid {
		_, err := NormalizeAccessPolicy(AccessPolicy{ModelRoutes: []ModelRoute{tc.route}}, AccountColumnLayout)
		var verr *PolicyValidationError
		if !errors.As(err, &verr) || verr.Field != tc.field {
			t.Fatalf("%s: err = %v, want field %s", tc.name, err, tc.field)
		}
	}
}

func TestResolveModelRouteIsSingleHop(t *testing.T) {
	routes := []ModelRoute{
		{ID: "1", From: []string{"a"}, To: "b"},
		{ID: "2", From: []string{"b"}, To: "c"},
	}
	if got, route := ResolveModelRoute(routes, "A(high)"); got != "b" || route == nil || route.ID != "1" {
		t.Fatalf("route a = %q %+v, want b via 1", got, route)
	}
	if got, route := ResolveModelRoute(routes, "z"); got != "z" || route != nil {
		t.Fatalf("unmatched = %q %+v", got, route)
	}
}

func TestMatchingRulesReturnsEveryOverlappingRule(t *testing.T) {
	rules := []ModelRule{
		{ID: "all-claude", Models: []string{"claude-*"}},
		{ID: "opus", Models: []string{"claude-opus-*"}},
		{ID: "gpt", Models: []string{"gpt-*"}},
	}
	got := MatchingRules(rules, "claude-opus-4-5")
	if len(got) != 2 || got[0].ID != "all-claude" || got[1].ID != "opus" {
		t.Fatalf("matching = %+v", got)
	}
	if MatchingRules(rules, "") != nil {
		t.Fatal("empty model must match nothing")
	}
}

func TestAccessPolicyEncodeDecodeRoundTrip(t *testing.T) {
	if got := EncodeAccessPolicy(AccessPolicy{}); got != "{}" {
		t.Fatalf("empty = %q", got)
	}
	for _, raw := range []string{"", "{}", "null", "  "} {
		if policy, err := DecodeAccessPolicy(raw); err != nil || !policy.IsEmpty() {
			t.Fatalf("decode %q = %+v, %v", raw, policy, err)
		}
	}
	if _, err := DecodeAccessPolicy("{not json"); !errors.Is(err, ErrInvalidAccessPolicy) {
		t.Fatalf("bad json err = %v", err)
	}
	policy := AccessPolicy{
		Limits:      ruleLimits(MetricTokens, PeriodMonth, 5),
		ModelRules:  []ModelRule{{ID: "r", Models: []string{"gpt-5"}, Limits: ruleLimits(MetricRequests, PeriodDay, 3)}},
		ModelRoutes: []ModelRoute{{ID: "x", From: []string{"a"}, To: "b"}},
	}
	decoded, err := DecodeAccessPolicy(EncodeAccessPolicy(policy))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Limits.Tokens.Month != 5 || decoded.ModelRules[0].Limits.Requests.Day != 3 || decoded.ModelRoutes[0].To != "b" {
		t.Fatalf("round trip = %+v", decoded)
	}
	clone := decoded.Clone()
	clone.ModelRules[0].Models[0] = "mutated"
	if decoded.ModelRules[0].Models[0] != "gpt-5" {
		t.Fatal("Clone must deep copy model lists")
	}
}

func TestProfileLayoutForbidsLifetimeCost(t *testing.T) {
	limits := Matrix{}
	limits.Set(MetricCost, PeriodLifetime, 500)
	limits.Set(MetricTokens, PeriodWeek, 7)
	out, err := NormalizeAccessPolicy(AccessPolicy{Limits: limits}, ProfileColumnLayout)
	if err != nil {
		t.Fatal(err)
	}
	if out.Limits.Cost.Lifetime != 0 || out.Limits.Tokens.Week != 7 {
		t.Fatalf("profile policy limits = %+v", out.Limits)
	}
	if owned := ProfileColumnLayout.OwnedCellsOnly(limits); owned.Cost.Lifetime != 0 || owned.Tokens.Week != 7 {
		t.Fatalf("owned cells = %+v", owned)
	}
}
