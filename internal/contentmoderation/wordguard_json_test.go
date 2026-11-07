package contentmoderation

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// pieces of a JSON string literal, in raw form. They cover plain and escaped
// spellings of the same characters, characters outside the BMP written both
// ways, and malformed surrogates.
var literalPieces = []string{
	"a", "Z", " ", "bad", "word", "\u8fdd",
	"\u7981", "\u00e9", "\U0001f600", "\U0002a700", `\"`, `\\`,
	`\/`, `\n`, `\t`, `\r`, `\b`, `\f`,
	`\u0041`, `\u00e9`, `\u4f60`, `\u200b`, `\u0000`, `\u001f`,
	`\u2028`, `\ud83d\ude00`, `\uD83D\uDE00`, `\ud83d`, `\ude00`, `\ud83dx`,
	`\ud83d\u0041`, `\ude00\ud83d`, "\u2028", "\u007f", "\ufeff",
}

func randomLiteral(random *rand.Rand, maxPieces int) []byte {
	var b strings.Builder
	b.WriteByte('"')
	for n := random.Intn(maxPieces + 1); n > 0; n-- {
		b.WriteString(literalPieces[random.Intn(len(literalPieces))])
	}
	b.WriteByte('"')
	return []byte(b.String())
}

// The decoded text must be exactly what gjson reports, malformed surrogates
// included: the extractor matches on gjson's text and splices using ours.
func TestDecodeJSONLiteralMatchesGJSON(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for i := 0; i < 30000; i++ {
		lit := randomLiteral(random, 10)
		if !gjson.ValidBytes(lit) {
			continue
		}
		want := gjson.ParseBytes(lit).String()
		got, ok := decodeJSONLiteral(lit)
		if !ok || got != want {
			t.Fatalf("decode(%s) = %+q, %v; gjson gives %+q", lit, got, ok, want)
		}
	}
}

func TestDecodeJSONLiteralKeepsBytesThatAreNotUTF8(t *testing.T) {
	lit := []byte("\"ok \xff\xfe bad\"")
	got, ok := decodeJSONLiteral(lit)
	if !ok || got != "ok \xff\xfe bad" || got != gjson.ParseBytes(lit).String() {
		t.Fatalf("decode = %+q, %v (gjson %+q)", got, ok, gjson.ParseBytes(lit).String())
	}
}

