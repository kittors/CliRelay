package contentmoderation

import (
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Replacing a word has to change the request body and nothing else. The body
// is JSON the client built, and the rest of it (ids, signatures, encrypted
// reasoning, tool schemas, the exact escaping of every other string) must reach
// the upstream byte for byte. Re-encoding the document would not guarantee
// that, so a replacement is spliced into the raw bytes of the one string
// literal it falls in. These helpers read a literal the way gjson does (the
// extractor decoded it with gjson, so offsets must agree with its text) and
// remember where each decoded character sat in the raw literal.

// literalStep is one decoded character of a JSON string literal and the raw
// bytes it came from. An escape sequence is one step, and so is the pair of
// \u escapes that spell one character outside the BMP.
type literalStep struct {
	rawStart, rawEnd int
	text             [utf8.UTFMax]byte
	n                int
	// malformed marks a \u escape for half of a surrogate pair with no valid
	// other half. Decoders disagree about it (gjson pairs it with whatever \u
	// escape follows, others read U+FFFD and go on), so text next to one cannot
	// be edited without changing what the rest means.
	malformed bool
}

// nextLiteralStep decodes the character at lit[i]. end is the index of the
// closing quote. It mirrors gjson's unescape, including what it makes of a
// lone surrogate (U+FFFD) and of bytes that are not UTF-8 (kept as they are).
func nextLiteralStep(lit []byte, i, end int) (literalStep, bool) {
	step := literalStep{rawStart: i}
	c := lit[i]
	switch {
	case c == '\\':
		if i+1 >= end {
			return step, false
		}
		switch lit[i+1] {
		case '\\', '/', '"':
			step.text[0], step.n, step.rawEnd = lit[i+1], 1, i+2
		case 'b':
			step.text[0], step.n, step.rawEnd = '\b', 1, i+2
		case 'f':
			step.text[0], step.n, step.rawEnd = '\f', 1, i+2
		case 'n':
			step.text[0], step.n, step.rawEnd = '\n', 1, i+2
		case 'r':
			step.text[0], step.n, step.rawEnd = '\r', 1, i+2
		case 't':
			step.text[0], step.n, step.rawEnd = '\t', 1, i+2
		case 'u':
			r, ok := parseUnicodeEscape(lit, i+2, end)
			if !ok {
				return step, false
			}
			next := i + 6
			half := utf16.IsSurrogate(r)
			if half && next+6 <= end && lit[next] == '\\' && lit[next+1] == 'u' {
				// gjson takes the second escape whatever it holds, and so do we, so
				// that the two agree on a malformed pair.
				second, ok := parseUnicodeEscape(lit, next+2, end)
				if !ok {
					return step, false
				}
				r = utf16.DecodeRune(r, second)
				next += 6
			}
			// A half that did not pair up into a character beyond the BMP.
			step.malformed = half && r < 0x10000
			step.n = utf8.EncodeRune(step.text[:], r)
			step.rawEnd = next
		default:
			return step, false
		}
	case c < 0x20:
		return step, false // a raw control character is not valid JSON
	case c < utf8.RuneSelf:
		step.text[0], step.n, step.rawEnd = c, 1, i+1
	default:
		r, size := utf8.DecodeRune(lit[i:end])
		if r == utf8.RuneError && size == 1 {
			// Not UTF-8: gjson copies the byte, so the decoded text has it too.
			step.text[0], step.n, step.rawEnd = c, 1, i+1
		} else {
			copy(step.text[:], lit[i:i+size])
			step.n, step.rawEnd = size, i+size
		}
	}
	return step, true
}

func parseUnicodeEscape(lit []byte, from, end int) (rune, bool) {
	if from+4 > end {
		return 0, false
	}
	n, err := strconv.ParseUint(string(lit[from:from+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(n), true
}

// isLiteral reports whether lit looks like a complete JSON string literal.
func isLiteral(lit []byte) bool {
	return len(lit) >= 2 && lit[0] == '"' && lit[len(lit)-1] == '"'
}

// decodeJSONLiteral returns the text of a JSON string literal (quotes
// included in lit), as gjson would.
func decodeJSONLiteral(lit []byte) (string, bool) {
	if !isLiteral(lit) {
		return "", false
	}
	end := len(lit) - 1
	out := make([]byte, 0, len(lit))
	for i := 1; i < end; {
		step, ok := nextLiteralStep(lit, i, end)
		if !ok {
			return "", false
		}
		out = append(out, step.text[:step.n]...)
		i = step.rawEnd
	}
	return string(out), true
}

// literalOffsets maps byte offsets of a literal's decoded text to offsets in
// the literal itself: the offset returned for o is where the character that
// begins at decoded offset o starts in lit (for the end of the text, the
// closing quote). The offsets must be ascending and fall between characters.
func literalOffsets(lit []byte, offsets []int) ([]int, bool) {
	if !isLiteral(lit) {
		return nil, false
	}
	end := len(lit) - 1
	out := make([]int, 0, len(offsets))
	decoded, next := 0, 0
	for i := 1; ; {
		for next < len(offsets) && offsets[next] == decoded {
			out = append(out, i)
			next++
		}
		if i >= end {
			return out, next == len(offsets)
		}
		if next < len(offsets) && offsets[next] < decoded {
			return nil, false
		}
		step, ok := nextLiteralStep(lit, i, end)
		if !ok || step.malformed {
			// The whole literal is read, not only up to the last offset: a broken
			// escape anywhere makes the decoders disagree about its neighbours.
			return nil, false
		}
		decoded += step.n
		i = step.rawEnd
	}
}

// literalEdit replaces bytes [start, end) of a literal's decoded text.
type literalEdit struct {
	start, end int
	text       string
}

// spliceLiteral applies edits (ascending and not overlapping) to the literal
// lit and returns the new literal. Everything outside the edited ranges keeps
// its exact raw bytes, escapes included.
func spliceLiteral(lit []byte, edits []literalEdit) ([]byte, bool) {
	offsets := make([]int, 0, 2*len(edits))
	for _, edit := range edits {
		if edit.end < edit.start {
			return nil, false
		}
		offsets = append(offsets, edit.start, edit.end)
	}
	raw, ok := literalOffsets(lit, offsets)
	if !ok {
		return nil, false
	}
	out := make([]byte, 0, len(lit)+16*len(edits))
	copied := 0
	for i, edit := range edits {
		from, to := raw[2*i], raw[2*i+1]
		if from < copied {
			return nil, false
		}
		out = append(out, lit[copied:from]...)
		out = appendJSONEscaped(out, edit.text)
		copied = to
	}
	out = append(out, lit[copied:]...)
	return out, true
}

// appendJSONEscaped appends s escaped for the inside of a JSON string.
// Everything that is not required to be escaped stays as written, so a
// replacement in Chinese does not turn into a run of \u escapes.
func appendJSONEscaped(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		case c < utf8.RuneSelf:
			dst = append(dst, c)
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				// Cannot be written into JSON as it is.
				dst = append(dst, "\\ufffd"...)
			case r == '\u2028' || r == '\u2029':
				// Legal JSON, but a line terminator to some consumers.
				dst = append(dst, '\\', 'u', '2', '0', '2', hex[r&0xf])
			default:
				dst = append(dst, s[i:i+size]...)
			}
			i += size
			continue
		}
		i++
	}
	return dst
}
