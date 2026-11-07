package contentmoderation

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// looseKw is a keyword rule that ignores the separators between its characters.
func looseKw(pattern string) WordRule {
	rule := kw(pattern)
	rule.IgnoreSeparators = true
	return rule
}

func TestIsWordSeparator(t *testing.T) {
	for _, r := range []rune{' ', '\t', '\n', ' ', '　', '-', '_', '.', ',', '*', '/', '\\', '~', '。', '，', '、', '·', '“', '+', '=', '$', '©', '😀', '\u0001', '\u007f'} {
		if !isWordSeparator(r) {
			t.Errorf("%q should be skipped between characters", r)
		}
	}
	for _, r := range []rune{'a', 'Z', '0', '9', '敏', '感', 'é', 'я', 'ا', 'ก', 'ß', '́', '1'} {
		if isWordSeparator(r) {
			t.Errorf("%q must never be skipped", r)
		}
	}
}

func TestStripSeparators(t *testing.T) {
	tests := []struct {
		name, text, want string
		runs             separatorRuns
	}{
		{"nothing to take out", "敏感词abc", "敏感词abc", nil},
		{"empty", "", "", nil},
		{"spaces between", "敏 感 词", "敏感词", separatorRuns{{3, 1}, {6, 2}}},
		{"leading and trailing", " 敏感 ", "敏感", separatorRuns{{0, 1}, {6, 2}}},
		{"a run of mixed separators", "a-._ b", "ab", separatorRuns{{1, 4}}},
		{"only separators", " -. ", "", separatorRuns{{0, 4}}},
		{"multi-byte separators", "敏，感。词", "敏感词", separatorRuns{{3, 3}, {6, 6}}},
		{"an invalid byte is kept", "a\xffb c", "a\xffbc", separatorRuns{{3, 1}}},
		{"the replacement character is a symbol", "a�b", "ab", separatorRuns{{1, 3}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, runs := stripSeparators(tt.text)
			if got != tt.want || fmt.Sprint(runs) != fmt.Sprint(tt.runs) {
				t.Fatalf("stripSeparators(%q) = %q, %v; want %q, %v", tt.text, got, runs, tt.want, tt.runs)
			}
		})
	}
}

func TestSeparatorRunsMapOffsetsBack(t *testing.T) {
	// "敏 感-词": the stripped text is 敏感词, nine bytes.
	text := "敏 感-词"
	stripped, runs := stripSeparators(text)
	if stripped != "敏感词" {
		t.Fatalf("stripped = %q", stripped)
	}
	for _, tt := range []struct {
		offset  int
		isStart bool
		want    int
	}{
		{0, true, 0},  // 敏 starts the text
		{3, true, 4},  // 感 starts after the space
		{6, true, 8},  // 词 starts after the hyphen
		{3, false, 3}, // a match that ends after 敏 ends before the space
		{6, false, 7}, // ... and one that ends after 感 before the hyphen
		{9, false, 11},
	} {
		if got := runs.original(tt.offset, tt.isStart); got != tt.want {
			t.Errorf("original(%d, start=%v) = %d, want %d", tt.offset, tt.isStart, got, tt.want)
		}
	}
	if (separatorRuns)(nil).original(7, true) != 7 || (separatorRuns)(nil).original(7, false) != 7 {
		t.Error("a text without separators maps to itself")
	}
}

// Taking a span of the stripped text back into the text it came from gives a
// span that starts and ends with a character, and that has exactly that span in
// it once its separators are taken out again.
func TestSeparatorRunsMapEverySpanBack(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 8))
	alphabet := []string{"a", "b", "敏", "感", " ", "  ", "-", ".", "，", "😀", " ", "1", "é", "_"}
	for trial := 0; trial < 500; trial++ {
		var sb strings.Builder
		for n := 1 + rng.IntN(30); n > 0; n-- {
			sb.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		text := sb.String()
		stripped, runs := stripSeparators(text)
		var starts []int
		for i := range stripped {
			starts = append(starts, i)
		}
		starts = append(starts, len(stripped))
		for _, p := range starts {
			for _, e := range starts {
				if e <= p {
					continue
				}
				from, to := runs.original(p, true), runs.original(e, false)
				if from < 0 || to > len(text) || from >= to {
					t.Fatalf("text %q: stripped[%d:%d] mapped to [%d:%d]", text, p, e, from, to)
				}
				span := text[from:to]
				first, _ := utf8.DecodeRuneInString(span)
				last, _ := utf8.DecodeLastRuneInString(span)
				if isWordSeparator(first) || isWordSeparator(last) {
					t.Fatalf("text %q: span %q of stripped[%d:%d] starts or ends with a separator", text, span, p, e)
				}
				if again, _ := stripSeparators(span); again != stripped[p:e] {
					t.Fatalf("text %q: span %q has %q in it, want %q", text, span, again, stripped[p:e])
				}
			}
		}
	}
}

