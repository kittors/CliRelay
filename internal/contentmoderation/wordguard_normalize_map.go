package contentmoderation

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// foldedText is a text in the form keyword matching compares in (see
// normalizeForMatch), together with a record of what each of its characters
// came from.
//
// Keyword and regex rules are matched against the normalized text, so a hit is
// a byte range of that text. Replacing it has to happen in the original, where
// normalization has moved every offset: zero-width characters are gone, a
// full-width letter is one byte shorter, a ligature has become three letters.
// The record is what makes that possible.
type foldedText struct {
	text string
	// from[i] and to[i] bound, as byte offsets into the original, the
	// characters the i-th rune of text was made from. Runes made from one
	// original character (or one NFKC segment, such as a letter and its
	// combining mark) share a range, so a hit that covers part of such a
	// group maps to the whole group: a replacement never splits a character.
	// A collapsed run of whitespace is one space whose range is the whole run.
	from, to []int32
	// exact is false when the original is not valid UTF-8. The steps treat
	// invalid bytes in ways that depend on their neighbours, so no range can be
	// trusted and callers must not rewrite such a text.
	exact bool
}

// textSpan is a byte range of a text.
type textSpan struct{ start, end int }

// normalizeMapped is normalizeForMatch that also records where every
// character came from. It mirrors normalizeForMatch step for step, including
// its shortcut for simple words, so the text it returns is the same string;
// TestNormalizeMappedMatchesNormalizeForMatch holds the two to that.
//
// It is meant for texts a rule has already matched: it allocates two offsets
// per character, which plain matching does not need.
func normalizeMapped(s string, foldTraditional bool) foldedText {
	var table map[rune]rune
	if foldTraditional {
		table = traditionalToSimplified()
	}
	out := mappedCollapser{exact: utf8.ValidString(s)}
	out.b.Grow(len(s))
	pending := -1 // start of the words waiting to take every step
	for i := 0; i < len(s); {
		if isASCIISpace(s[i]) {
			if pending < 0 {
				out.space(i, i+1)
			}
			i++
			continue
		}
		end, simple := scanWord(s, i)
		switch {
		case !simple:
			if pending < 0 {
				pending = i
			}
		case pending >= 0:
			out.writeSteps(s, pending, i, table)
			pending = -1
			fallthrough
		default:
			out.writeSimple(s, i, end, table)
		}
		i = end
	}
	if pending >= 0 {
		out.writeSteps(s, pending, len(s), table)
	}
	return foldedText{text: out.b.String(), from: out.from, to: out.to, exact: out.exact}
}

// originalSpans converts byte spans of f.text into byte spans of the original
// text. The input must be ascending and made of whole runes of f.text. Spans
// that touch the same original character come back overlapping or equal;
// merging them is the caller's business.
func (f foldedText) originalSpans(spans []textSpan) ([]textSpan, bool) {
	out := make([]textSpan, 0, len(spans))
	rune0, byte0 := 0, 0 // rune index and byte offset reached so far
	advance := func(target int) (int, bool) {
		for byte0 < target && byte0 < len(f.text) {
			_, size := utf8.DecodeRuneInString(f.text[byte0:])
			byte0 += size
			rune0++
		}
		return rune0, byte0 == target
	}
	for _, span := range spans {
		if span.end <= span.start || span.end > len(f.text) {
			return nil, false
		}
		first, ok := advance(span.start)
		if !ok {
			return nil, false
		}
		last, ok := advance(span.end)
		if !ok || last <= first || last > len(f.from) {
			return nil, false
		}
		out = append(out, textSpan{start: int(f.from[first]), end: int(f.to[last-1])})
	}
	return out, true
}

// mappedCollapser is spaceCollapser with provenance: it builds the same text
// and keeps, per rune, the original range the rune came from.
type mappedCollapser struct {
	b        strings.Builder
	from, to []int32
	exact    bool
	pending  bool
	// pendFrom and pendTo bound the whitespace run waiting to be written as
	// one space: from the start of its first character to the end of its last.
	pendFrom, pendTo int
}

// space records whitespace found at s[from:to]. Whitespace before any text is
// dropped, as spaceCollapser does.
func (c *mappedCollapser) space(from, to int) {
	if c.b.Len() == 0 {
		return
	}
	if !c.pending {
		c.pending, c.pendFrom = true, from
	}
	c.pendTo = to
}

// emit writes r, made from s[from:to], after the space that was waiting.
func (c *mappedCollapser) emit(r rune, from, to int) {
	if c.pending {
		c.b.WriteByte(' ')
		c.from = append(c.from, int32(c.pendFrom))
		c.to = append(c.to, int32(c.pendTo))
		c.pending = false
	}
	c.b.WriteRune(r)
	c.from = append(c.from, int32(from))
	c.to = append(c.to, int32(to))
}

// writeSimple is spaceCollapser.writeSimple for the word s[lo:hi]: every rune
// is mapped on its own, so each has a range of its own.
func (c *mappedCollapser) writeSimple(s string, lo, hi int, table map[rune]rune) {
	for i := lo; i < hi; {
		b := s[i]
		start, size := i, 1
		var r rune
		if b >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(s[i:hi])
			i += size
			if r < 0xFF01 || r > 0xFF5E {
				if simplified, ok := table[r]; ok {
					r = simplified
				}
				c.emit(r, start, i)
				continue
			}
			b = byte(r - 0xFEE0)
		} else {
			i++
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		c.emit(rune(b), start, i)
	}
}

// writeSteps is writeText(normalizeSteps(s[lo:hi])) with provenance.
//
// normalizeSteps works on the whole region, but only NFKC looks across
// characters, and it does so within what norm calls a segment: a starter and
// what composes with it. Cutting the region at segment boundaries and taking
// the steps per segment therefore gives the same text, and each segment's
// output can be attributed to the original characters it was made from.
func (c *mappedCollapser) writeSteps(s string, lo, hi int, table map[rune]rune) {
	// Step one: drop default-ignorable characters, remembering where every
	// kept rune sat. Ignorables have no output of their own; they only matter
	// when a hit spans them, and then the range between its ends covers them.
	kept := make([]byte, 0, hi-lo)
	starts := make([]int32, 0, hi-lo)
	ends := make([]int32, 0, hi-lo)
	for i := lo; i < hi; {
		r, size := utf8.DecodeRuneInString(s[i:hi])
		if !isIgnorable(r) {
			kept = append(kept, s[i:i+size]...)
			starts = append(starts, int32(i))
			ends = append(ends, int32(i+size))
		}
		i += size
	}
	text := string(kept)
	for pos, ri := 0, 0; pos < len(text); {
		n := norm.NFKC.NextBoundaryInString(text[pos:], true)
		if n <= 0 {
			n = len(text) - pos
		}
		segment := text[pos : pos+n]
		count := utf8.RuneCountInString(segment)
		from, to := int(starts[ri]), int(ends[ri+count-1])
		if n == 1 && segment[0] < utf8.RuneSelf {
			// The common case, and the one worth keeping cheap: an ASCII
			// character changes only by case folding.
			b := segment[0]
			if isASCIISpace(b) {
				c.space(from, to)
			} else {
				if 'A' <= b && b <= 'Z' {
					b += 'a' - 'A'
				}
				c.emit(rune(b), from, to)
			}
		} else {
			for _, r := range normalizeSteps(segment, table) {
				if unicode.IsSpace(r) {
					c.space(from, to)
				} else {
					c.emit(r, from, to)
				}
			}
		}
		pos += n
		ri += count
	}
}
