package contentmoderation

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

func refs(layer string, rules ...WordRule) []wordRuleRef {
	out := make([]wordRuleRef, 0, len(rules))
	for i, rule := range rules {
		if rule.ID == "" {
			rule.ID = fmt.Sprintf("%s-%d", layer, i)
		}
		out = append(out, wordRuleRef{WordRule: rule, Layer: layer})
	}
	return out
}

func matcherFor(t *testing.T, fold bool, rules ...WordRule) *wordMatcher {
	t.Helper()
	m := compileWordMatcher(refs("tenant", rules...), fold)
	if m == nil {
		t.Fatal("no matcher")
	}
	return m
}

func hitTexts(text string, find wordFind) []string {
	out := make([]string, 0, len(find.hits))
	for _, hit := range find.hits {
		out = append(out, text[hit.start:hit.end])
	}
	return out
}

func TestWordMatcherKeywordsLeftmostLongest(t *testing.T) {
	m := matcherFor(t, false, kw("ab"), kw("abc"), kw("bcd"), kw("x"))
	tests := []struct {
		text string
		want []string
	}{
		{"abcd", []string{"abc"}},
		{"bcd", []string{"bcd"}},
		{"ab abc x", []string{"ab", "abc", "x"}},
		{"nothing here", nil},
		{"xxabx", []string{"x", "x", "ab", "x"}},
	}
	for _, tt := range tests {
		got := hitTexts(tt.text, m.find(tt.text, findOptions{}))
		if fmt.Sprint(got) != fmt.Sprint(tt.want) {
			t.Errorf("find(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestWordMatcherChineseAndMixedText(t *testing.T) {
	m := matcherFor(t, false, kw("违禁词"), kw("违禁"), kw("Bad Word"))
	text := normalizeForMatch("这里有违禁词和 BAD   word 还有违禁", false)
	got := hitTexts(text, m.find(text, findOptions{}))
	if fmt.Sprint(got) != fmt.Sprint([]string{"违禁词", "bad word", "违禁"}) {
		t.Fatalf("hits = %q", got)
	}
}

func TestWordMatcherFoldsTraditionalOnlyWhenAsked(t *testing.T) {
	traditional := normalizeForMatch("測試軟體", false)
	if got := matcherFor(t, false, kw("测试软体")).find(traditional, findOptions{}); len(got.hits) != 0 {
		t.Fatalf("matched Traditional text without folding: %+v", got)
	}
	folded := normalizeForMatch("測試軟體", true)
	if got := matcherFor(t, true, kw("測試")).find(folded, findOptions{}); len(got.hits) != 1 {
		t.Fatalf("a Traditional keyword must match folded text: %+v", got)
	}
}

func TestWordMatcherUndecidedTail(t *testing.T) {
	m := matcherFor(t, false, kw("badword"), kw("bad"))
	tests := []struct {
		name      string
		text      string
		more      bool
		undecided int
		hits      []string
	}{
		{"complete keyword, nothing follows", "say badword", false, -1, []string{"badword"}},
		{"partial keyword at the end waits", "say badw", true, 4, nil},
		{"partial keyword at the end is dropped at the end of the stream", "say badw", false, -1, []string{"bad"}},
		{"short keyword that could still grow waits", "say bad", true, 4, nil},
		{"short keyword is final once the stream ends", "say bad", false, -1, []string{"bad"}},
		{"a keyword that cannot grow is final at once", "bad badword", true, -1, []string{"bad", "badword"}},
		{"an earlier final hit stays", "bad x badw", true, 6, []string{"bad"}},
		{"nothing resembling a keyword", "hello", true, -1, nil},
		{"letters that only look like the start", "bax", true, -1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			find := m.find(tt.text, findOptions{more: tt.more})
			if find.undecided != tt.undecided {
				t.Fatalf("undecided = %d, want %d", find.undecided, tt.undecided)
			}
			if got := hitTexts(tt.text, find); fmt.Sprint(got) != fmt.Sprint(tt.hits) {
				t.Fatalf("hits = %q, want %q", got, tt.hits)
			}
		})
	}
}

func TestWordMatcherRegex(t *testing.T) {
	m := matcherFor(t, false, rx(`\d{11}`), rx(`foo(bar)?`))
	text := "call 13800138000 or foobar, foo."
	got := hitTexts(text, m.find(text, findOptions{}))
	if fmt.Sprint(got) != fmt.Sprint([]string{"13800138000", "foobar", "foo"}) {
		t.Fatalf("hits = %q", got)
	}
	// Case-insensitive, and leftmost-longest: foobar, not foo.
	upper := "FooBar"
	if got := hitTexts(upper, m.find(upper, findOptions{})); fmt.Sprint(got) != fmt.Sprint([]string{"FooBar"}) {
		t.Fatalf("hits = %q", got)
	}
}

func TestWordMatcherRegexMatchAtTheEndWaitsForMore(t *testing.T) {
	m := matcherFor(t, false, rx(`\d{3,}`))
	find := m.find("code 1234", findOptions{more: true})
	if find.undecided != 5 || len(find.hits) != 0 {
		t.Fatalf("a match running to the end may grow: %+v", find)
	}
	find = m.find("code 1234 and", findOptions{more: true})
	if find.undecided != -1 || len(find.hits) != 1 {
		t.Fatalf("a match that stops before the end is final: %+v", find)
	}
	find = m.find("code 1234", findOptions{})
	if len(find.hits) != 1 {
		t.Fatalf("at the end of the stream the match counts: %+v", find)
	}
}

func TestWordMatcherOverlappingHitsBecomeTheirUnion(t *testing.T) {
	m := matcherFor(t, false, kw("abc"), rx(`c\d+`))
	text := "xabc123y"
	find := m.find(text, findOptions{})
	if got := hitTexts(text, find); fmt.Sprint(got) != fmt.Sprint([]string{"abc123"}) {
		t.Fatalf("hits = %q, want the union of both", got)
	}
	if m.rules[find.hits[0].rule].Pattern != "abc" {
		t.Fatalf("the hit belongs to the rule that began first, got %q", m.rules[find.hits[0].rule].Pattern)
	}
}

func TestWordMatcherHoldBackNeverCutsAHit(t *testing.T) {
	// The keyword scan wants to hold from 2 ("abc" begins "abcde"); the
	// expression has a final hit, "zab", that covers part of it. Holding from 2
	// would cut that hit in two, so the hold-back starts at the hit.
	m := matcherFor(t, false, kw("abcde"), rx(`zab`))
	text := "xzabc"
	find := m.find(text, findOptions{more: true})
	for _, hit := range find.hits {
		if hit.start < find.undecided && hit.end > find.undecided {
			t.Fatalf("hit %+v straddles undecided=%d", hit, find.undecided)
		}
	}
	if find.undecided != 1 || len(find.hits) != 0 {
		t.Fatalf("find = %+v, want the whole hit held from offset 1", find)
	}
	// Once the text is complete the expression's hit is final.
	if done := m.find(text, findOptions{}); len(done.hits) != 1 || done.undecided != -1 {
		t.Fatalf("find = %+v", done)
	}
}

func TestWordMatcherLimitStopsEarly(t *testing.T) {
	m := matcherFor(t, false, kw("a"))
	text := strings.Repeat("a ", 1000)
	if got := m.find(text, findOptions{limit: 1}); len(got.hits) != 1 {
		t.Fatalf("hits = %d", len(got.hits))
	}
	if got := m.find(text, findOptions{}); len(got.hits) != 1000 {
		t.Fatalf("hits = %d", len(got.hits))
	}
}

func TestWordMatcherIgnoresDisabledAndBrokenRules(t *testing.T) {
	off := kw("secret")
	off.Enabled = false
	if m := compiledWordMatcher(refs("tenant", off), false); m != nil {
		t.Fatalf("a list without an enabled rule has no matcher")
	}
	// A regex that does not compile (stored by some other build) is skipped.
	m := compileWordMatcher(refs("tenant", rx(`(`), kw("fine")), false)
	if got := m.find("a fine day", findOptions{}); len(got.hits) != 1 {
		t.Fatalf("the valid rule must still work: %+v", got)
	}
}

func TestCompiledWordMatcherIsCachedByContent(t *testing.T) {
	list := refs("tenant", kw("alpha"), rx(`be+ta`))
	first := compiledWordMatcher(list, false)
	if again := compiledWordMatcher(refs("tenant", kw("alpha"), rx(`be+ta`)), false); again != first {
		t.Fatalf("the same rules must reuse the compiled matcher")
	}
	if compiledWordMatcher(list, true) == first {
		t.Fatalf("the fold setting changes the compiled form")
	}
	if compiledWordMatcher(refs("key", kw("alpha"), rx(`be+ta`)), false) == first {
		t.Fatalf("the layer is part of the attribution")
	}
	changed := refs("tenant", kw("alpha"), rx(`be+ta`), kw("gamma"))
	if compiledWordMatcher(changed, false) == first {
		t.Fatalf("a different list must compile again")
	}
}

// naiveKeywordScan is the definition the trie must agree with: at each
// position the longest keyword that starts there wins, and a position where
// the rest of the text is a proper prefix of some keyword is undecided.
func naiveKeywordScan(text string, keywords []string, more bool) ([][2]int, int) {
	var hits [][2]int
	for p := 0; p < len(text); {
		if more {
			rest := text[p:]
			for _, keyword := range keywords {
				if len(keyword) > len(rest) && strings.HasPrefix(keyword, rest) {
					return hits, p
				}
			}
		}
		best := -1
		for _, keyword := range keywords {
			if strings.HasPrefix(text[p:], keyword) && len(keyword) > best {
				best = len(keyword)
			}
		}
		if best > 0 {
			hits = append(hits, [2]int{p, p + best})
			p += best
			continue
		}
		_, size := firstRune(text[p:])
		p += size
	}
	return hits, -1
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 1
}

func TestWordTrieMatchesNaiveScan(t *testing.T) {
	random := rand.New(rand.NewSource(3))
	alphabet := []string{"a", "b", "c", "ab", "é", "违", "禁"}
	word := func(maxLen int) string {
		var b strings.Builder
		for n := random.Intn(maxLen) + 1; n > 0; n-- {
			b.WriteString(alphabet[random.Intn(len(alphabet))])
		}
		return b.String()
	}
	for i := 0; i < 4000; i++ {
		keywords := make([]string, 0, 8)
		var builder wordTrieBuilder
		for n := random.Intn(7) + 1; n > 0; n-- {
			k := word(4)
			keywords = append(keywords, k)
			builder.add(k, len(keywords)-1)
		}
		trie := builder.build()
		text := word(14)
		for _, more := range []bool{false, true} {
			got, undecided := trie.scan(text, findOptions{more: more})
			want, wantUndecided := naiveKeywordScan(text, keywords, more)
			var gotSpans [][2]int
			for _, hit := range got {
				gotSpans = append(gotSpans, [2]int{hit.start, hit.end})
			}
			if fmt.Sprint(gotSpans) != fmt.Sprint(want) || undecided != wantUndecided {
				t.Fatalf("keywords %q text %q more=%v: trie gave %v undecided %d, naive %v undecided %d", keywords, text, more, gotSpans, undecided, want, wantUndecided)
			}
		}
	}
}

func TestWordTrieFirstDuplicateWins(t *testing.T) {
	var builder wordTrieBuilder
	builder.add("word", 4)
	builder.add("other", 9)
	builder.add("word", 7)
	trie := builder.build()
	hits, _ := trie.scan("a word", findOptions{})
	if len(hits) != 1 || hits[0].rule != 4 {
		t.Fatalf("hits = %+v, want the first rule registered for the text", hits)
	}
}

func TestWordTrieSortedEdges(t *testing.T) {
	// Many first bytes under one node exercise the binary search in child.
	var builder wordTrieBuilder
	for b := 'a'; b <= 'z'; b++ {
		builder.add("x"+string(b)+"!", int(b))
	}
	trie := builder.build()
	for b := 'a'; b <= 'z'; b++ {
		text := "..x" + string(b) + "!.."
		hits, _ := trie.scan(text, findOptions{})
		if len(hits) != 1 || hits[0].start != 2 || hits[0].end != 5 {
			t.Fatalf("%q: hits = %+v", text, hits)
		}
	}
}

// A big list and a big text stay fast: the scan must not be quadratic in
// either.
func TestWordMatcherScalesToLargeLists(t *testing.T) {
	random := rand.New(rand.NewSource(5))
	rules := make([]WordRule, 0, MaxWordRulesPerPolicy)
	for i := 0; i < MaxWordRulesPerPolicy; i++ {
		var b strings.Builder
		for n := random.Intn(10) + 3; n > 0; n-- {
			b.WriteRune(rune('a' + random.Intn(26)))
		}
		rules = append(rules, kw(b.String()+fmt.Sprint(i)))
	}
	start := time.Now()
	m := matcherFor(t, false, rules...)
	compile := time.Since(start)
	var text strings.Builder
	for text.Len() < 4<<20 {
		text.WriteString("the quick brown fox jumps over the lazy dog and says hello world ")
	}
	start = time.Now()
	find := m.find(text.String(), findOptions{})
	scan := time.Since(start)
	if len(find.hits) != 0 {
		t.Fatalf("unexpected hits: %d", len(find.hits))
	}
	if compile > 5*time.Second || scan > 5*time.Second {
		t.Fatalf("compile %v, scan of 4 MiB %v", compile, scan)
	}
	t.Logf("compile %v, scan of %d bytes %v", compile, text.Len(), scan)
}

func TestMergeWordHits(t *testing.T) {
	hits := []wordHit{{10, 12, 1}, {0, 3, 2}, {2, 6, 3}, {6, 8, 4}, {10, 11, 5}}
	got := mergeWordHits(hits)
	sort.SliceStable(got, func(i, j int) bool { return got[i].start < got[j].start })
	want := []wordHit{{0, 6, 2}, {6, 8, 4}, {10, 12, 1}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}