func looseHits(t *testing.T, fold bool, text string, rules ...WordRule) []string {
	t.Helper()
	m := matcherFor(t, fold, rules...)
	normalized := normalizeForMatch(text, fold)
	var got []string
	for _, hit := range m.find(normalized, findOptions{}).hits {
		got = append(got, normalized[hit.start:hit.end])
	}
	return got
}

func TestKeywordThatIgnoresSeparatorsMatchesAcrossThem(t *testing.T) {
	rule := looseKw("敏感词")
	for text, want := range map[string]string{
		"敏感词":         "敏感词",
		"敏 感 词":       "敏 感 词",
		"敏-感-词":       "敏-感-词",
		"敏。感，词":       "敏。感,词", // the full-width comma is a plain one by then
		"敏\n感\t词":     "敏 感 词", // white space is one space by then
		"敏😀感😀词":       "敏😀感😀词",
		"敏***感___词":   "敏***感___词",
		"请勿 敏 感 词 出现": "敏 感 词",
		"敏感 词 敏感词":    "敏感 词",
	} {
		got := looseHits(t, false, text, rule)
		if len(got) == 0 || got[0] != want {
			t.Errorf("%q: hits = %q, want first %q", text, got, want)
		}
	}
	for _, text := range []string{"敏a感词", "敏1感词", "敏感个词", "敏", "感词", "敏 感", "词感敏"} {
		if got := looseHits(t, false, text, rule); len(got) != 0 {
			t.Errorf("%q must not match, hits = %q", text, got)
		}
	}
}

func TestKeywordThatDoesNotIgnoreSeparatorsStaysStrict(t *testing.T) {
	for _, text := range []string{"敏 感 词", "敏-感-词", "敏。感，词"} {
		if got := looseHits(t, false, text, kw("敏感词")); len(got) != 0 {
			t.Errorf("a plain keyword matched %q: %q", text, got)
		}
	}
	if got := looseHits(t, false, "敏感词", kw("敏感词")); len(got) != 1 {
		t.Fatalf("a plain keyword stopped matching: %q", got)
	}
}

func TestKeywordThatIgnoresSeparatorsMayHaveSomeOfItsOwn(t *testing.T) {
	// Written with separators it is the same keyword.
	for _, rule := range []WordRule{looseKw("a-b"), looseKw("a b"), looseKw("ab"), looseKw(" a.b ")} {
		for _, text := range []string{"ab", "a b", "a.b", "a - b"} {
			if got := looseHits(t, false, text, rule); len(got) != 1 {
				t.Errorf("keyword %q on %q: hits = %q", rule.Pattern, text, got)
			}
		}
	}
	// Words are not told apart, which is what ignoring the spaces means and why
	// the option is a choice made rule by rule.
	if got := looseHits(t, false, "a pic at the gate", looseKw("cat")); len(got) != 1 || got[0] != "c at" {
		t.Fatalf("hits = %q", got)
	}
}

func TestKeywordThatIgnoresSeparatorsFollowsTheFoldSetting(t *testing.T) {
	if got := looseHits(t, true, "繁 體", looseKw("繁体")); len(got) != 1 {
		t.Fatalf("a traditional spelling with a space was not found: %q", got)
	}
	if got := looseHits(t, false, "繁 體", looseKw("繁体")); len(got) != 0 {
		t.Fatalf("without folding the two spellings differ: %q", got)
	}
	if got := looseHits(t, false, "ＡＢ ＣＤ", looseKw("abcd")); len(got) != 1 {
		t.Fatalf("width and case still fold: %q", got)
	}
}

