package chat_completions

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/tidwall/sjson"
)

// TestWriteSJSONStringMatchesSJSON checks the strings of hand-written parts
// against what sjson.Set writes for the same value, which is what the
// upstream got before the parts were written by hand.
func TestWriteSJSONStringMatchesSJSON(t *testing.T) {
	values := []string{
		"", "a", `"`, `\`, "\x00", "\x1f", " ", "~", "\x7f", "\x80", "\xff",
		"é", "中文", "😀", "  ", "<b>&amp;</b>", "\xc3\x28 invalid",
		"line\nbreak\ttab", "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAA+/=",
	}
	// Each byte value at each position of the first two words and of the tail.
	for c := range 256 {
		for pos := range 19 {
			b := []byte(strings.Repeat("A", 19))
			b[pos] = byte(c)
			values = append(values, string(b[:pos+1]), string(b))
		}
	}
	const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
	rng := rand.New(rand.NewPCG(1, 2))
	for range 5000 {
		b := make([]byte, rng.IntN(64))
		for i := range b {
			if rng.IntN(32) == 0 {
				b[i] = byte(rng.IntN(256))
			} else {
				b[i] = base64Chars[rng.IntN(len(base64Chars))]
			}
		}
		values = append(values, string(b))
	}

	for _, v := range values {
		want, err := sjson.Set(`{}`, "v", v)
		if err != nil {
			t.Fatalf("sjson.Set(%q): %v", v, err)
		}
		var buf bytes.Buffer
		buf.WriteString(`{"v":`)
		writeSJSONString(&buf, v)
		buf.WriteByte('}')
		if buf.String() != want {
			t.Fatalf("value %q\n got: %s\nwant: %s", v, buf.String(), want)
		}
	}
}
