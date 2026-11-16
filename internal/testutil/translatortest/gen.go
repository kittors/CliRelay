package translatortest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
)

// gen holds the random source shared by the request generators of every
// protocol.
type gen struct{ r *rand.Rand }

// str returns a JSON string of n characters that need escaping now and then.
func (g gen) str(n int) string {
	const alphabet = "abcdefghij KLMNOP 0123 \"quote\" back\\slash\nnew line 中文 🙂 <&> json JSON"
	runes := []rune(alphabet)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(runes[g.r.Intn(len(runes))])
	}
	out, _ := json.Marshal(b.String())
	return string(out)
}

// text is str with a random length, sometimes empty.
func (g gen) text() string {
	if g.r.Intn(10) == 0 {
		return `""`
	}
	return g.str(1 + g.r.Intn(20))
}

func (g gen) pick(options ...string) string { return options[g.r.Intn(len(options))] }

func (g gen) chance(n int) bool { return g.r.Intn(n) == 0 }

// base64Data returns base64 text standing in for an image.
func (g gen) base64Data() string { return strings.Repeat("iVBORw0K", 1+g.r.Intn(30)) }

// claudeSignature returns a value shaped like an Anthropic thinking
// signature: base64 of a protobuf record starting with 0x12.
func (g gen) claudeSignature() string {
	raw := append([]byte{0x12}, bytes.Repeat([]byte{byte('a' + g.r.Intn(26))}, 40+g.r.Intn(40))...)
	return base64.StdEncoding.EncodeToString(raw)
}

// signature returns a thought signature or encrypted reasoning of a random
// family, length and shape.
func (g gen) signature() string {
	switch g.r.Intn(8) {
	case 0:
		return `""`
	case 1:
		return `"short"`
	case 2:
		return `"` + g.claudeSignature() + `"`
	case 3:
		return `"claude#` + g.claudeSignature() + `"`
	case 4:
		return `"claude-redacted-thinking:` + g.base64Data() + `"`
	case 5:
		return `"gAAAAA` + strings.Repeat("B", 60+g.r.Intn(40)) + `"`
	case 6:
		return `"skip_thought_signature_validator"`
	default:
		return `"` + strings.Repeat("S", 50+g.r.Intn(60)) + `"`
	}
}

// args returns tool call arguments as a JSON string, rarely one that is not
// valid JSON (see MatchReference).
func (g gen) args() string {
	if g.chance(40) {
		return `"{oops"`
	}
	return g.pick(`"{\"cmd\":[\"ls\"]}"`, `""`, `"[1,2]"`, `"{\"nested\":{\"a\":\"}\"}}"`, `"{\"a\":1} trailing"`, `"{}"`, `"null"`, `"{\"path\":\"a.go\",\"n\":3}"`)
}

// schema returns a JSON Schema for tool parameters, sometimes with upper-case
// type names as Gemini writes them.
func (g gen) schema() string {
	typ := g.pick(`"object"`, `"OBJECT"`, `"Object"`)
	return `{"type":` + typ + `,"properties":{"cmd":{"type":` + g.pick(`"array"`, `"ARRAY"`) + `,"items":{"type":` + g.pick(`"string"`, `"STRING"`) + `}},"flag":{"type":["boolean","null"]},"n":{},"type":{"type":"STRING","description":"a property named type"}},"required":["cmd"]` + g.pick(``, `,"$schema":"http://json-schema.org/draft-07/schema#"`, `,"additionalProperties":false`) + `}`
}

// indent re-indents body now and then: translators copy raw values, so the
// whitespace inside them must survive.
func (g gen) indent(body string) []byte {
	if g.chance(4) {
		var indented bytes.Buffer
		if err := json.Indent(&indented, []byte(body), "", "  "); err == nil {
			return indented.Bytes()
		}
	}
	return []byte(body)
}

func (g gen) id(prefix string) string {
	return fmt.Sprintf(`"%s%d"`, prefix, g.r.Intn(6))
}
