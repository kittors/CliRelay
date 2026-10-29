// Package secretscrub removes credentials from text before it is kept: the
// request and response bodies and diagnostics stored with each request log,
// and the request log files. Agents read .env files, configs and shell history
// into their prompts, and models repeat what they read, so without this every
// key a client's tools touched would sit in plain text in the usage database,
// readable by the tenant's admins in the panel and copied into every backup.
//
// Only what is recognised with high confidence is replaced, so the stored
// request still reads as it was sent and stays useful for troubleshooting:
//
//   - credential formats with a fixed prefix (sk-, ghp_, AIza, xoxb-, AKIA, …),
//     JSON Web Tokens and PEM private keys;
//   - passwords in URLs (scheme://user:password@host) and Bearer and Basic
//     credentials;
//   - the value of a field whose name says it holds a password, secret, token
//     or key (DB_PASSWORD=…, "client_secret": "…", api_key: …), and the
//     Authorization and Cookie headers in request dumps;
//   - the same in Chinese, Japanese and Korean phrasing (密码：…, 令牌是…).
//
// Each is replaced by [REDACTED:<kind>]. The replacement is irreversible: the
// value is not kept anywhere, and nothing is sent upstream differently.
//
// The text is usually JSON, often JSON inside JSON strings (an upstream request
// body inside the diagnostics, tool-call arguments inside a message), so every
// replaced span starts and ends outside a JSON escape sequence and the
// replacement holds no quote or backslash: a valid document stays valid at any
// depth of nesting. Model output in a stored event stream arrives split across
// deltas; a credential the model repeats is replaced only where one event
// carries it whole.
package secretscrub

import (
	"sort"
	"strings"
)

// minScrubLen is shorter than any credential the rules recognise, together
// with whatever introduces it.
const minScrubLen = 8

// span is a range of text to replace, and what it held.
type span struct {
	start, end int
	kind       string
}

// Scrub returns text with every credential it recognises replaced by
// [REDACTED:<kind>]. Text without any is returned as it is, without copying.
func Scrub(text string) string {
	if len(text) < minScrubLen {
		return text
	}
	// A replacement can expose a credential that was glued to the one before
	// it (…-----END PRIVATE KEY-----sk-…), so scan the result again until it
	// stops changing; the rescan only happens when something was replaced.
	for pass := 0; pass < maxPasses; pass++ {
		s := scanner{text: text}
		s.scanFormats()
		s.scanNames()
		if len(s.spans) == 0 {
			break
		}
		text = s.apply()
	}
	return text
}

// maxPasses bounds the rescans; each pass only shortens what is left.
const maxPasses = 4

// ScrubBytes is Scrub for a byte slice. It returns data itself when nothing
// is replaced.
func ScrubBytes(data []byte) []byte {
	if len(data) < minScrubLen {
		return data
	}
	text := string(data)
	scrubbed := Scrub(text)
	if len(scrubbed) == len(text) && scrubbed == text {
		return data
	}
	return []byte(scrubbed)
}

type scanner struct {
	text  string
	spans []span
}

func (s *scanner) add(start, end int, kind string) {
	if start < end {
		s.spans = append(s.spans, span{start: start, end: end, kind: kind})
	}
}

// apply writes the text with every span replaced; overlapping spans are
// merged and keep the kind of the one that starts first, the longest of
// those, or of those the one found first.
func (s *scanner) apply() string {
	sort.SliceStable(s.spans, func(i, j int) bool {
		if s.spans[i].start != s.spans[j].start {
			return s.spans[i].start < s.spans[j].start
		}
		return s.spans[i].end > s.spans[j].end
	})
	var out strings.Builder
	out.Grow(len(s.text))
	pos := 0
	for i := 0; i < len(s.spans); {
		current := s.spans[i]
		i++
		for i < len(s.spans) && s.spans[i].start < current.end {
			if s.spans[i].end > current.end {
				current.end = s.spans[i].end
			}
			i++
		}
		out.WriteString(s.text[pos:current.start])
		out.WriteString("[REDACTED:")
		out.WriteString(current.kind)
		out.WriteByte(']')
		pos = current.end
	}
	out.WriteString(s.text[pos:])
	return out.String()
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// isKeyChar reports whether c can continue a credential or identifier.
func isKeyChar(c byte) bool { return isAlnum(c) || c == '_' || c == '-' }

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func isBase64Char(c byte) bool { return isAlnum(c) || c == '+' || c == '/' || c == '=' }

// isEscapeLetter reports whether a backslash followed by c is a JSON escape
// that stands for a separator: a newline, tab and so on, or a quote.
func isEscapeLetter(c byte) bool {
	switch c {
	case 'n', 'r', 't', 'b', 'f', '"', '/':
		return true
	}
	return false
}

// startsToken reports whether a credential can start at i: the byte before it
// must not continue a longer token. A JSON escape such as \n counts as a
// separator, since in JSON-encoded text a key on its own line follows one (or
// \\n, \\\\n deeper in the nesting). A '+' or '/' ends a token unless it sits
// inside a longer base64 run, where a short match is a coincidence.
func startsToken(text string, i int) bool {
	if i == 0 {
		return true
	}
	c := text[i-1]
	switch {
	case c == '+' || c == '/' || c == '=':
		return !inBase64Run(text, i-1)
	case !isKeyChar(c):
		return true
	case i >= 2 && text[i-2] == '\\' && isEscapeLetter(c):
		return true
	case i >= 6 && text[i-6] == '\\' && text[i-5] == 'u' && isHex(text[i-4]) && isHex(text[i-3]) && isHex(text[i-2]) && isHex(c):
		return true
	}
	return false
}

// inBase64Run reports whether the 16 bytes up to and including i are all
// base64, as inside an inline image or file rather than in a URL or prose.
func inBase64Run(text string, i int) bool {
	if i < 16 {
		return false
	}
	for j := i - 15; j <= i; j++ {
		if !isBase64Char(text[j]) {
			return false
		}
	}
	return true
}

// endsToken reports whether a credential can end before j.
func endsToken(text string, j int) bool {
	return j >= len(text) || !isKeyChar(text[j])
}

// runEnd returns the end of the run of bytes from i that satisfy in, scanning
// at most limit bytes.
func runEnd(text string, i int, in func(byte) bool, limit int) int {
	end := i
	for end < len(text) && end-i < limit && in(text[end]) {
		end++
	}
	return end
}

// hasPrefixFold is strings.HasPrefix ignoring ASCII case; prefix is lower case.
func hasPrefixFold(text, prefix string) bool {
	if len(text) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := text[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}

// pairSet is a set of two-byte sequences: a scan checks the byte it stands on
// and the next against it before doing any other work.
type pairSet [256][256]bool

func (p *pairSet) has(a, b byte) bool { return p[a][b] }

func (p *pairSet) add(word string) { p[word[0]][word[1]] = true }

// addFold adds the first two bytes of a lower-case word in every case.
func (p *pairSet) addFold(word string) {
	upper := func(c byte) byte {
		if c >= 'a' && c <= 'z' {
			return c - ('a' - 'A')
		}
		return c
	}
	a, b := word[0], word[1]
	p[a][b], p[upper(a)][b], p[a][upper(b)], p[upper(a)][upper(b)] = true, true, true, true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