func TestKeywordsThatIgnoreSeparatorsMergeWithTheOtherHits(t *testing.T) {
	// A plain keyword inside what a loose one matches, and one that overlaps its end.
	if got := looseHits(t, false, "x 敏 感词 y", looseKw("敏感词"), kw("感词")); len(got) != 1 || got[0] != "敏 感词" {
		t.Fatalf("hits = %q, want the union", got)
	}
	if got := looseHits(t, false, "敏 感词条", looseKw("敏感词"), kw("词条")); len(got) != 1 || got[0] != "敏 感词条" {
		t.Fatalf("hits = %q, want the union", got)
	}
	if got := looseHits(t, false, "敏 感词 和 foo", looseKw("敏感词"), kw("foo")); len(got) != 2 || got[0] != "敏 感词" || got[1] != "foo" {
		t.Fatalf("hits = %q", got)
	}
}

// The hit belongs to the rule that matched, whether or not it was a loose one.
func TestKeywordsThatIgnoreSeparatorsAreAttributedToTheirRule(t *testing.T) {
	loose := looseKw("敏感词")
	loose.ID = "loose-1"
	strict := kw("other")
	strict.ID = "strict-1"
	m := matcherFor(t, false, strict, loose)
	hits := m.find("x 敏 感 词 other", findOptions{}).hits
	if len(hits) != 2 || m.rules[hits[0].rule].ID != "loose-1" || m.rules[hits[1].rule].ID != "strict-1" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestKeywordThatIgnoresSeparatorsHoldsBackAMatchThatMayContinue(t *testing.T) {
	m := matcherFor(t, false, looseKw("敏感词"))
	for _, tt := range []struct {
		text      string
		undecided int
	}{
		{"x 敏", 2},
		{"x 敏 ", 2},
		{"x 敏 - 感 ", 2},
		{"ab 敏感", 3},
		{"x 好", -1},
		{"x ", -1},
	} {
		if got := m.find(tt.text, findOptions{more: true}).undecided; got != tt.undecided {
			t.Errorf("%q: undecided = %d, want %d", tt.text, got, tt.undecided)
		}
	}
	// A finished match that could still be a longer keyword is held back too.
	longer := matcherFor(t, false, looseKw("敏感"), looseKw("敏感词"))
	if found := longer.find("x 敏 感", findOptions{more: true}); found.undecided != 2 || len(found.hits) != 0 {
		t.Fatalf("found = %+v: a match that may grow is not final", found)
	}
	if found := longer.find("x 敏 感", findOptions{}); len(found.hits) != 1 {
		t.Fatalf("found = %+v: at the end of the text it is final", found)
	}
}

func TestKeywordThatIgnoresSeparatorsAndTheHitLimit(t *testing.T) {
	m := matcherFor(t, false, looseKw("敏感词"), kw("zzz"))
	text := strings.Repeat("敏 感 词 ", 50) + "zzz"
	if got := m.find(text, findOptions{limit: 1}).hits; len(got) != 1 {
		t.Fatalf("hits = %d, want the limit", len(got))
	}
	if got := m.find(text, findOptions{}).hits; len(got) != 51 {
		t.Fatalf("hits = %d, want all of them", len(got))
	}
}

// Whatever the text, the hits are what a plain reading of the rule gives: from a
// character that matches the keyword's first, skipping separators between the
// characters, to the one that matches its last, longest keyword first.
func TestKeywordsThatIgnoreSeparatorsAgreeWithAPlainReading(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	alphabet := []string{"a", "b", "c", "敏", "感", "词", " ", "-", ".", "，", "x"}
	words := []string{"ab", "abc", "bca", "敏感", "敏感词", "cab", "a"}
	for trial := 0; trial < 600; trial++ {
		var rules []WordRule
		var chosen []string
		for _, i := range rng.Perm(len(words))[:1+rng.IntN(4)] {
			rules = append(rules, looseKw(words[i]))
			chosen = append(chosen, words[i])
		}
		var sb strings.Builder
		for n := 1 + rng.IntN(25); n > 0; n-- {
			sb.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		text := normalizeForMatch(sb.String(), false)

		got := matcherFor(t, false, rules...).find(text, findOptions{}).hits
		want := plainLooseHits(text, chosen)
		if len(got) != len(want) {
			t.Fatalf("rules %q text %q: hits %v, want %v", chosen, text, got, want)
		}
		for i := range got {
			if got[i].start != want[i][0] || got[i].end != want[i][1] {
				t.Fatalf("rules %q text %q: hit %d is [%d,%d), want [%d,%d)", chosen, text, i, got[i].start, got[i].end, want[i][0], want[i][1])
			}
		}
	}
}

// plainLooseHits is the slow reading of keywords that ignore separators: try
// every keyword at every character, longest first, and go on after the match.
func plainLooseHits(text string, keywords []string) [][2]int {
	var hits [][2]int
	for pos := 0; pos < len(text); {
		r, size := utf8.DecodeRuneInString(text[pos:])
		if isWordSeparator(r) {
			pos += size
			continue
		}
		best := -1
		for _, word := range keywords {
			want := []rune(word)
			i, ok := pos, true
			for k, wr := range want {
				if k > 0 {
					for i < len(text) {
						r, size := utf8.DecodeRuneInString(text[i:])
						if !isWordSeparator(r) {
							break
						}
						i += size
					}
				}
				if i >= len(text) {
					ok = false
					break
				}
				r, size := utf8.DecodeRuneInString(text[i:])
				if r != wr {
					ok = false
					break
				}
				i += size
			}
			if ok && i > best {
				best = i
			}
		}
		if best >= 0 {
			hits = append(hits, [2]int{pos, best})
			pos = best
			continue
		}
		pos += size
	}
	return hits
}

func TestKeywordThatIgnoresSeparatorsIsReplacedWhole(t *testing.T) {
	contexts := buildWordContexts(nil, tenantOf(t, WordActionReplace, looseKw("敏感词")), nil)
	payload := `{"model":"m","messages":[{"role":"user","content":"请勿 敏 感，词 出现，也不要 敏感词。"}]}`
	verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, contexts)
	if verdict.Action != WordVerdictReplace || verdict.Replaced != 2 {
		t.Fatalf("verdict = %+v", verdict)
	}
	want := `{"model":"m","messages":[{"role":"user","content":"请勿 *** 出现，也不要 ***。"}]}`
	if string(verdict.Payload) != want {
		t.Fatalf("payload = %s\nwant      %s", verdict.Payload, want)
	}
}

func TestKeywordThatIgnoresSeparatorsIsLocatedInTheTextAsWritten(t *testing.T) {
	contexts := buildWordContexts(nil, tenantOf(t, WordActionLog, looseKw("敏感词"), looseKw("a-b")), nil)
	for _, tt := range []struct {
		text       string
		start, end int
		normalized bool
	}{
		{"xy 敏 感 词 cd", 3, 8, true},      // separators the keyword does not have
		{"xy 敏感词 cd", 3, 6, false},       // the keyword as it is
		{"x a-b y", 2, 5, false},         // a keyword that has its own separators, found as written
		{"x a b y", 2, 5, true},          // ... and found with another one
		{"敏\u200b 感\u3000词", 0, 6, true}, // zero-width and ideographic space
	} {
		payload := `{"model":"m","messages":[{"role":"user","content":` + quoteJSON(tt.text) + `}]}`
		verdict := evaluateWords(sdktranslator.FormatOpenAI, []byte(payload), contexts, wordEvalOptions{positions: true})
		if len(verdict.Matches) != 1 {
			t.Fatalf("%q: matches = %+v", tt.text, verdict.Matches)
		}
		match := verdict.Matches[0]
		if match.Start != tt.start || match.End != tt.end || match.NormalizedOnly != tt.normalized {
			t.Errorf("%q: located at [%d,%d) normalized-only=%v, want [%d,%d) %v", tt.text, match.Start, match.End, match.NormalizedOnly, tt.start, tt.end, tt.normalized)
		}
	}
}

// However a reply is cut into events, the client reads what it would have read
// of the whole text, now with separators that a keyword ignores.
func TestReplyFilterTextStreamingEqualsWholeTextWithIgnoredSeparators(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 19))
	alphabet := []string{"a", "b", "c", "ab", "x", " ", "  ", "\n", "-", ".", "，", "😀", "敏", "感", "词", "é", "\u200b"}
	keywords := []string{"ab", "abc", "bca", "敏感", "敏感词", "cab x", "xx"}
	for trial := 0; trial < 500; trial++ {
		var rules []WordRule
		for _, i := range rng.Perm(len(keywords))[:2+rng.IntN(4)] {
			if rng.IntN(3) == 0 {
				rules = append(rules, kw(keywords[i]))
			} else {
				rules = append(rules, looseKw(keywords[i]))
			}
		}
		engine, _ := replyEngineOf(t, WordActionReplace, rules...)
		var whole strings.Builder
		for n := 3 + rng.IntN(40); n > 0; n-- {
			whole.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		text := whole.String()
		want := engine.filterText(text, false).out

		var pieces []string
		for from := 0; from < len(text); {
			to := from + 1 + rng.IntN(7)
			for to < len(text) && !isRuneStart(text[to]) {
				to++
			}
			if to > len(text) {
				to = len(text)
			}
			pieces = append(pieces, text[from:to])
			from = to
		}
		fresh, _ := replyEngineOf(t, WordActionReplace, rules...)
		if got := streamText(fresh, pieces...); got != want {
			t.Fatalf("rules %v\ntext   %q\npieces %q\nwhole  %q\nstream %q", rules, text, pieces, want, got)
		}
	}
}

