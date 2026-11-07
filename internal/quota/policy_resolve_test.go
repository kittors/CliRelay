package quota

import (
	"errors"
	"testing"
)

// fixedUsage 返回一个按模型名查表的用量回调，并统计被调用的次数。
func fixedUsage(byModel map[string]Matrix, calls *int) func([]string) (Matrix, error) {
	return func(patterns []string) (Matrix, error) {
		*calls++
		out := Matrix{}
		for model, used := range byModel {
			if MatchesAnyModelPattern(patterns, model) {
				out = out.Add(used)
			}
		}
		return out, nil
	}
}

func spent(cost float64) Matrix {
	m := Matrix{}
	m.Set(MetricCost, PeriodWeek, cost)
	return m
}

func TestResolveModelPolicyPassThroughWithoutRulesDoesNotReadUsage(t *testing.T) {
	calls := 0
	layers := []ScopedRules{{Scope: RuleScopeAccount, Rules: []ModelRule{{ID: "gpt", Models: []string{"gpt-*"}, Limits: spent(1)}}, Usage: fixedUsage(nil, &calls)}}
	decision, err := ResolveModelPolicy("claude-sonnet-4-5", nil, layers)
	if err != nil || decision.Exceeded != nil || decision.FinalModel != "claude-sonnet-4-5" || decision.Rewritten() {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
	if calls != 0 {
		t.Fatalf("usage read %d times for a request matching no rule", calls)
	}
}

func TestResolveModelPolicyAppliesRouteBeforeRules(t *testing.T) {
	calls := 0
	routes := []ModelRoute{{ID: "down", From: []string{"claude-opus-*"}, To: "claude-sonnet-4-5"}}
	layers := []ScopedRules{{Scope: RuleScopeAccount, Rules: []ModelRule{
		{ID: "sonnet", Models: []string{"claude-sonnet-*"}, Limits: spent(10)},
	}, Usage: fixedUsage(map[string]Matrix{"claude-sonnet-4-5": spent(3)}, &calls)}}
	decision, err := ResolveModelPolicy("claude-opus-4-5", routes, layers)
	if err != nil || decision.Exceeded != nil {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
	if decision.RoutedModel != "claude-sonnet-4-5" || decision.FinalModel != "claude-sonnet-4-5" || decision.Route == nil || !decision.Rewritten() {
		t.Fatalf("decision = %+v", decision)
	}
	if calls != 1 {
		t.Fatalf("usage calls = %d, want 1", calls)
	}
}

func TestResolveModelPolicyRejectsExceededRuleWithoutFallback(t *testing.T) {
	calls := 0
	layers := []ScopedRules{{Scope: RuleScopeAccount, Rules: []ModelRule{
		{ID: "opus", Name: "Opus", Models: []string{"claude-opus-*"}, Limits: spent(100)},
	}, Usage: fixedUsage(map[string]Matrix{"claude-opus-4-5": spent(100)}, &calls)}}
	decision, err := ResolveModelPolicy("claude-opus-4-5", nil, layers)
	if err != nil || decision.Exceeded == nil {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
	ex := decision.Exceeded
	if ex.Rule.ID != "opus" || ex.Metric != MetricCost || ex.Window != PeriodWeek || ex.Limit != 100 || ex.Current != 100 || ex.Scope != RuleScopeAccount {
		t.Fatalf("exceeded = %+v", ex)
	}
}

func TestResolveModelPolicyFallsBackAndRechecksTheFallbackModel(t *testing.T) {
	calls := 0
	usage := fixedUsage(map[string]Matrix{"claude-opus-4-5": spent(100), "claude-sonnet-4-5": spent(5)}, &calls)
	layers := []ScopedRules{{Scope: RuleScopeAccount, Rules: []ModelRule{
		{ID: "opus", Models: []string{"claude-opus-*"}, Limits: spent(100), FallbackModel: "claude-sonnet-4-5"},
		{ID: "sonnet", Models: []string{"claude-sonnet-*"}, Limits: spent(50)},
	}, Usage: usage}}
	decision, err := ResolveModelPolicy("claude-opus-4-5", nil, layers)
	if err != nil || decision.Exceeded != nil {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
	if decision.FinalModel != "claude-sonnet-4-5" || len(decision.Fallbacks) != 1 || decision.Fallbacks[0].From != "claude-opus-4-5" {
		t.Fatalf("decision = %+v", decision)
	}

	// 降级后的模型被另一条（无降级的）规则拦下：报告那条规则。
	usage = fixedUsage(map[string]Matrix{"claude-opus-4-5": spent(100), "claude-sonnet-4-5": spent(50)}, &calls)
	layers[0].Usage = usage
	decision, err = ResolveModelPolicy("claude-opus-4-5", nil, layers)
	if err != nil || decision.Exceeded == nil || decision.Exceeded.Rule.ID != "sonnet" || decision.FinalModel != "claude-sonnet-4-5" {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
}

func TestResolveModelPolicyStopsOnFallbackCycleAndHopLimit(t *testing.T) {
	calls := 0
	usage := fixedUsage(map[string]Matrix{"a": spent(1), "b": spent(1)}, &calls)
	layers := []ScopedRules{{Scope: RuleScopeAccount, Rules: []ModelRule{
		{ID: "ra", Models: []string{"a"}, Limits: spent(1), FallbackModel: "b"},
		{ID: "rb", Models: []string{"b"}, Limits: spent(1), FallbackModel: "a"},
	}, Usage: usage}}
	decision, err := ResolveModelPolicy("a", nil, layers)
	if err != nil || decision.Exceeded == nil || decision.Exceeded.Rule.ID != "rb" || len(decision.Fallbacks) != 1 {
		t.Fatalf("cycle decision = %+v, err = %v", decision, err)
	}

	chain := map[string]Matrix{}
	rules := []ModelRule{}
	names := []string{"m0", "m1", "m2", "m3", "m4", "m5"}
	for i, name := range names {
		chain[name] = spent(1)
		next := ""
		if i+1 < len(names) {
			next = names[i+1]
		}
		rules = append(rules, ModelRule{ID: name, Models: []string{name}, Limits: spent(1), FallbackModel: next})
	}
	decision, err = ResolveModelPolicy("m0", nil, []ScopedRules{{Scope: RuleScopeKey, Rules: rules, Usage: fixedUsage(chain, &calls)}})
	if err != nil || decision.Exceeded == nil || len(decision.Fallbacks) != MaxFallbackHops {
		t.Fatalf("hop decision = %+v, err = %v", decision, err)
	}
}

func TestResolveModelPolicyReportsKeyLayerFirstAndPropagatesUsageErrors(t *testing.T) {
	calls := 0
	opus := map[string]Matrix{"claude-opus-4-5": spent(10)}
	layers := []ScopedRules{
		{Scope: RuleScopeKey, Rules: []ModelRule{{ID: "key-opus", Models: []string{"claude-opus-*"}, Limits: spent(5)}}, Usage: fixedUsage(opus, &calls)},
		{Scope: RuleScopeAccount, Rules: []ModelRule{{ID: "acct-opus", Models: []string{"claude-opus-*"}, Limits: spent(8)}}, Usage: fixedUsage(opus, &calls)},
	}
	decision, err := ResolveModelPolicy("claude-opus-4-5", nil, layers)
	if err != nil || decision.Exceeded == nil || decision.Exceeded.Scope != RuleScopeKey {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}

	boom := errors.New("db down")
	layers = []ScopedRules{{Scope: RuleScopeAccount, Rules: []ModelRule{{ID: "x", Models: []string{"x"}, Limits: spent(1)}},
		Usage: func([]string) (Matrix, error) { return Matrix{}, boom }}}
	if _, err := ResolveModelPolicy("x", nil, layers); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want usage error", err)
	}
}

func TestNormalizeAccessPolicyFallbackModel(t *testing.T) {
	ok := AccessPolicy{ModelRules: []ModelRule{{Models: []string{"claude-opus-*"}, Limits: spent(1), FallbackModel: " claude-sonnet-4-5 "}}}
	out, err := NormalizeAccessPolicy(ok, AccountColumnLayout)
	if err != nil || out.ModelRules[0].FallbackModel != "claude-sonnet-4-5" {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	for _, fallback := range []string{"claude-opus-4-5", "claude-*"} {
		bad := AccessPolicy{ModelRules: []ModelRule{{Models: []string{"claude-opus-*"}, Limits: spent(1), FallbackModel: fallback}}}
		var verr *PolicyValidationError
		if _, err := NormalizeAccessPolicy(bad, AccountColumnLayout); !errors.As(err, &verr) || verr.Field != "model-rules[0].fallback-model" {
			t.Fatalf("fallback %q err = %v", fallback, err)
		}
	}
}
