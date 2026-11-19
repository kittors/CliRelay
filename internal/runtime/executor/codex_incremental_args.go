package executor

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// Incremental Codex requests: function call arguments.
//
// A function call's arguments are a JSON document, and clients that hand a call
// back do not always write it the way the model did. magpie, which offers Codex
// custom tools (apply_patch) to Chat Completions upstreams as functions taking
// {"input": …}, reads the input out of the model's arguments and, on the next
// turn, wraps it again with Go's json.Marshal (internal/gateway/ir.go
// parseArgs). json.Marshal escapes < > and & as six-character \u sequences the
// model never writes, so the arguments came back as different bytes holding the
// same value. In the first 16.5 hours of shadow traffic (2026-11-18/19), the
// user's own magpie sessions failed to match on 114 of the 116 turns after an
// apply_patch whose input held one of those characters, and on 1 of the 72
// after one whose input held none.
//
// So arguments are compared by the JSON value they hold: whitespace between
// tokens and the way each string is escaped do not count. Everything a tool
// could read differently still does: object keys keep their order, duplicate
// keys stay, numbers are compared as written (1.0 is not 1), and every string's
// characters must be the same. Arguments that are not valid JSON are compared
// byte for byte, as before.

// codexIncrementalArgumentsDigest identifies function call arguments by their
// JSON value. The two forms are kept apart by a leading tag, so no document
// shares a digest with arguments that are not JSON.
func codexIncrementalArgumentsDigest(args string) codexIncrementalDigest {
	h := sha256.New()
	if gjson.Valid(args) {
		_, _ = h.Write([]byte{'j'})
		if writeCodexCanonicalJSON(h, args) {
			return codexIncrementalDigestSum(h)
		}
		h.Reset()
	}
	_, _ = h.Write([]byte{'r'})
	_, _ = h.Write(codexIncrementalStringBytes(args))
	return codexIncrementalDigestSum(h)
}

// writeCodexCanonicalJSON writes the tokens of the valid JSON document doc to h:
// whitespace between tokens left out, every string unescaped and written as a
// quote, its length and its bytes (a quote starts no other token, and the
// length says where the string ends), every other token as written. Valid JSON
// never needs whitespace to keep two tokens apart, so what is written names one
// sequence of tokens and nothing else.
//
// It reports false for a string holding a surrogate escape that is not half of
// a pair: no character stands for it, and decoders fold every such escape into
// U+FFFD, which would make different arguments look the same.
func writeCodexCanonicalJSON(h hash.Hash, doc string) bool {
	var out []byte
	var str []byte
	flush := func() {
		if len(out) > 0 {
			_, _ = h.Write(out)
			out = out[:0]
		}
	}
	for i := 0; i < len(doc); {
		c := doc[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		case '"':
		default:
			out = append(out, c)
			i++
			if len(out) >= 4096 {
				flush()
			}
			continue
		}
		end, plain := codexJSONStringEnd(doc, i)
		if end < 0 {
			return false
		}
		value := doc[i+1 : end]
		if !plain {
			var ok bool
			if str, ok = appendCodexJSONUnescaped(str[:0], value); !ok {
				return false
			}
		}
		out = append(out, '"')
		length := len(value)
		if !plain {
			length = len(str)
		}
		out = binary.AppendUvarint(out, uint64(length))
		flush()
		if plain {
			_, _ = h.Write(codexIncrementalStringBytes(value))
		} else {
			_, _ = h.Write(str)
		}
		i = end + 1
	}
	flush()
	return true
}

// codexJSONStringEnd returns the index of the quote closing the string whose
// opening quote is at doc[start], and whether the string has no escapes.
func codexJSONStringEnd(doc string, start int) (int, bool) {
	plain := true
	for i := start + 1; i < len(doc); i++ {
		switch doc[i] {
		case '"':
			return i, plain
		case '\\':
			plain = false
			i++
		}
	}
	return -1, false
}

// appendCodexJSONUnescaped appends the characters of the JSON string body s
// (between its quotes) to dst. It reports false for escapes it cannot stand
// behind: a lone surrogate, or anything gjson.Valid would not have let through.
func appendCodexJSONUnescaped(dst []byte, s string) ([]byte, bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			dst = append(dst, c)
			continue
		}
		i++
		if i >= len(s) {
			return dst, false
		}
		switch s[i] {
		case '"', '\\', '/':
			dst = append(dst, s[i])
		case 'b':
			dst = append(dst, '\b')
		case 'f':
			dst = append(dst, '\f')
		case 'n':
			dst = append(dst, '\n')
		case 'r':
			dst = append(dst, '\r')
		case 't':
			dst = append(dst, '\t')
		case 'u':
			r, ok := codexJSONHex4(s, i+1)
			if !ok {
				return dst, false
			}
			i += 4 // the last digit
			if utf16.IsSurrogate(r) {
				// Only a high half followed by an escaped low half is a
				// character: the six bytes after this digit must be one.
				if r >= 0xdc00 || i+6 >= len(s) || s[i+1] != '\\' || s[i+2] != 'u' {
					return dst, false
				}
				low, ok := codexJSONHex4(s, i+3)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return dst, false
				}
				r = utf16.DecodeRune(r, low)
				i += 6
			}
			dst = utf8.AppendRune(dst, r)
		default:
			return dst, false
		}
	}
	return dst, true
}

// codexJSONHex4 reads the four hex digits at s[at:].
func codexJSONHex4(s string, at int) (rune, bool) {
	if at+4 > len(s) {
		return 0, false
	}
	var r rune
	for k := at; k < at+4; k++ {
		c := s[k]
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}
