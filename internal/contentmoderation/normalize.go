package contentmoderation

import (
	_ "embed"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// caseFolder applies full Unicode case folding, so "ß" matches "ss" and Greek
// or Cyrillic letters fold the way strings.ToLower does not. The Caser that
// cases.Fold returns is documented as stateless and safe for concurrent use.
var caseFolder = cases.Fold()

// normalizeForMatch maps text onto the form keyword matching compares in. The
// keyword list and the scanned request go through the same function, so a
// keyword cannot be dodged by spelling it differently:
//
//   - default-ignorable code points are removed: Unicode Cf format controls
//     such as U+200B ZERO WIDTH SPACE, variation selectors and Hangul fillers.
//     They render as nothing, so one inserted inside a word used to hide it
//     from a plain substring search;
//   - NFKC folds compatibility forms (full-width "ＡＢＣ", ligatures, circled
//     digits) onto their plain form;
//   - full case folding replaces strings.ToLower;
//   - optionally, Traditional Chinese characters fold to Simplified;
//   - runs of whitespace collapse to one space, as the extractor always did,
//     so a keyword containing a space matches any whitespace in the request.
//
// The result is idempotent. Keyword lists depend on that: they are normalized
// once, when compiled, and must still match normalized request text.
//
// Requests run to megabytes, so the steps are not applied to all of it. Most
// text is ASCII, CJK ideographs and full-width punctuation, characters the
// steps map one at a time whatever their neighbours (see isSimpleRune). A word
// (text between ASCII whitespace, which no step looks across) made only of
// them is mapped directly; other words take every step.
// TestNormalizeShortcutMatchesEveryStep holds the two paths to the same output.
func normalizeForMatch(s string, foldTraditional bool) string {
	var table map[rune]rune
	if foldTraditional {
		table = traditionalToSimplified()
	}
	var out spaceCollapser
	out.b.Grow(len(s))
	pending := -1 // start of the words waiting to take every step
	for i := 0; i < len(s); {
		if isASCIISpace(s[i]) {
			if pending < 0 {
				out.space()
			}
			i++
			continue
		}
		end, simple := scanWord(s, i)
		switch {
		case !simple:
			// Neighbouring words that need every step are passed on together,
			// which keeps text in other scripts from being cut into words.
			if pending < 0 {
				pending = i
			}
		case pending >= 0:
			out.writeText(normalizeSteps(s[pending:i], table))
			pending = -1
			fallthrough
		default:
			out.writeSimple(s[i:end], table)
		}
		i = end
	}
	if pending >= 0 {
		out.writeText(normalizeSteps(s[pending:], table))
	}
	return out.b.String()
}

// normalizeSteps applies every step of normalizeForMatch except the
// whitespace collapse.
func normalizeSteps(s string, table map[rune]rune) string {
	s = norm.NFKC.String(stripIgnorable(s))
	if folded := caseFolder.String(s); folded != s {
		// Folding can produce a sequence NFKC would compose differently.
		s = norm.NFKC.String(folded)
	}
	if table != nil {
		s = strings.Map(func(r rune) rune {
			if simplified, ok := table[r]; ok {
				return simplified
			}
			return r
		}, s)
	}
	return s
}

// isSimpleRune reports whether normalizeSteps maps r on its own: r is not
// ignorable, neither r nor what it maps to combines with a neighbour under
// NFKC, and its only change is the case and width folding writeSimple does.
func isSimpleRune(r rune) bool {
	switch {
	case r < utf8.RuneSelf:
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
		return true
	case r >= 0xFF01 && r <= 0xFF5E: // full-width forms of ASCII
		return true
	case r >= 0x3001 && r <= 0x3003, r >= 0x3008 && r <= 0x3011: // 、。〃〈〉《》「」『』【】
		return true
	case r >= 0x2018 && r <= 0x201D, r == 0x2014: // ‘’‚‛“” and the em dash
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK Unified Ideographs Extension A
		return true
	}
	return false
}

func isASCIISpace(c byte) bool {
	return c == ' ' || ('\t' <= c && c <= '\r')
}

// scanWord returns where the word starting at s[i] ends, at the next ASCII
// whitespace, and whether all of it is simple.
func scanWord(s string, i int) (int, bool) {
	simple := true
	for i < len(s) {
		c := s[i]
		if c < utf8.RuneSelf {
			if isASCIISpace(c) {
				break
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		simple = simple && isSimpleRune(r)
		i += size
	}
	return i, simple
}

// spaceCollapser builds text in which every run of whitespace, by the
// strings.Fields definition, is one space, with none at either end.
type spaceCollapser struct {
	b       strings.Builder
	pending bool
}

func (c *spaceCollapser) space() { c.pending = c.b.Len() > 0 }

func (c *spaceCollapser) flush() {
	if c.pending {
		c.b.WriteByte(' ')
		c.pending = false
	}
}

// writeText appends s, collapsing its whitespace.
func (c *spaceCollapser) writeText(s string) {
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if isASCIISpace(b) {
				c.space()
			} else {
				c.flush()
				c.b.WriteByte(b)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			c.space()
		} else {
			c.flush()
			c.b.WriteRune(r)
		}
		i += size
	}
}

// writeSimple appends a word of simple runes as normalizeSteps would map it:
// NFKC turns full-width forms into ASCII, case folding lowercases ASCII, and
// the Traditional Chinese table, when given, maps ideographs.
func (c *spaceCollapser) writeSimple(word string, table map[rune]rune) {
	c.flush()
	for i := 0; i < len(word); {
		b := word[i]
		if b >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(word[i:])
			i += size
			if r < 0xFF01 || r > 0xFF5E {
				if simplified, ok := table[r]; ok {
					r = simplified
				}
				c.b.WriteRune(r)
				continue
			}
			b = byte(r - 0xFEE0)
		} else {
			i++
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		c.b.WriteByte(b)
	}
}

// collapseSpaces trims s and turns every run of whitespace into one space.
func collapseSpaces(s string) string {
	var c spaceCollapser
	c.b.Grow(len(s))
	c.writeText(s)
	return c.b.String()
}

// isIgnorable reports whether r is a default-ignorable code point. This is
// the Unicode definition minus its whitespace exclusions, which do not matter
// here because whitespace is collapsed afterwards anyway.
func isIgnorable(r rune) bool {
	// There are none below U+00AD or in U+3165..U+FDFF, which takes in CJK and
	// Hangul syllables; skipping the table search there keeps common text
	// cheap. TestIsIgnorableShortcutRanges checks the claim.
	if r < 0xAD || (r > 0x3164 && r < 0xFE00) {
		return false
	}
	return unicode.In(r, unicode.Cf, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point)
}

// stripIgnorable removes default-ignorable code points. It returns s itself
// when there are none, which is the common case.
func stripIgnorable(s string) string {
	for i, r := range s {
		if isIgnorable(r) {
			return s[:i] + strings.Map(func(r rune) rune {
				if isIgnorable(r) {
					return -1
				}
				return r
			}, s[i:])
		}
	}
	return s
}

// tsCharacters is OpenCC's per-character Traditional to Simplified table,
// embedded unmodified; see data/opencc/NOTICE for its source and license.
//
//go:embed data/opencc/TSCharacters.txt
var tsCharacters string

var (
	traditionalOnce  sync.Once
	traditionalTable map[rune]rune
)

// traditionalToSimplified maps each Traditional Chinese character onto its
// Simplified form. It is a per-character mapping, not phrase conversion: that
// is enough to undo a keyword written in the other script, and it cannot turn
// one word into a different one the way phrase rules can.
func traditionalToSimplified() map[rune]rune {
	traditionalOnce.Do(func() { traditionalTable = parseTSCharacters(tsCharacters) })
	return traditionalTable
}

// parseTSCharacters reads lines of "traditional<TAB>simplified [alternative ...]".
// The first candidate wins. Lines OpenCC marks @tofu-risk list the character
// itself first and so map to nothing. Both sides are NFKC-normalized because
// the table is applied after NFKC.
func parseTSCharacters(data string) map[rune]rune {
	table := make(map[rune]rune, 4096)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, values, ok := strings.Cut(line, "\t")
		candidates := strings.Fields(values)
		if !ok || len(candidates) == 0 {
			continue
		}
		from, fromOK := singleRune(norm.NFKC.String(strings.TrimSpace(key)))
		to, toOK := singleRune(norm.NFKC.String(candidates[0]))
		if fromOK && toOK && from != to {
			table[from] = to
		}
	}
	resolveTraditionalChains(table)
	return table
}

// resolveTraditionalChains makes the mapping idempotent: if A maps to B and B
// maps to C, A maps straight to C, so no mapped character is mapped again.
// A character on a cycle has no stable target and loses its mapping. Values
// only ever point at characters that are not keys, so the invariant holds
// whatever order the map is walked in.
func resolveTraditionalChains(table map[rune]rune) {
	for from, to := range table {
		for steps := 0; steps < len(table); steps++ {
			next, ok := table[to]
			if !ok {
				break
			}
			to = next
		}
		if _, cyclic := table[to]; cyclic || to == from {
			delete(table, from)
			continue
		}
		table[from] = to
	}
}

func singleRune(s string) (rune, bool) {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || size != len(s) {
		return 0, false
	}
	return r, true
}
