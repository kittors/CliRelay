package contentmoderation

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func TestNormalizeForMatch(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		foldTraditional bool
		want            string
	}{
		{"ascii case and whitespace", "  Hello \t\n WORLD ", false, "hello world"},
		{"zero width space", "for\u200bbid\u200cden", false, "forbidden"},
		{"other format controls", "\ufeffbad\u2060word\u00ad\u202e", false, "badword"},
		{"variation selector and hangul filler", "bad\ufe0f\u3164word", false, "badword"},
		{"full width", "ＦＯＲＢＩＤＤＥＮ\u3000ｗｏｒｄ", false, "forbidden word"},
		{"ligature and circled digit", "ﬁle ①", false, "file 1"},
		{"full case folding", "STRASSE straße ΣΊΣΥΦΟΣ", false, "strasse strasse σίσυφοσ"},
		{"traditional kept when off", "測試軟體", false, "測試軟體"},
		{"traditional folded when on", "測試軟體", true, "测试软体"},
		{"simplified unchanged when on", "测试软件", true, "测试软件"},
		{"only ignorable", "\u200b\u200d\ufeff", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeForMatch(tt.input, tt.foldTraditional); got != tt.want {
				t.Fatalf("normalizeForMatch(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// Keyword lists are normalized once and compared against normalized text, so
// normalizing twice must change nothing.
func TestNormalizeForMatchIsIdempotent(t *testing.T) {
	samples := []string{
		"ＦＯＲＢＩＤＤＥＮ", "for\u200bbidden", "İstanbul ǅemal ﬃ", "e\u0301 é ℌ ㎏ ½",
		"測試 臺灣 乾隆 後來", "Ǳ ǲ ǳ ΐ ΰ ẞ", "\u3000中文\u00a0空格", "ㄱ\u3164ㅏ",
	}
	for _, sample := range samples {
		for _, foldTraditional := range []bool{false, true} {
			once := normalizeForMatch(sample, foldTraditional)
			if twice := normalizeForMatch(once, foldTraditional); twice != once {
				t.Fatalf("not idempotent for %q (fold=%v): %q then %q", sample, foldTraditional, once, twice)
			}
		}
	}
}

// normalizeEveryStep is normalizeForMatch without its shortcut for simple words.
func normalizeEveryStep(s string, foldTraditional bool) string {
	var table map[rune]rune
	if foldTraditional {
		table = traditionalToSimplified()
	}
	return strings.Join(strings.Fields(normalizeSteps(s, table)), " ")
}

// normalizeAlphabet mixes the simple runes the shortcut maps itself with ones
// that interact with their neighbours: combining marks, Hangul jamo, the
// half-width voiced mark, characters whose folding changes length.
var normalizeAlphabet = []string{
	"a", "Z", "K", "S", "0", "-", ".", "\x00", "\x7f",
	" ", "  ", "\t", "\n", "\r\n", "\v", "\f",
	"\u00a0", "\u0085", "\u3000", "\u2028", "\u2003",
	"\u200b", "\u200d", "\ufeff", "\u00ad", "\ufe0f", "\u3164", "\u2060", "\U000e0041",
	"\u0301", "\u0308", "\u0327", "\u0345", "\u3099", "\u309a",
	"測", "試", "軟", "體", "乾", "臺", "后", "中", "文", "㐀", "䶿", "鿿",
	"Ａ", "ｚ", "！", "１", "，", "：", "～",
	"、", "。", "《", "》", "「", "」", "“", "”", "‘", "‚", "—", "…", "–",
	"Σ", "ς", "ß", "ẞ", "İ", "ı", "ﬁ", "ǅ", "\u212a", "\u212b", "①", "㎏", "½", "ℌ", "ΐ",
	"ᄀ", "ᅡ", "ᆨ", "가", "각", "ｶ", "ﾞ", "か", "゛",
	"é", "e\u0301", "😀", "\U0001f44d\U0001f3fd", "\U0002a700",
}

// The shortcut must give exactly what every step gives, and like the steps
// it must be idempotent.
func TestNormalizeShortcutMatchesEveryStep(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	var b strings.Builder
	for i := 0; i < 50000; i++ {
		b.Reset()
		for n := random.Intn(16); n > 0; n-- {
			b.WriteString(normalizeAlphabet[random.Intn(len(normalizeAlphabet))])
		}
		s := b.String()
		for _, foldTraditional := range []bool{false, true} {
			got := normalizeForMatch(s, foldTraditional)
			if want := normalizeEveryStep(s, foldTraditional); got != want {
				t.Fatalf("normalizeForMatch(%+q, %v) = %+q, every step gives %+q", s, foldTraditional, got, want)
			}
			if again := normalizeForMatch(got, foldTraditional); again != got {
				t.Fatalf("not idempotent for %+q (fold=%v): %+q then %+q", s, foldTraditional, got, again)
			}
		}
	}
}

// Every simple rune maps on its own as every step would map it, and neither it
// nor its mapping combines with a neighbour, so the steps cannot treat it
// differently inside a longer word. ASCII whitespace, where words are split,
// does not combine on either side.
func TestSimpleRunesMapOnTheirOwn(t *testing.T) {
	table := traditionalToSimplified()
	simple := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !isSimpleRune(r) || (r < utf8.RuneSelf && isASCIISpace(byte(r))) {
			continue
		}
		simple++
		s := string(r)
		for _, tbl := range []map[rune]rune{nil, table} {
			var c spaceCollapser
			c.writeSimple(s, tbl)
			if got, want := c.b.String(), normalizeSteps(s, tbl); got != want {
				t.Fatalf("U+%04X maps to %+q, every step gives %+q", r, got, want)
			}
		}
		for _, form := range []string{s, normalizeSteps(s, nil), normalizeSteps(s, table)} {
			for _, x := range form {
				if !norm.NFKC.PropertiesString(string(x)).BoundaryBefore() {
					t.Fatalf("U+%04X (as U+%04X) combines with the rune before it", r, x)
				}
			}
		}
	}
	if simple < 27000 {
		t.Fatalf("only %d simple runes", simple)
	}
	for _, c := range []byte(" \t\n\v\f\r") {
		p := norm.NFKC.PropertiesString(string(c))
		if !p.BoundaryBefore() || !p.BoundaryAfter() || !unicode.IsSpace(rune(c)) {
			t.Fatalf("%q is not an inert space", c)
		}
	}
}

func TestIsIgnorableShortcutRanges(t *testing.T) {
	for r := rune(0); r < 0xFE00; r++ {
		if r == 0xAD {
			r = 0x3165
		}
		if unicode.In(r, unicode.Cf, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point) {
			t.Fatalf("U+%04X is ignorable but isIgnorable skips it", r)
		}
	}
}

func TestTraditionalTableNeverMapsTwice(t *testing.T) {
	table := traditionalToSimplified()
	if len(table) < 3000 {
		t.Fatalf("table has %d entries; the embedded OpenCC data did not load", len(table))
	}
	for from, to := range table {
		if next, ok := table[to]; ok {
			t.Fatalf("%q maps to %q, which maps again to %q", from, to, next)
		}
		if from < utf8.RuneSelf || (from >= 0xFF01 && from <= 0xFF5E) {
			t.Fatalf("%q is mapped, but simple words never look it up", from)
		}
	}
}

func TestResolveTraditionalChainsHandlesChainsAndCycles(t *testing.T) {
	table := map[rune]rune{'a': 'b', 'b': 'c', 'x': 'y', 'y': 'x'}
	resolveTraditionalChains(table)
	if table['a'] != 'c' {
		t.Fatalf("a -> %q, want c", table['a'])
	}
	for from, to := range table {
		if _, again := table[to]; again {
			t.Fatalf("%q -> %q maps again: %v", from, to, table)
		}
	}
}

func keywordProfile(t *testing.T, keywords ...string) Profile {
	t.Helper()
	profile := testProfile(t, "tenant-a", "keywords", ModePreBlock, KeywordModeKeywordOnly)
	profile.BlockedKeywords = keywords
	return profile
}

// Each spelling below used to slip past a keyword list with "forbidden" on it.
func TestKeywordMatchSurvivesSpellingTricks(t *testing.T) {
	profile := keywordProfile(t, "forbidden")
	for _, text := range []string{
		"contains for\u200bbid\u200bden text",
		"contains ＦＯＲＢＩＤＤＥＮ text",
		"contains FoRbIdDeN text",
		"contains for\u00adbidden text",
	} {
		decision := NewEvaluator(nil).Evaluate(context.Background(), profile, TextInput(text))
		if !decision.WouldBlock || decision.MatchedKeyword != "forbidden" {
			t.Fatalf("%q was not blocked: %#v", text, decision)
		}
	}
}

func TestKeywordMatchTraditionalChineseIsOptIn(t *testing.T) {
	profile := keywordProfile(t, "测试")
	if decision := NewEvaluator(nil).Evaluate(context.Background(), profile, TextInput("這是測試")); decision.WouldBlock {
		t.Fatalf("traditional text matched with folding off: %#v", decision)
	}
	profile.KeywordFoldTraditionalChinese = true
	decision := NewEvaluator(nil).Evaluate(context.Background(), profile, TextInput("這是測試"))
	if !decision.WouldBlock || decision.MatchedKeyword != "测试" {
		t.Fatalf("traditional text not blocked with folding on: %#v", decision)
	}
	// A list written in Traditional characters catches Simplified text too.
	profile.BlockedKeywords = []string{"測試"}
	decision = NewEvaluator(nil).Evaluate(context.Background(), profile, TextInput("这是测试"))
	if !decision.WouldBlock || decision.MatchedKeyword != "測試" {
		t.Fatalf("simplified text not blocked by a traditional keyword: %#v", decision)
	}
}

// The decision names the keyword as the operator wrote it, not its normalized form.
func TestKeywordMatchReportsTheConfiguredSpelling(t *testing.T) {
	profile := keywordProfile(t, "ＢＡＤ\u3000Ｗｏｒｄ")
	decision := NewEvaluator(nil).Evaluate(context.Background(), profile, TextInput("a bad\nword here"))
	if !decision.WouldBlock || decision.MatchedKeyword != "ＢＡＤ\u3000Ｗｏｒｄ" {
		t.Fatalf("decision = %#v", decision)
	}
}

// A keyword that normalizes to nothing would match every request as an empty
// substring; it must be dropped instead.
func TestKeywordOfOnlyIgnorableCharactersIsDropped(t *testing.T) {
	profile := keywordProfile(t, "\u200b\u200b")
	if decision := NewEvaluator(nil).Evaluate(context.Background(), profile, TextInput("ordinary text")); decision.WouldBlock {
		t.Fatalf("empty keyword blocked a request: %#v", decision)
	}
}

func TestCompiledKeywordSetIsCachedByContent(t *testing.T) {
	first := compiledKeywordSet([]string{"alpha", "beta"}, false)
	if again := compiledKeywordSet([]string{"alpha", "beta"}, false); again != first {
		t.Fatal("an unchanged list was compiled again")
	}
	if folded := compiledKeywordSet([]string{"alpha", "beta"}, true); folded == first {
		t.Fatal("the folding option did not get a set of its own")
	}
	changed := compiledKeywordSet([]string{"alpha", "gamma"}, false)
	if changed == first || changed.entries[1].original != "gamma" {
		t.Fatalf("a changed list reused the old set: %#v", changed.entries)
	}
}

// Keywords from every class are matched, and the decision says which class
// the match was found in.
func TestKeywordMatchReportsTheMatchedClass(t *testing.T) {
	profile := keywordProfile(t, "forbidden")
	cases := map[Class]Input{
		ClassPrimary:          NewInput([]Segment{{Class: ClassPrimary, User: true, Text: "forbidden"}}),
		ClassToolOutput:       NewInput([]Segment{{Class: ClassPrimary, User: true, Text: "hi"}, {Class: ClassToolOutput, Text: "a forbidden result"}}),
		ClassAssistantHistory: NewInput([]Segment{{Class: ClassAssistantHistory, Text: "forbidden"}, {Class: ClassPrimary, User: true, Text: "ok"}}),
	}
	for class, input := range cases {
		decision := NewEvaluator(nil).Evaluate(context.Background(), profile, input)
		if !decision.WouldBlock || decision.MatchedSource != class.String() {
			t.Fatalf("%s: decision = %#v", class, decision)
		}
	}
}

func BenchmarkKeywordScanAgentRequest(b *testing.B) {
	keywords := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		keywords = append(keywords, "敏感词"+strings.Repeat("甲", i%7)+string(rune('a'+i%26)))
	}
	profile := Profile{BlockedKeywords: keywords, Mode: ModePreBlock, KeywordMode: KeywordModeKeywordOnly}
	segments := make([]Segment, 0, 400)
	for i := 0; i < 400; i++ {
		segments = append(segments, Segment{Class: Class(i % classCount), Turn: i, Text: strings.Repeat("工具输出 tool output line ", 60)})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NewEvaluator(nil).Evaluate(context.Background(), profile, NewInput(segments))
	}
}
