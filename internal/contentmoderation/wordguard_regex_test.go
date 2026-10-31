package contentmoderation

import (
	"regexp/syntax"
	"strings"
	"testing"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// The expressions a review found to cost a request seconds: a short source whose
// compiled program is thousands of instructions, run over every byte of text.
// The standard library's parser already refuses the nested counted repetitions
// whose product is over a thousand; the rest are this package's to refuse.
var heavyRegexes = []struct{ pattern, code string }{
	{`(?:a?){1000}a{1000}`, "regex_too_complex"},
	{`(?:a?){500}a{500}`, "regex_too_complex"},
	{`\pL{1000}`, "regex_too_complex"},
	{`(?:\w+\s*){600}x`, "regex_too_complex"},
	{`(?:ab|cd|ef|gh){120}z`, "regex_too_complex"},
	{`[a-z]{1,300}[0-9]{1,300}`, "regex_too_complex"},
	{`(?:a{1000}){100}`, "invalid_regex"},
	{`(?:a{1000}){1000}`, "invalid_regex"},
	{`(((a{1000}){1000}){1000}){1000}`, "invalid_regex"},
}

func TestWordRegexTooComplexIsRefusedAtSave(t *testing.T) {
	for _, heavy := range heavyRegexes {
		p := tenantPolicy(rx(heavy.pattern))
		problems := wordPolicyProblems(t, p)
		if len(problems) != 1 || problems[0].Code != heavy.code {
			t.Errorf("%s: problems = %+v, want one %s", heavy.pattern, problems, heavy.code)
			continue
		}
		if heavy.code == "regex_too_complex" && !strings.Contains(problems[0].Message, "500") {
			t.Errorf("%s: the message does not name the limit: %q", heavy.pattern, problems[0].Message)
		}
	}
}

func TestWordRegexTooComplexIsRefusedQuickly(t *testing.T) {
	// A source of a few bytes must not be expanded to find out it is too big.
	started := time.Now()
	for _, heavy := range heavyRegexes {
		compileWordRegex(heavy.pattern)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("refusing %d heavy expressions took %v", len(heavyRegexes), elapsed)
	}
}

func TestWordRegexOrdinaryExpressionsAreAccepted(t *testing.T) {
	for _, pattern := range []string{
		`1[3-9]\d{9}`,
		`\d{17}[\dXx]`,
		`[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`,
		`(?:\d{1,3}\.){3}\d{1,3}`,
		`sk-[A-Za-z0-9]{20,48}`,
		`(?:foo|bar|baz)\s*=\s*\S+`,
	} {
		compiled, code, message := compileWordRegex(pattern)
		if code != "" {
			t.Errorf("%s refused: %s %s", pattern, code, message)
			continue
		}
		if compiled.program <= 0 || compiled.program > MaxWordRegexProgram {
			t.Errorf("%s: program = %d", pattern, compiled.program)
		}
	}
}

func TestWordRegexLimitIsTheCompiledProgram(t *testing.T) {
	// `a{n}` compiles to n instructions plus the two every program has, so the
	// largest repetition that is accepted is known exactly.
	if _, code, _ := compileWordRegex(`a{498}`); code != "" {
		t.Fatalf("a{498} refused: %s", code)
	}
	if _, code, _ := compileWordRegex(`a{499}`); code != "regex_too_complex" {
		t.Fatalf("a{499}: code = %q, want regex_too_complex", code)
	}
}

func TestWordRegexProgramEstimateTracksTheCompiler(t *testing.T) {
	// The estimate is what keeps a pattern from being expanded before it is
	// judged, so it has to be close to the real program wherever the real one
	// can be built.
	for _, pattern := range []string{
		`a`, `abc`, `a|b|c`, `a*`, `a+`, `a?`, `(a)`, `(?:ab)+c`, `a{3}`, `a{3,}`, `a{3,7}`, `a{0,5}`,
		`(a|bc){2,4}`, `[a-z]{10}`, `\d{3}-\d{4}`, `(?:a{10}){5}`, `x(?:y|z){3}w`, `.{20}`, `(?s).{20}`,
		`\bword\b`, `^start`, `end$`, `(?:\w+\s*){5}`, `\pL{50}`, `(a?){30}a{30}`, `1[3-9]\d{9}`,
	} {
		parsed, err := syntax.Parse("(?i)"+pattern, syntax.Perl)
		if err != nil {
			t.Fatalf("%s: %v", pattern, err)
		}
		prog, err := syntax.Compile(parsed.Simplify())
		if err != nil {
			t.Fatalf("%s: %v", pattern, err)
		}
		estimate, real := estimateRegexProgram(parsed), len(prog.Inst)
		if estimate > real || real-estimate > 3 {
			t.Errorf("%s: estimate %d, program %d", pattern, estimate, real)
		}
	}
}

func TestWordRegexEstimateSaturates(t *testing.T) {
	// The parser caps nested repetitions, but the arithmetic must not depend on
	// it: build the tree by hand, five levels of {1000} deep.
	tree := &syntax.Regexp{Op: syntax.OpLiteral, Rune: []rune{'a'}}
	for i := 0; i < 5; i++ {
		tree = &syntax.Regexp{Op: syntax.OpRepeat, Min: 1000, Max: 1000, Sub: []*syntax.Regexp{tree}}
	}
	if estimate := estimateRegexProgram(tree); estimate < regexEstimateLimit || estimate < 0 {
		t.Fatalf("estimate = %d, want it past the limit without overflowing", estimate)
	}
	if got := maxRegexLen(tree); got < 0 {
		t.Fatalf("maxLen = %d: five nested {1000} are bounded, and must not wrap around", got)
	}
}

func TestWordPolicyRegexProgramTotal(t *testing.T) {
	// Each of these compiles to about 400 instructions; the policy holds 2000.
	var rules []WordRule
	for _, letter := range "abcdefg" {
		rules = append(rules, rx(strings.Repeat(string(letter), 1)+`{400}`))
	}
	problems := wordPolicyProblems(t, tenantPolicy(rules...))
	if len(problems) != 3 {
		t.Fatalf("problems = %+v, want the last three of seven refused", problems)
	}
	for i, problem := range problems {
		if problem.Code != "regex_too_complex" || problem.Index != 4+i {
			t.Fatalf("problem %d = %+v", i, problem)
		}
		if !strings.Contains(problem.Message, "together") || !strings.Contains(problem.Message, "2000") {
			t.Fatalf("the message does not say the policy as a whole is the problem: %q", problem.Message)
		}
	}
	// A repeated expression is the same rule once, and costs once.
	repeated := tenantPolicy(rx(`a{400}`), rx(`a{400}`), rx(`b{400}`), rx(`c{400}`), rx(`d{400}`))
	if err := repeated.Normalize(); err != nil {
		t.Fatalf("a duplicate was counted twice: %v", err)
	}
}

func TestWordPolicyDoesNotCompileMoreExpressionsThanItHolds(t *testing.T) {
	rules := make([]WordRule, 0, 5000)
	for i := 0; i < 5000; i++ {
		rules = append(rules, rx(`x{3}`+strings.Repeat("y", i%7+1)))
	}
	started := time.Now()
	problems := wordPolicyProblems(t, tenantPolicy(rules...))
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("checking 5000 expressions took %v", elapsed)
	}
	if len(problems) == 0 || problems[len(problems)-1].Code != "too_many_regex" {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestWordRuleIDIsBounded(t *testing.T) {
	rule := kw("bad")
	rule.ID = strings.Repeat("x", MaxWordRuleIDBytes)
	exact := tenantPolicy(rule)
	if err := exact.Normalize(); err != nil {
		t.Fatalf("an id of exactly the limit was refused: %v", err)
	}
	rule.ID += "x"
	problems := wordPolicyProblems(t, tenantPolicy(rule))
	if len(problems) != 1 || problems[0].Code != "too_long" {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestWordRegexBudget(t *testing.T) {
	budget := &regexBudget{}
	budget.left.Store(100)
	if !budget.take(60) || budget.wasExceeded() {
		t.Fatal("60 of 100 should be available")
	}
	if budget.take(60) {
		t.Fatal("60 more must not fit")
	}
	if !budget.wasExceeded() {
		t.Fatal("a refused take must be remembered")
	}
	if !budget.take(40) {
		t.Fatal("a refused take must not spend anything: 40 are left")
	}
	if budget.take(1) {
		t.Fatal("the budget is spent")
	}
	var unlimited *regexBudget
	if !unlimited.take(1<<62) || unlimited.wasExceeded() {
		t.Fatal("a nil budget has no limit")
	}
}

// a text no expression can finish quickly: the NFA keeps every thread alive.
func heavyText(n int) string { return strings.Repeat("a", n) }

func TestWordRegexOverTheBudgetFailsOpenAndSaysSo(t *testing.T) {
	// Allowed at save (about 480 instructions), and 480 steps a byte over a
	// megabyte is far more than a request may spend.
	const pattern = `(?:a?){160}a{160}b`
	contexts := buildWordContexts(nil, tenantOf(t, WordActionLog, rx(pattern), kw("needle")), nil)
	body := `{"messages":[{"role":"user","content":"` + heavyText(1<<20) + ` needle"}]}`

	started := time.Now()
	verdict := evaluateWords(sdktranslator.FormatOpenAI, []byte(body), contexts, wordEvalOptions{})
	elapsed := time.Since(started)

	if !verdict.RegexSkipped {
		t.Fatal("the verdict does not say the expression was skipped")
	}
	if verdict.Blocked || verdict.MatchCount != 1 || verdict.Matches[0].Kind != WordRuleKeyword {
		t.Fatalf("the keyword must still be checked: %+v", verdict)
	}
	// Running it takes seconds on the slowest machine this was measured on.
	if elapsed > 750*time.Millisecond {
		t.Fatalf("the evaluation took %v: the expression was run", elapsed)
	}
}

func TestWordRegexWithinTheBudgetStillMatches(t *testing.T) {
	contexts := buildWordContexts(nil, tenantOf(t, WordActionLog, rx(`1[3-9]\d{9}`)), nil)
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("hello world. ", 20000) + ` call 13800138000"}]}`
	verdict := evaluateWords(sdktranslator.FormatOpenAI, []byte(body), contexts, wordEvalOptions{})
	if verdict.RegexSkipped || verdict.MatchCount != 1 {
		t.Fatalf("an ordinary expression on a long text must run: %+v", verdict)
	}
}

func TestWordRegexBudgetIsPerRequestNotPerText(t *testing.T) {
	// Many texts that each fit, together too much: the budget is spent across
	// them and the later ones are not scanned by the expression.
	contexts := buildWordContexts(nil, tenantOf(t, WordActionLog, rx(`(?:a?){100}a{100}b`)), nil)
	var messages []string
	for i := 0; i < 20; i++ {
		messages = append(messages, `{"role":"user","content":"`+heavyText(2_000)+`"}`)
	}
	body := []byte(`{"messages":[` + strings.Join(messages, ",") + `]}`)

	// 302 steps a byte: a scan of one text is about 600 thousand steps.
	enough := &regexBudget{}
	enough.left.Store(20 * 700_000)
	if verdict := evaluateWords(sdktranslator.FormatOpenAI, body, contexts, wordEvalOptions{budget: enough}); verdict.RegexSkipped {
		t.Fatal("twenty texts that fit must all be scanned")
	}
	short := &regexBudget{}
	short.left.Store(1_500_000)
	if verdict := evaluateWords(sdktranslator.FormatOpenAI, body, contexts, wordEvalOptions{budget: short}); !verdict.RegexSkipped {
		t.Fatal("twenty texts do not fit in the budget of two")
	}
}

func TestWordRegexBudgetIsSharedByTheTwoBodiesOfOneRequest(t *testing.T) {
	budget := &regexBudget{}
	// One scan of the text below costs 403 * 600 = 241,800 steps.
	budget.left.Store(300_000)
	contexts := buildWordContexts(nil, tenantOf(t, WordActionLog, rx(`a{400}b`)), nil)
	body := []byte(`{"messages":[{"role":"user","content":"` + heavyText(600) + `"}]}`)
	first := evaluateWords(sdktranslator.FormatOpenAI, body, contexts, wordEvalOptions{budget: budget})
	second := evaluateWords(sdktranslator.FormatOpenAI, body, contexts, wordEvalOptions{budget: budget})
	if first.RegexSkipped {
		t.Fatal("the first scan fits")
	}
	if !second.RegexSkipped {
		t.Fatal("the second scan draws on what the first left")
	}
}

func TestWordRegexRewriteDoesNotScanAgain(t *testing.T) {
	// A replacement is located from the hits the first scan found: a request the
	// budget allowed to be scanned once is not stopped half-way through its
	// rewrite for a second scan.
	contexts := buildWordContexts(nil, tenantOf(t, WordActionReplace, rx(`\d{11}`)), nil)
	budget := &regexBudget{}
	budget.left.Store(12 * 200) // the first scan of a 200 byte text, and nothing more
	body := `{"messages":[{"role":"user","content":"call 13800138000 ` + strings.Repeat("x", 150) + `"}]}`
	verdict := evaluateWords(sdktranslator.FormatOpenAI, []byte(body), contexts, wordEvalOptions{budget: budget})
	if verdict.Blocked || verdict.Replaced != 1 || verdict.RegexSkipped {
		t.Fatalf("verdict = %+v", verdict)
	}
	if strings.Contains(string(verdict.Payload), "13800138000") {
		t.Fatalf("the number survived: %s", verdict.Payload)
	}
}

func TestWordRegexReplyBudgetIsPerReply(t *testing.T) {
	engine, snapshot := replyEngineOf(t, WordActionReplace, rx(`(?:a?){160}a{160}b`))
	engine.budget = &regexBudget{}
	engine.budget.left.Store(int64(MaxWordRegexProgram) * 5000)
	// Five thousand bytes fit; the pieces after that are not scanned by the
	// expression, however small each one is.
	for i := 0; i < 20; i++ {
		engine.filterText(heavyText(1000), true)
	}
	if !engine.budget.wasExceeded() {
		t.Fatal("the reply's pieces were not charged to one budget")
	}
	view := snapshot.view.Reply
	if view == nil || view.Skipped != ReplySkipRegexBudget {
		t.Fatalf("the request log must say the reply was only partly checked: %+v", view)
	}
}

func TestWordRegexLogLimitStopsAtTheFirstMatches(t *testing.T) {
	// A text made of nothing but matches is not collected whole when the action
	// only needs the first.
	contexts := buildWordContexts(nil, tenantOf(t, "", rx(`a`)), nil)
	body := `{"messages":[{"role":"user","content":"` + heavyText(1<<20) + `"}]}`
	verdict := evaluateWords(sdktranslator.FormatOpenAI, []byte(body), contexts, wordEvalOptions{})
	if !verdict.Blocked || verdict.MatchCount != 1 {
		t.Fatalf("verdict = %+v", verdict)
	}
}

func TestWordRegexAnalysis(t *testing.T) {
	tests := []struct {
		pattern string
		maxLen  int
		first   string // characters a match can begin with; empty means any
		notIn   string // characters it cannot
	}{
		{`\d{11}`, 11, "0123456789", "a "},
		{`1[3-9]\d{9}`, 11, "1", "2a"},
		{`ab|cde`, 3, "aAcC", "bdx"},
		{`a+`, -1, "aA", "b"},
		{`x?yz`, 3, "xXyY", "z"},
		{`(?:foo|bar)\d{2,4}`, 7, "fFbB", "o0"},
		{`\bword\b`, 4, "wW", "o"},
		{`[a-c]{2}d*`, -1, "abcABC", "dD"},
		{`.{3}x`, 4, "", ""},
	}
	for _, tc := range tests {
		compiled, code, message := compileWordRegex(tc.pattern)
		if code != "" {
			t.Fatalf("%s: %s %s", tc.pattern, code, message)
		}
		if compiled.maxLen != tc.maxLen {
			t.Errorf("%s: maxLen = %d, want %d", tc.pattern, compiled.maxLen, tc.maxLen)
		}
		for _, r := range tc.first {
			if !compiled.first.contains(r) {
				t.Errorf("%s: %q should begin a match", tc.pattern, r)
			}
		}
		for _, r := range tc.notIn {
			if compiled.first.contains(r) {
				t.Errorf("%s: %q cannot begin a match", tc.pattern, r)
			}
		}
		if tc.first == "" && !compiled.first.any {
			t.Errorf("%s: a match can begin with any character", tc.pattern)
		}
	}
}
