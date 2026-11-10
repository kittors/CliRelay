package contentmoderation

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// The walk that finds many places at once (jsonPathsAt, unitFields) is held to
// the walk it replaced, place by place: referencePathAt below is jsonPathAt as
// it was, one walk per place, strings read byte by byte, every key decoded as
// it is crossed. Whatever the old walk said of a place, including nothing, the
// new one must say too.

type referenceStep struct {
	key   string
	index int
	isKey bool
}

type referenceFinder struct {
	data   []byte
	target int
	path   []referenceStep
	found  bool
}

func referencePathAt(data []byte, offset int) string {
	finder := &referenceFinder{data: data, target: offset}
	finder.value(finder.skip(0), 0)
	if !finder.found {
		return ""
	}
	var b strings.Builder
	for _, step := range finder.path {
		switch {
		case !step.isKey:
			b.WriteString("[" + strconv.Itoa(step.index) + "]")
		case isPlainPathKey(step.key):
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(step.key)
		default:
			b.WriteString("[" + strconv.Quote(step.key) + "]")
		}
	}
	return b.String()
}

func (f *referenceFinder) skip(i int) int {
	for i < len(f.data) && (f.data[i] == ' ' || f.data[i] == '\t' || f.data[i] == '\n' || f.data[i] == '\r') {
		i++
	}
	return i
}