func TestWordRuleThatIgnoresSeparatorsIsValidatedAndStored(t *testing.T) {
	bad := tenantPolicy(looseKw("敏 感 词"), kw("敏感词"), looseKw("敏-感-词"), looseKw("---"), WordRule{Kind: WordRuleRegex, Pattern: `\d+`, Enabled: true, IgnoreSeparators: true}, rx(`\d{5}`))
	problems := wordPolicyProblems(t, bad)
	codes := map[int]string{}
	for _, problem := range problems {
		codes[problem.Index] = problem.Code
	}
	if len(problems) != 2 || codes[3] != "only_separators" || codes[4] != "invalid_option" {
		t.Fatalf("problems = %+v", problems)
	}

	good := tenantPolicy(looseKw("敏 感 词"), kw("敏感词"), looseKw("敏-感-词"), looseKw("敏感词"))
	if err := good.Normalize(); err != nil {
		t.Fatal(err)
	}
	// "敏 感 词", "敏-感-词" and "敏感词" that ignore separators are one rule; the plain keyword is another.
	if len(good.Rules) != 2 || !good.Rules[0].IgnoreSeparators || good.Rules[1].IgnoreSeparators {
		t.Fatalf("rules = %+v", good.Rules)
	}

	encoded, err := json.Marshal(good.Rules)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []WordRule
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded[0].IgnoreSeparators || decoded[1].IgnoreSeparators {
		t.Fatalf("decoded = %+v, err = %v", decoded, err)
	}
	if strings.Count(string(encoded), "ignore_separators") != 1 {
		t.Fatalf("the option is written only where it is on: %s", encoded)
	}
	// Policies stored before the option existed have no such field.
	var old []WordRule
	if err := json.Unmarshal([]byte(`[{"id":"1","kind":"keyword","pattern":"x","enabled":true}]`), &old); err != nil || old[0].IgnoreSeparators {
		t.Fatalf("old = %+v, err = %v", old, err)
	}
}

func TestMatcherSizeCountsTheKeywordsThatIgnoreSeparators(t *testing.T) {
	plain := newWordMatcher(refs("tenant", kw("敏感词"), kw("abc")), false)
	loose := newWordMatcher(refs("tenant", looseKw("敏感词"), looseKw("abc")), false)
	if loose.size() < plain.size()*9/10 {
		t.Fatalf("a matcher of keywords that ignore separators weighs %d against %d: the second trie is not counted", loose.size(), plain.size())
	}
}
