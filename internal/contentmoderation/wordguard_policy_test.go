package contentmoderation

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func kw(pattern string) WordRule {
	return WordRule{Kind: WordRuleKeyword, Pattern: pattern, Enabled: true}
}
func rx(pattern string) WordRule {
	return WordRule{Kind: WordRuleRegex, Pattern: pattern, Enabled: true}
}

func tenantPolicy(rules ...WordRule) WordPolicy {
	return WordPolicy{ScopeType: WordScopeTenant, Enabled: true, Rules: rules}
}

func wordPolicyProblems(t *testing.T, p WordPolicy) []WordRuleProblem {
	t.Helper()
	err := p.Normalize()
	var policyErr *WordPolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("Normalize() = %v, want a *WordPolicyError", err)
	}
	return policyErr.Rules
}

func TestWordPolicyNormalizeAcceptsDefaults(t *testing.T) {
	p := tenantPolicy(kw(" forbidden "), rx(`\d{11}`))
	if err := p.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Rules[0].Pattern != "forbidden" {
		t.Fatalf("keyword not trimmed: %q", p.Rules[0].Pattern)
	}
	for _, rule := range p.Rules {
		if rule.ID == "" {
			t.Fatalf("rule %q got no id", rule.Pattern)
		}
	}
	if p.Rules[0].ID == p.Rules[1].ID {
		t.Fatalf("ids must be unique")
	}
	if p.Action != "" || p.ScanScope != "" || p.Replacement != "" {
		t.Fatalf("unset settings must stay unset so they inherit: %+v", p)
	}
}

func TestWordPolicyNormalizeKeepsGivenIDsAndFixesClashes(t *testing.T) {
	a, b := kw("one"), kw("two")
	a.ID, b.ID = "same", "same"
	p := tenantPolicy(a, b)
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.Rules[0].ID != "same" || p.Rules[1].ID == "same" || p.Rules[1].ID == "" {
		t.Fatalf("ids = %q, %q", p.Rules[0].ID, p.Rules[1].ID)
	}
}

func TestWordPolicyNormalizeSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*WordPolicy)
		field  string
	}{
		{"unknown action", func(p *WordPolicy) { p.Action = "drop" }, "action"},
		{"unknown scan scope", func(p *WordPolicy) { p.ScanScope = "everything" }, "scan_scope"},
		{"unknown scope type", func(p *WordPolicy) { p.ScopeType = "group" }, "scope_type"},
		{"tenant policy with a scope id", func(p *WordPolicy) { p.ScopeID = "k1" }, "scope_id"},
		{"key policy without a key", func(p *WordPolicy) { p.ScopeType = WordScopeKey }, "scope_id"},
		{"control character in the replacement", func(p *WordPolicy) { p.Replacement = "a\nb" }, "replacement"},
		{"replacement too long", func(p *WordPolicy) { p.Replacement = strings.Repeat("x", MaxWordReplacementRunes+1) }, "replacement"},
		{"invalid utf-8 replacement", func(p *WordPolicy) { p.Replacement = "\xff" }, "replacement"},
		{"block message too long", func(p *WordPolicy) { p.BlockMessage = strings.Repeat("x", MaxWordBlockMessageRunes+1) }, "block_message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tenantPolicy(kw("word"))
			tt.mutate(&p)
			err := p.Normalize()
			var policyErr *WordPolicyError
			if !errors.As(err, &policyErr) || policyErr.Field != tt.field {
				t.Fatalf("Normalize() = %v, want a problem with %s", err, tt.field)
			}
		})
	}
	// A key policy and the multi-line block message are fine.
	p := WordPolicy{ScopeType: WordScopeKey, ScopeID: "k1", Enabled: true, BlockMessage: "line one\nline two", Action: WordActionReplace, Replacement: "[removed]"}
	if err := p.Normalize(); err != nil {
		t.Fatalf("valid key policy refused: %v", err)
	}
}

func TestWordPolicyNormalizeNamesEveryBadRule(t *testing.T) {
	p := tenantPolicy(
		kw("fine"),
		kw("   "),
		kw("\u200b\u200d"),
		kw(strings.Repeat("长", MaxWordKeywordRunes+1)),
		rx(`(unclosed`),
		rx(`a*`),
		rx(`fine\d+`),
		WordRule{Kind: "glob", Pattern: "*.exe", Enabled: true},
	)
	problems := wordPolicyProblems(t, p)
	got := map[int]string{}
	for _, problem := range problems {
		got[problem.Index] = problem.Code
	}
	want := map[int]string{1: "empty", 2: "empty", 3: "too_long", 4: "invalid_regex", 5: "matches_empty", 7: "unknown_kind"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("problems = %v, want %v", got, want)
	}
	for _, problem := range problems {
		if problem.Message == "" || problem.Pattern == "" && problem.Index != 1 {
			t.Fatalf("problem lacks detail: %+v", problem)
		}
	}
	// The message of a regex problem must not leak the internal (?i) prefix.
	for _, problem := range problems {
		if strings.Contains(problem.Message, "(?i)") {
			t.Fatalf("message mentions the internal flag: %q", problem.Message)
		}
	}
}