func (f *referenceFinder) stringEnd(i int) int {
	for j := i + 1; j < len(f.data); j++ {
		switch f.data[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

func (f *referenceFinder) value(i, depth int) int {
	i = f.skip(i)
	if f.found || i >= len(f.data) || depth > maxJSONPathDepth {
		return -1
	}
	if i == f.target {
		f.found = true
		return -1
	}
	switch f.data[i] {
	case '"':
		return f.stringEnd(i)
	case '{':
		return f.object(i+1, depth)
	case '[':
		return f.array(i+1, depth)
	default:
		for i < len(f.data) && !strings.ContainsRune(",]} \t\r\n", rune(f.data[i])) {
			i++
		}
		return i
	}
}

func (f *referenceFinder) object(i, depth int) int {
	if i = f.skip(i); i < len(f.data) && f.data[i] == '}' {
		return i + 1
	}
	for {
		i = f.skip(i)
		if i >= len(f.data) || f.data[i] != '"' {
			return -1
		}
		end := f.stringEnd(i)
		if end < 0 {
			return -1
		}
		name, _ := decodeJSONLiteral(f.data[i:end])
		f.path = append(f.path, referenceStep{key: name, isKey: true})
		if i == f.target {
			f.found = true
			return -1
		}
		if i = f.skip(end); i >= len(f.data) || f.data[i] != ':' {
			return -1
		}
		if i = f.value(i+1, depth+1); i < 0 {
			return -1
		}
		f.path = f.path[:len(f.path)-1]
		if i = f.skip(i); i >= len(f.data) {
			return -1
		}
		switch f.data[i] {
		case ',':
			i++
		case '}':
			return i + 1
		default:
			return -1
		}
	}
}

func (f *referenceFinder) array(i, depth int) int {
	if i = f.skip(i); i < len(f.data) && f.data[i] == ']' {
		return i + 1
	}
	for index := 0; ; index++ {
		f.path = append(f.path, referenceStep{index: index})
		if i = f.value(i, depth+1); i < 0 {
			return -1
		}
		f.path = f.path[:len(f.path)-1]
		if i = f.skip(i); i >= len(f.data) {
			return -1
		}
		switch f.data[i] {
		case ',':
			i++
		case ']':
			return i + 1
		default:
			return -1
		}
	}
}

// referenceUnitField is unitField as it was: a walk of the unit's document,
// and one of each document above it.
func referenceUnitField(unit wordUnit) string {
	if unit.doc == nil {
		return ""
	}
	field := joinJSONPath(referenceDocPath(unit.doc), referencePathAt(unit.doc.data, unit.lit0))
	if field != "" && unit.kind == unitKey {
		field += " (an object key)"
	}
	return field
}

func referenceDocPath(d *wordDoc) string {
	if d.parent == nil {
		return ""
	}
	return joinJSONPath(referenceDocPath(d.parent), referencePathAt(d.parent.data, d.lit0))
}

// codexBody is a Codex request of turns turns, a call each, with screenshots
// of imageBytes.
func codexBody(turns, imageBytes int) string {
	body, _ := codexScreenshotRequest(turns, 1, imageBytes, true)
	return string(body)
}

// randomJSON writes a document with what trips a walk up: keys and strings that
// end in backslashes or hold escaped quotes, \u escapes, keys a path quotes,
// white space anywhere, a run of base64 like a screenshot's.
func randomJSON(rng *rand.Rand, b *strings.Builder, depth int) {
	strs := []string{`"plain"`, `""`, `"a\\"`, `"\\\\"`, `"q\"x"`, `"\\\""`, `"\u0041b"`, `"x y"`, `"k-1"`,
		`"2x"`, `"中文"`, `"tab\there"`, `"a\\\\\\\"b\\\\"`, `"` + strings.Repeat("iVBORw0KGgo", 6) + `"`}
	space := func() {
		if rng.Intn(4) == 0 {
			b.WriteString([]string{" ", "\n", "\t", "\r\n  "}[rng.Intn(4)])
		}
	}
	space()
	switch k := rng.Intn(10); {
	case depth > 0 && k < 3:
		b.WriteByte('{')
		for n, i := rng.Intn(4), 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			space()
			b.WriteString(strs[rng.Intn(len(strs))])
			space()
			b.WriteByte(':')
			randomJSON(rng, b, depth-1)
		}
		space()
		b.WriteByte('}')
	case depth > 0 && k < 6:
		b.WriteByte('[')
		for n, i := rng.Intn(4), 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			randomJSON(rng, b, depth-1)
		}
		space()
		b.WriteByte(']')
	case k < 8:
		b.WriteString(strs[rng.Intn(len(strs))])
	default:
		b.WriteString([]string{"0", "-12.5e3", "true", "false", "null"}[rng.Intn(5)])
	}
	space()
}

// checkPathsAgainstReference finds the paths of offsets in data in one walk,
// and each one again with the reference walk.
func checkPathsAgainstReference(t *testing.T, name string, data []byte, offsets []int) {
	t.Helper()
	got := jsonPathsAt(data, offsets)
	for k, offset := range offsets {
		if want := referencePathAt(data, offset); got[k] != want {
			t.Fatalf("%s: offset %d of %q: path %q, the walk per place says %q", name, offset, data, got[k], want)
		}
	}
}

// checkEveryOffset checks every offset of data at once, in a shuffled order
// with some offsets twice, and then a few small sets of them, where the walk
// stops early.
func checkEveryOffset(t *testing.T, rng *rand.Rand, name string, data []byte) {
	t.Helper()
	var all []int
	for offset := -1; offset <= len(data)+1; offset++ {
		all = append(all, offset)
		if rng.Intn(5) == 0 {
			all = append(all, offset)
		}
	}
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	checkPathsAgainstReference(t, name, data, all)
	for range 20 {
		some := make([]int, 1+rng.Intn(4))
		for i := range some {
			some[i] = rng.Intn(len(data) + 2)
		}
		checkPathsAgainstReference(t, name, data, some)
	}
}

func TestJSONPathsAtMatchesAWalkPerPlace(t *testing.T) {
	rng := rand.New(rand.NewSource(20261110))
	fixed := map[string]string{
		"nested":          `{"a":{"b":[1,{"c":"x"}]}}`,
		"escaped keys":    `{"q\"uote":"a \"x\" b","bad":{"\\":"\\\\","a\\\"":["\\\"",{"e":"\u0022"}]}}`,
		"white space":     "{ \"a\" :\n\t[ 1 ,\r\n { \"c\" : \"x\" } ] }",
		"literals":        `{"t":true,"f":false,"n":null,"v":"x","num":-1.5e10}`,
		"empty":           `{"a":{},"b":[],"c":[{},[]]}`,
		"top-level array": `[[1,2],[3,"z"],"w"]`,
		"top-level value": `"just a string"`,
		"cut short":       `{"a":{"b":["x","y`,
		"missing colon":   `{"a" 1,"b":2}`,
		"stray bytes":     `{"a":1}}],"b"`,
		"not a document":  `hello`,
		"deep arrays":     strings.Repeat("[", maxJSONPathDepth+3) + `"x"` + strings.Repeat("]", maxJSONPathDepth+3),
		"deep objects":    strings.Repeat(`{"k":`, maxJSONPathDepth+3) + `"x"` + strings.Repeat("}", maxJSONPathDepth+3),
		"claude request":  string(claudeHistory()),
		"chat request":    codexBody(2, 256),
		"tool arguments":  `{"command":["bash","-lc","curl -H 'Authorization: Bearer x'"],"workdir":"/repo"}`,
	}
	for name, doc := range fixed {
		checkEveryOffset(t, rng, name, []byte(doc))
	}
	for i := 0; i < 200; i++ {
		var b strings.Builder
		randomJSON(rng, &b, 4)
		if b.Len() > 1500 {
			continue
		}
		doc := []byte(b.String())
		checkEveryOffset(t, rng, "generated", doc)
		if i%20 == 0 && len(doc) < 400 {
			// every way of cutting the document short
			for cut := 0; cut < len(doc); cut++ {
				checkEveryOffset(t, rng, "generated, cut short", doc[:cut])
			}
		}
	}
}

func TestJSONPathsAtWithNoPlaces(t *testing.T) {
	if got := jsonPathsAt([]byte(`{"a":1}`), nil); len(got) != 0 {
		t.Fatalf("paths = %q", got)
	}
}

// Every unit of a request gets the field the walk per unit gave it, whatever
// document it sits in and in whatever order the units are asked for.
func TestUnitFieldsMatchAWalkPerUnit(t *testing.T) {
	deep := strings.Repeat(`{"a":`, maxJSONTextDepth+2) + `"` + testOpenAIKey + `"` + strings.Repeat(`}`, maxJSONTextDepth+2)
	requests := []struct {
		name   string
		format sdktranslator.Format
		body   string
	}{
		{"claude", sdktranslator.FormatClaude, string(claudeHistory())},
		{"chat with tool calls", sdktranslator.FormatOpenAI, codexBody(3, 128)},
		{"chat arguments with keys to quote", sdktranslator.FormatOpenAI, string(chatRequestWithToolCall("hi",
			`{"a b":{"list":["x",{"deep key":"`+testOpenAIKey+`"}]},"q\"k":"v","`+testAWSKey+`":1}`))},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"model":"m","instructions":"use ` + testOpenAIKey + `","input":[` +
			`{"type":"function_call","call_id":"c","name":"f","arguments":` + quoteJSON(`{"k":["`+testAWSKey+`"]}`) + `},` +
			`{"type":"function_call_output","call_id":"c","output":"ok ` + testAnthropicKey + `"}]}`},
		{"gemini", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"key ` + testOpenAIKey + `"},` +
			`{"functionCall":{"name":"f","args":{"id":"` + testAWSKey + `","nested":{"x":["y"]}}}}]}]}`},
		{"too deep to rewrite", sdktranslator.FormatClaude, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":` + deep + `}]}]}`},
	}
	rng := rand.New(rand.NewSource(7))
	for _, r := range requests {
		for _, extract := range []func(sdktranslator.Format, []byte) []wordUnit{extractSecretUnits, extractWordUnits} {
			units := extract(r.format, []byte(r.body))
			if len(units) == 0 {
				t.Fatalf("%s: no units", r.name)
			}
			rng.Shuffle(len(units), func(i, j int) { units[i], units[j] = units[j], units[i] })
			fields := unitFields(units)
			for i, unit := range units {
				want := referenceUnitField(unit)
				if fields[i] != want {
					t.Fatalf("%s: unit %q: field %q, the walk per unit says %q", r.name, unit.text, fields[i], want)
				}
				if one := unitField(unit); one != want {
					t.Fatalf("%s: unit %q: unitField %q, the walk per unit says %q", r.name, unit.text, one, want)
				}
			}
		}
	}
}