func TestDecodeJSONLiteralRejectsBrokenLiterals(t *testing.T) {
	for _, lit := range []string{``, `"`, `abc`, `"abc`, `"a\"`, `"a\x"`, `"\u12"`, `"\u12zz"`, "\"a\x01b\"", `"\`} {
		if got, ok := decodeJSONLiteral([]byte(lit)); ok {
			t.Errorf("decode(%q) = %q, want it refused", lit, got)
		}
	}
}

// Offsets of decoded text map back to the raw position of the same character.
func TestLiteralOffsetsLandOnCharacterBoundaries(t *testing.T) {
	random := rand.New(rand.NewSource(2))
	for i := 0; i < 20000; i++ {
		lit := randomLiteral(random, 8)
		if !gjson.ValidBytes(lit) || hasMalformedSurrogate(lit) {
			continue
		}
		text, ok := decodeJSONLiteral(lit)
		if !ok {
			t.Fatalf("decode(%s) failed", lit)
		}
		// Every offset between characters, plus the end.
		var offsets []int
		for i := 0; i < len(text); {
			offsets = append(offsets, i)
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
		}
		offsets = append(offsets, len(text))
		raw, ok := literalOffsets(lit, offsets)
		if !ok || len(raw) != len(offsets) {
			t.Fatalf("literalOffsets(%s, %v) = %v, %v", lit, offsets, raw, ok)
		}
		for k, o := range offsets {
			prefix := append(append([]byte{}, lit[:raw[k]]...), '"')
			got, ok := decodeJSONLiteral(prefix)
			// A malformed surrogate decodes to U+FFFD, three bytes for one escape,
			// so compare on the text before the offset only when it is clean.
			if !ok || got != text[:o] {
				t.Fatalf("literal %s: raw offset %d for decoded offset %d decodes to %+q, want %+q", lit, raw[k], o, got, text[:o])
			}
		}
	}
}

func TestLiteralOffsetsRefusesOffsetsInsideACharacter(t *testing.T) {
	lit := []byte(`"a你b😀"`)
	for _, offsets := range [][]int{{2}, {6}, {1, 3}, {2, 1}, {99}} {
		if got, ok := literalOffsets(lit, offsets); ok {
			t.Errorf("literalOffsets(%v) = %v, want refused", offsets, got)
		}
	}
	if got, ok := literalOffsets(lit, []int{0, 1, 4, 5}); !ok || fmt.Sprint(got) != "[1 2 5 6]" {
		t.Fatalf("literalOffsets = %v, %v", got, ok)
	}
}

func TestSpliceLiteralKeepsEveryOtherByte(t *testing.T) {
	lit := []byte(`"say bad\u200bword \/ now 😀 bad"`)
	text, _ := decodeJSONLiteral(lit)
	if text != "say bad\u200bword / now 😀 bad" {
		t.Fatalf("text = %+q", text)
	}
	// Replace "bad<ZWSP>word" (decoded bytes 4..14) and the last "bad".
	first := strings.Index(text, "bad\u200bword")
	last := strings.LastIndex(text, "bad")
	out, ok := spliceLiteral(lit, []literalEdit{
		{first, first + len("bad\u200bword"), "***"},
		{last, last + 3, "[x]"},
	})
	if !ok {
		t.Fatal("splice failed")
	}
	want := `"say *** \/ now 😀 [x]"`
	if string(out) != want {
		t.Fatalf("out = %s, want %s", out, want)
	}
}

func TestSpliceLiteralEscapesTheReplacement(t *testing.T) {
	lit := []byte(`"a BAD b"`)
	out, ok := spliceLiteral(lit, []literalEdit{{2, 5, "\"quoted\"\n\\ \u2028 é\x01"}})
	if !ok {
		t.Fatal("splice failed")
	}
	if want := `"a \"quoted\"\n\\ \u2028 é\u0001 b"`; string(out) != want {
		t.Fatalf("out = %s, want %s", out, want)
	}
	if !json.Valid(out) {
		t.Fatalf("not valid JSON: %s", out)
	}
	if got, _ := decodeJSONLiteral(out); got != "a \"quoted\"\n\\ \u2028 é\x01 b" {
		t.Fatalf("decoded = %+q", got)
	}
}

func TestSpliceLiteralRandom(t *testing.T) {
	random := rand.New(rand.NewSource(4))
	for i := 0; i < 20000; i++ {
		lit := randomLiteral(random, 10)
		if !gjson.ValidBytes(lit) {
			continue
		}
		text, _ := decodeJSONLiteral(lit)
		// Pick up to three ordered, non-overlapping character ranges.
		var starts []int
		for k := 0; k < len(text); {
			starts = append(starts, k)
			_, size := utf8.DecodeRuneInString(text[k:])
			k += size
		}
		starts = append(starts, len(text))
		var edits []literalEdit
		want := ""
		cursor := 0
		for n := random.Intn(4); n > 0 && cursor < len(starts)-1; n-- {
			a := cursor + random.Intn(len(starts)-cursor)
			b := a + random.Intn(len(starts)-a)
			if a == b {
				continue
			}
			repl := []string{"***", "", "é\"\\", "😀"}[random.Intn(4)]
			want += text[starts[cursor]:starts[a]] + repl
			edits = append(edits, literalEdit{starts[a], starts[b], repl})
			cursor = b
		}
		want += text[starts[cursor]:]
		out, ok := spliceLiteral(lit, edits)
		if hasMalformedSurrogate(lit) {
			if ok {
				t.Fatalf("splice(%s, %v) = %s: a literal with a malformed surrogate must be refused", lit, edits, out)
			}
			continue
		}
		if !ok {
			t.Fatalf("splice(%s, %v) failed", lit, edits)
		}
		if !json.Valid(out) {
			t.Fatalf("splice(%s, %v) = %s: not valid JSON", lit, edits, out)
		}
		got, ok := decodeJSONLiteral(out)
		if !ok || got != want {
			t.Fatalf("splice(%s, %v) = %s decodes to %+q, want %+q", lit, edits, out, got, want)
		}
	}
}

func TestAppendJSONEscapedRoundTrips(t *testing.T) {
	random := rand.New(rand.NewSource(9))
	pool := []string{"a", "\"", "\\", "\n", "\r", "\t", "\x00", "\x1f", "\x7f", "é", "违", "😀", "\u2028", "\u2029", "/", "<", ">", "&", "\ufeff"}
	for i := 0; i < 5000; i++ {
		var b strings.Builder
		for n := random.Intn(12); n > 0; n-- {
			b.WriteString(pool[random.Intn(len(pool))])
		}
		s := b.String()
		lit := append(append([]byte{'"'}, appendJSONEscaped(nil, s)...), '"')
		if !json.Valid(lit) {
			t.Fatalf("%+q encodes to invalid JSON %s", s, lit)
		}
		if got, ok := decodeJSONLiteral(lit); !ok || got != s {
			t.Fatalf("%+q round trips to %+q", s, got)
		}
		var viaStdlib string
		if err := json.Unmarshal(lit, &viaStdlib); err != nil || viaStdlib != s {
			t.Fatalf("encoding/json reads %s as %+q, %v", lit, viaStdlib, err)
		}
	}
	// Characters that need no escape stay as they are.
	if got := string(appendJSONEscaped(nil, "违禁/é")); got != "违禁/é" {
		t.Fatalf("got %s", got)
	}
}

func hasMalformedSurrogate(lit []byte) bool {
	end := len(lit) - 1
	for i := 1; i < end; {
		step, ok := nextLiteralStep(lit, i, end)
		if !ok {
			return false
		}
		if step.malformed {
			return true
		}
		i = step.rawEnd
	}
	return false
}

// A lone surrogate escape followed by another \u escape is read as a pair by
// gjson and as two characters by other decoders. Removing what separates them
// would silently change the text after it, so such a literal is not edited.
func TestSpliceLiteralRefusesMalformedSurrogates(t *testing.T) {
	for _, lit := range []string{`"\ude00\b\u00e9"`, `"x \ud83d y"`, `"\ud83d\u0041"`, `"bad \ude00"`} {
		if out, ok := spliceLiteral([]byte(lit), []literalEdit{{0, 1, "*"}}); ok {
			t.Errorf("splice(%s) = %s, want refused", lit, out)
		}
	}
	// A valid pair is fine.
	if out, ok := spliceLiteral([]byte(`"\ud83d\ude00 bad"`), []literalEdit{{5, 8, "***"}}); !ok || string(out) != `"\ud83d\ude00 ***"` {
		t.Fatalf("splice = %s, %v", out, ok)
	}
}
