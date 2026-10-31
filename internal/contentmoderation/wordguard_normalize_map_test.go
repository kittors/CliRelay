package contentmoderation

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// The mapped normalization must produce exactly the text normalizeForMatch
// does: matches are found on one and replaced using the other.
func TestNormalizeMappedMatchesNormalizeForMatch(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	var b strings.Builder
	for i := 0; i < 60000; i++ {
		b.Reset()
		for n := random.Intn(18); n > 0; n-- {
			b.WriteString(normalizeAlphabet[random.Intn(len(normalizeAlphabet))])
		}
		s := b.String()
		for _, fold := range []bool{false, true} {
			got := normalizeMapped(s, fold)
			if want := normalizeForMatch(s, fold); got.text != want {
				t.Fatalf("normalizeMapped(%+q, %v).text = %+q, normalizeForMatch gives %+q", s, fold, got.text, want)
			}
			if runes := utf8.RuneCountInString(got.text); len(got.from) != runes || len(got.to) != runes {
				t.Fatalf("normalizeMapped(%+q, %v): %d runes but %d/%d offsets", s, fold, runes, len(got.from), len(got.to))
			}
			checkMapIsOrdered(t, s, got)
		}
	}
}

func checkMapIsOrdered(t *testing.T, s string, f foldedText) {
	t.Helper()
	prevFrom, prevTo := int32(0), int32(0)
	for i := range f.from {
		if f.from[i] < prevFrom || f.to[i] < prevTo || f.from[i] >= f.to[i] || int(f.to[i]) > len(s) {
			t.Fatalf("map of %+q is not ordered at rune %d: from=%v to=%v", s, i, f.from, f.to)
		}
		prevFrom, prevTo = f.from[i], f.to[i]
	}
}

// Whatever part of the normalized text is picked, the original range it maps
// to must normalize to something that still contains it. That is what makes a
// replacement cover the words that matched and no others.
func TestNormalizeMappedRangesCoverTheirText(t *testing.T) {
	random := rand.New(rand.NewSource(11))
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.Reset()
		for n := random.Intn(14) + 1; n > 0; n-- {
			b.WriteString(normalizeAlphabet[random.Intn(len(normalizeAlphabet))])
		}
		s := b.String()
		fold := random.Intn(2) == 0
		f := normalizeMapped(s, fold)
		runes := []rune(f.text)
		if len(runes) == 0 {
			continue
		}
		lo := random.Intn(len(runes))
		hi := lo + 1 + random.Intn(len(runes)-lo)
		span := textSpan{start: len(string(runes[:lo])), end: len(string(runes[:hi]))}
		mapped, ok := f.originalSpans([]textSpan{span})
		if !ok || len(mapped) != 1 {
			t.Fatalf("originalSpans(%+q, %v) failed for %v", s, span, f.text)
		}
		want := strings.TrimSpace(f.text[span.start:span.end])
		if want == "" {
			continue
		}
		got := normalizeForMatch(s[mapped[0].start:mapped[0].end], fold)
		if !strings.Contains(got, want) {
			t.Fatalf("original %+q (fold=%v): normalized span %q maps to %q, which normalizes to %q", s, fold, want, s[mapped[0].start:mapped[0].end], got)
		}
	}
}

func TestNormalizeMappedPositions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		fold  bool
		match string // normalized text to find
		want  string // original text that range must be
	}{
		{"zero width inside the word", "say ba\u200bd\u200cword now", false, "badword", "ba\u200bd\u200cword"},
		{"zero width at the edges stays out", "\u200bbad\u200b", false, "bad", "bad"},
		{"full width letters", "x ＢＡＤ y", false, "bad", "ＢＡＤ"},
		{"upper case", "Say BaD Word", false, "bad word", "BaD Word"},
		{"run of whitespace is one space", "bad \t\n  word!", false, "bad word", "bad \t\n  word"},
		{"no break space", "bad\u00a0word", false, "bad word", "bad\u00a0word"},
		{"ligature covers the whole character", "eﬃcient", false, "ffi", "ﬃ"},
		{"combining mark stays with its letter", "café au lait", false, "café", "café"},
		{"traditional folded", "這是測試軟體喔", true, "测试软体", "測試軟體"},
		{"traditional kept when off", "這是測試軟體喔", false, "測試軟體", "測試軟體"},
		{"cjk after ascii", "abc违禁词def", false, "违禁词", "违禁词"},
		{"emoji", "bad 😀 word", false, "😀", "😀"},
		{"sharp s expands", "STRASSE straße", false, "strasse strasse", "STRASSE straße"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := normalizeMapped(tt.input, tt.fold)
			start := strings.Index(f.text, tt.match)
			if start < 0 {
				t.Fatalf("normalized text %q lacks %q", f.text, tt.match)
			}
			got, ok := f.originalSpans([]textSpan{{start, start + len(tt.match)}})
			if !ok {
				t.Fatalf("originalSpans failed")
			}
			if original := tt.input[got[0].start:got[0].end]; original != tt.want {
				t.Fatalf("range covers %q, want %q", original, tt.want)
			}
		})
	}
}

func TestNormalizeMappedRejectsBadSpans(t *testing.T) {
	f := normalizeMapped("héllo wörld", false)
	for _, spans := range [][]textSpan{
		{{0, 0}},                 // empty
		{{0, len(f.text) + 1}},   // beyond the text
		{{1, 2}},                 // splits é
		{{4, 6}, {2, 3}},         // not ascending
		{{-1, 2}},                // negative
		{{0, 2}, {1, 3}, {3, 4}}, // overlapping inputs go through, but only if aligned
	} {
		_, ok := f.originalSpans(spans)
		if ok && !alignedAscending(f.text, spans) {
			t.Fatalf("originalSpans accepted %v", spans)
		}
	}
}

func alignedAscending(text string, spans []textSpan) bool {
	prev := 0
	for _, s := range spans {
		if s.start < prev || s.end <= s.start || s.end > len(text) || !utf8.RuneStart(text[s.start]) || (s.end < len(text) && !utf8.RuneStart(text[s.end])) {
			return false
		}
		prev = s.end
	}
	return true
}

func TestNormalizeMappedFlagsInvalidUTF8(t *testing.T) {
	if f := normalizeMapped("ok \xff text", false); f.exact {
		t.Fatalf("invalid UTF-8 must not be reported as exact")
	}
	if f := normalizeMapped("fine text", false); !f.exact {
		t.Fatalf("valid UTF-8 must be exact")
	}
}
