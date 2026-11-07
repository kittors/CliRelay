package contentmoderation

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A keyword rule may ask to be matched "ignoring separators" (WordRule's
// IgnoreSeparators). Text in Chinese, Japanese and Korean has no spaces between
// words, so a banned word is dodged just as well by writing it as 敏 感 词,
// 敏-感-词 or 敏。感，词, and nothing about the normal matching (which only
// collapses runs of white space into one space) sees through that. Such a rule
// is matched against the text with the separators taken out, and the match is
// then traced back to the text with them in it: 敏 感 词 is one hit, from the
// first character to the last, with the separators between them inside it.
//
// The option is off by default and is not offered for regular expressions, which
// can say `敏\W*感\W*词` themselves. It applies across word boundaries: with it on,
// "cat" also matches "pic at". That is what ignoring the spaces means, and it is
// why it is a choice made per rule rather than a setting of the whole policy.

// isWordSeparator reports whether r is skipped between the characters of a
// keyword that ignores separators: white space, control characters,
// punctuation and symbols (so "_", "-", ".", full-width and CJK marks, emoji
// and the like). Letters, digits and combining marks are never skipped.
func isWordSeparator(r rune) bool {
	if r < utf8.RuneSelf {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}
	return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// separatorRun records that a run of separators was taken out of a text.
type separatorRun struct {
	// at is the offset in the stripped text where the run was taken out, which is
	// where the character that followed it is.
	at int
	// removed is the number of bytes taken out so far, this run included.
	removed int
}

// separatorRuns lists the runs taken out of one text, in order.
type separatorRuns []separatorRun

// stripSeparators returns text without its separators, and where they were. A
// text with none is returned as it is, with no runs. Bytes that are not valid
// UTF-8 are kept: they are not separators, and keeping them keeps every offset
// of the result a place in the original.
func stripSeparators(text string) (string, separatorRuns) {
	first := -1
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if isWordSeparator(r) && (r != utf8.RuneError || size > 1) {
			first = i
			break
		}
		i += size
	}
	if first < 0 {
		return text, nil
	}
	var kept strings.Builder
	kept.Grow(len(text) - 1)
	kept.WriteString(text[:first])
	var runs separatorRuns
	removed := 0
	for i := first; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if isWordSeparator(r) && (r != utf8.RuneError || size > 1) {
			start := i
			for i < len(text) {
				r, size = utf8.DecodeRuneInString(text[i:])
				if !isWordSeparator(r) || (r == utf8.RuneError && size == 1) {
					break
				}
				i += size
			}
			removed += i - start
			runs = append(runs, separatorRun{at: kept.Len(), removed: removed})
			continue
		}
		kept.WriteString(text[i : i+size])
		i += size
	}
	return kept.String(), runs
}

// original maps an offset in the stripped text back to the text it came from.
//
// The two ends of a match are mapped differently. A match starts at a character,
// and the separators taken out just before that character are not part of it;
// a match ends after a character, and the separators taken out just after it are
// not part of it either. So a start offset counts the runs taken out at the
// offset, an end offset does not.
func (r separatorRuns) original(offset int, isStart bool) int {
	n := sort.Search(len(r), func(i int) bool {
		if isStart {
			return r[i].at > offset
		}
		return r[i].at >= offset
	})
	if n == 0 {
		return offset
	}
	return offset + r[n-1].removed
}