func TestWordPolicyRegexMustNotMatchEmpty(t *testing.T) {
	empty := []string{`a*`, `^$`, `\b`, `\B`, `(foo)?`, `x{0}`, `(?:a|b*)`, `^`, `$`, `(?m)^`, `\b|a`, `(a|)`, `[a-z]{0,3}`, `(?:)`, `()`}
	for _, pattern := range empty {
		p := tenantPolicy(rx(pattern))
		problems := wordPolicyProblems(t, p)
		if len(problems) != 1 || problems[0].Code != "matches_empty" {
			t.Errorf("%q: problems = %+v, want matches_empty", pattern, problems)
		}
	}
	fine := []string{`a+`, `\d{11}`, `foo|bar`, `\bfoo\b`, `^foo$`, `(?:ab){2,}`, `[a-z]{1,3}`, `.`, `x{1}`, `(?i)abc`}
	for _, pattern := range fine {
		p := tenantPolicy(rx(pattern))
		if err := p.Normalize(); err != nil {
			t.Errorf("%q refused: %v", pattern, err)
		}
	}
}

// A pattern must not be able to close the group the case-insensitive flag is
// written in and add an alternative of its own.
func TestWordPolicyRegexCannotEscapeItsFlags(t *testing.T) {
	p := tenantPolicy(rx(`a)|(b`))
	if problems := wordPolicyProblems(t, p); len(problems) != 1 || problems[0].Code != "invalid_regex" {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestWordPolicyRegexLimits(t *testing.T) {
	p := tenantPolicy(rx(strings.Repeat("a", MaxWordRegexBytes+1)))
	if problems := wordPolicyProblems(t, p); problems[0].Code != "too_long" {
		t.Fatalf("problems = %+v", problems)
	}
	var rules []WordRule
	for i := 0; i <= MaxWordRegexPerPolicy; i++ {
		rules = append(rules, rx(fmt.Sprintf("word%d+", i)))
	}
	problems := wordPolicyProblems(t, tenantPolicy(rules...))
	if len(problems) != 1 || problems[0].Code != "too_many_regex" || problems[0].Index != MaxWordRegexPerPolicy {
		t.Fatalf("problems = %+v", problems)
	}
	rules = rules[:MaxWordRegexPerPolicy]
	if err := (&WordPolicy{ScopeType: WordScopeTenant, Rules: rules}).Normalize(); err != nil {
		t.Fatalf("exactly the limit must be accepted: %v", err)
	}
}

func TestWordPolicyRuleCountLimit(t *testing.T) {
	rules := make([]WordRule, 0, MaxWordRulesPerPolicy+1)
	for i := 0; i <= MaxWordRulesPerPolicy; i++ {
		rules = append(rules, kw(fmt.Sprintf("word-%d", i)))
	}
	problems := wordPolicyProblems(t, tenantPolicy(rules...))
	if len(problems) != 1 || problems[0].Code != "too_many_rules" {
		t.Fatalf("problems = %+v", problems)
	}
	p := tenantPolicy(rules[:MaxWordRulesPerPolicy]...)
	if err := p.Normalize(); err != nil {
		t.Fatalf("exactly the limit must be accepted: %v", err)
	}
}

func TestWordPolicyDeduplicatesByEffect(t *testing.T) {
	// Case, width and zero-width characters do not make a keyword a different one.
	p := tenantPolicy(kw("Bad"), kw("ＢＡＤ"), kw("b\u200bad"), kw("bad"), rx(`x+`), rx(`x+`), rx(`X+`))
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != 3 {
		t.Fatalf("rules = %+v, want the keyword once and the two distinct expressions", p.Rules)
	}
	if p.Rules[0].Pattern != "Bad" {
		t.Fatalf("the first spelling must be the one kept: %+v", p.Rules[0])
	}
}

func TestWordPolicyRegexStripsOnlyLineBreaks(t *testing.T) {
	p := tenantPolicy(rx("foo \r\n"))
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.Rules[0].Pattern != "foo " {
		t.Fatalf("pattern = %q: trailing spaces belong to the expression", p.Rules[0].Pattern)
	}
	if problems := wordPolicyProblems(t, tenantPolicy(rx("a\nb"))); problems[0].Code != "control_characters" {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestWordPolicyKindDefaultsToKeyword(t *testing.T) {
	p := tenantPolicy(WordRule{Pattern: "word", Enabled: true})
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.Rules[0].Kind != WordRuleKeyword {
		t.Fatalf("kind = %q", p.Rules[0].Kind)
	}
}

func TestWordPolicyErrorMentionsTheRule(t *testing.T) {
	policy := tenantPolicy(kw("ok"), rx("("))
	err := policy.Normalize()
	if err == nil || !strings.Contains(err.Error(), "rule 2") {
		t.Fatalf("error = %v, want it to name rule 2", err)
	}
}
