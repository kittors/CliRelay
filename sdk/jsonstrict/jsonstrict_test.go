package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

// checkBoth runs Check and CheckReader one byte at a time, which cuts the body
// at every possible place, and fails the test when they disagree.
func checkBoth(t *testing.T, body string) error {
	t.Helper()
	whole := Check([]byte(body))
	streamed := CheckReader(iotest.OneByteReader(strings.NewReader(body)))
	if fmt.Sprint(whole) != fmt.Sprint(streamed) {
		t.Fatalf("%q: Check says %v, CheckReader one byte at a time says %v", body, whole, streamed)
	}
	return whole
}

func TestCheckAcceptsJSONTexts(t *testing.T) {
	for _, body := range []string{
		`{}`, `[]`, `""`, `0`, `-0`, `12.5e-3`, `1E+9`, `true`, `false`, `null`,
		" \t\r\n{\"a\" : [ 1 , 2 ] }\n",
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"user","content":"again"}]}`,
		// one name in different objects, and in an object and its member
		`[{"a":1},{"a":2}]`, `{"a":{"a":{"a":1}}}`,
		// escapes that decode to different names
		`{"a\n":1,"a\t":2,"\u00e9":3,"e\u0301":4,"éx":5}`,
		// lone surrogates are allowed, and two different ones are two names
		`{"\ud800":1,"\udbff":2,"\udc00":3,"x":"\ud83d"}`,
		// a pair, then the same high half alone
		`{"😀":1,"\ud83d":2}`,
		// control characters escaped, every escape
		`"\u0000\u001f\"\\\/\b\f\n\r\t"`,
		// bytes above ASCII as they are, valid UTF-8 or not
		"{\"k\":\"\xff\xfe\",\"\xc3\":1}",
	} {
		if err := checkBoth(t, body); err != nil {
			t.Errorf("%q: %v", body, err)
		}
	}
}

func TestCheckRefusesWhatParsersReadDifferently(t *testing.T) {
	for _, tc := range []struct{ body, path, reason string }{
		// read by Python's json and pydantic, by nothing else
		{`{"temperature":NaN}`, "$.temperature", `invalid character 'N'`},
		{`{"top_p":Infinity}`, "$.top_p", `invalid character 'I'`},
		{`{"top_p":-Infinity}`, "$.top_p", "invalid number"},
		{`[1,]`, "$[1]", "invalid character ']'"},
		{`{"a":1,}`, "$", "a comma before '}'"},
		{"\xef\xbb\xbf{}", "$", "invalid character 0xEF"},
		{`{'a':1}`, "$", "expected a member name"},
		{`{"a":'b'}`, "$.a", `invalid character '\''`},
		{`{"a":01}`, "$.a", "leading zero"},
		{`{"a":1.}`, "$.a", "invalid number"},
		{`{"a":.5}`, "$.a", `invalid character '.'`},
		{`{"a":+1}`, "$.a", `invalid character '+'`},
		{`{"a":1e}`, "$.a", "invalid number"},
		{`{"a":0x1}`, "$.a", "expected ',' or '}'"},
		{`{"a":tru}`, "$.a", "invalid literal"},
		{`{"a":truex}`, "$.a", "expected ',' or '}'"},
		{"{\"a\":\"\x01\"}", "$.a", "unescaped control character"},
		{`{"a":"\x"}`, "$.a", "invalid escape"},
		{`{"a":"\u12g4"}`, "$.a", `invalid \u escape`},
		{`{"a":1} {"a":2}`, "$", "data after the JSON value"},
		{`{"messages":[{"role":"user"}]`, "$.messages", "the body ends inside"},
		{`{"a":"open`, "$.a", "the body ends inside"},
		{``, "$", "no JSON value"},
		{" \n", "$", "no JSON value"},
		{`{"a" 1}`, "$.a", "expected ':'"},
		{`{"a":1 "b":2}`, "$.a", "expected ',' or '}'"},
		{`[1 2]`, "$[0]", "expected ',' or ']'"},
	} {
		err := checkBoth(t, tc.body)
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%q: got %v, want an *Error", tc.body, err)
			continue
		}
		if e.Path != tc.path || !strings.Contains(e.Reason, tc.reason) || e.Duplicate {
			t.Errorf("%q: path %q, reason %q; want %q and %q", tc.body, e.Path, e.Reason, tc.path, tc.reason)
		}
	}
}

func TestCheckRefusesARepeatedName(t *testing.T) {
	many := func(dup string) string {
		var b strings.Builder
		b.WriteString(`{"tools":[{"parameters":{"properties":{`)
		for i := range 40 {
			fmt.Fprintf(&b, `"p%d":{},`, i)
		}
		b.WriteString(`"` + dup + `":{}}}}]}`)
		return b.String()
	}
	for _, tc := range []struct{ body, path, name string }{
		{`{"model":"m","messages":[],"messages":[]}`, "$", "messages"},
		{`{"model":"cheap","model":"expensive"}`, "$", "model"},
		{`{"":0,"":1}`, "$", ""},
		{`{"messages":[{"role":"user","content":"hi","content":"bad"}]}`, "$.messages[0]", "content"},
		{`{"contents":[{"parts":[{"text":"a"},{"text":"b","text":"c"}]}]}`, "$.contents[0].parts[1]", "text"},
		// names are compared after their escapes are decoded
		{`{"mess\u0061ges":1,"messages":2}`, "$", "messages"},
		{`{"mod\u0065l":1,"model":2}`, "$", "model"},
		{`{"a\/b":1,"a/b":2}`, "$", "a/b"},
		{`{"\ud83d\ude00":1,"😀":2}`, "$", "😀"},
		{`{"\ud800":1,"\uD800":2}`, "$", "\xed\xa0\x80"},
		// past the list, in the set
		{many("p7"), "$.tools[0].parameters.properties", "p7"},
		{many("p39"), "$.tools[0].parameters.properties", "p39"},
		// a name with characters a JSONPath writes in brackets
		{`{"x":{"a b":1,"a b":2}}`, "$.x", "a b"},
		{`{"a b":{"c":1,"c":2}}`, "$['a b']", "c"},
	} {
		err := checkBoth(t, tc.body)
		var e *Error
		if !errors.As(err, &e) || !IsDuplicate(err) {
			t.Errorf("%.60q: got %v, want a repeated name", tc.body, err)
			continue
		}
		if e.Path != tc.path || e.Name != tc.name {
			t.Errorf("%.60q: path %q, name %q; want %q and %q", tc.body, e.Path, e.Name, tc.path, tc.name)
		}
	}
	if err := checkBoth(t, many("p40")); err != nil {
		t.Fatalf("41 different names: %v", err)
	}
}

func TestErrorPointsAtTheRepeatedName(t *testing.T) {
	body := `{"messages":[],"messages":[]}`
	err := Check([]byte(body))
	var e *Error
	if !errors.As(err, &e) || e.Offset != int64(strings.LastIndex(body, `"messages"`)) {
		t.Fatalf("got %+v", err)
	}
	if got := err.Error(); got != `the object at $ has two members named "messages" (byte 15); a name may appear only once in an object` {
		t.Fatalf("message = %q", got)
	}
}

func TestErrorCutsALongName(t *testing.T) {
	name := strings.Repeat("é", 100)
	err := Check([]byte(`{"x":{"` + name + `":1,"` + name + `":2}}`))
	var e *Error
	if !errors.As(err, &e) || e.Name != name {
		t.Fatalf("got %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, name) || !strings.Contains(msg, "…") || len(msg) > 200 {
		t.Fatalf("the message holds the whole name: %q", msg)
	}
}

func TestCheckBoundsNesting(t *testing.T) {
	deep := strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth)
	if err := Check([]byte(deep)); err != nil {
		t.Fatalf("%d levels: %v", MaxDepth, err)
	}
	deeper := strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1)
	if err := Check([]byte(deeper)); err == nil || !strings.Contains(err.Error(), "nested too deeply") {
		t.Fatalf("%d levels: %v", MaxDepth+1, err)
	}
	// encoding/json draws the line at the same depth
	if !json.Valid([]byte(deep)) || json.Valid([]byte(deeper)) {
		t.Fatal("encoding/json bounds nesting elsewhere")
	}
}

func TestCheckReaderReturnsAReadErrorAsItIs(t *testing.T) {
	broken := errors.New("disk gone")
	err := CheckReader(io.MultiReader(strings.NewReader(`{"a":`), iotest.ErrReader(broken)))
	if !errors.Is(err, broken) {
		t.Fatalf("got %v", err)
	}
	var e *Error
	if errors.As(err, &e) {
		t.Fatal("a read error passed for a problem of the body")
	}
}

// For a body without repeated names, Check accepts exactly what encoding/json
// accepts: the syntax is RFC 8259's, and nothing else is refused.
func FuzzCheckAgreesWithEncodingJSON(f *testing.F) {
	for _, seed := range []string{
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		`[1,-0.5e+3,true,false,null,"é😀"]`, `{"a":NaN}`, `[1,]`, "\xef\xbb\xbf[]",
		`{"a":1,"a":2}`, `{"a":1,"a":2}`, `{"x":{"y":[{"z":1,"z":2}]}}`, ` 0 `, `01`, `"\ud800"`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		err := Check(data)
		streamed := CheckReader(iotest.OneByteReader(bytes.NewReader(data)))
		if fmt.Sprint(err) != fmt.Sprint(streamed) {
			t.Fatalf("Check says %v, CheckReader %v", err, streamed)
		}
		valid := json.Valid(data)
		var e *Error
		switch {
		case err == nil && !valid:
			t.Fatalf("accepted what encoding/json refuses: %q", data)
		case err != nil && !IsDuplicate(err) && valid:
			t.Fatalf("refused what encoding/json accepts (%v): %q", err, data)
		case !valid:
			// a repeated name, then a syntax error: the name is met first
		case errors.As(err, &e) && e.Duplicate && comparable(e.Name) && !repeatsAName(data):
			t.Fatalf("found a repeated name encoding/json does not (%v): %q", err, data)
		case err == nil && repeatsAName(data):
			t.Fatalf("missed a repeated name: %q", data)
		}
	})
}

// comparable says a name decodes the same here and in encoding/json, which
// turns every lone surrogate and every invalid byte into U+FFFD where Check
// keeps them apart.
func comparable(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsRune(name, utf8.RuneError)
}

// repeatsAName is a slow, independent check for a repeated name, on
// encoding/json's tokens, for a valid body. Names that are not comparable are
// left out.
func repeatsAName(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	type level struct {
		object bool
		atName bool // the object's next token is a name or its '}'
		seen   map[string]bool
	}
	var stack []*level
	valueEnded := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].atName = true
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].atName {
			if name, ok := tok.(string); ok {
				top := stack[n-1]
				top.atName = false
				if !comparable(name) {
					continue
				}
				if top.seen[name] {
					return true
				}
				top.seen[name] = true
				continue
			}
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &level{object: true, atName: true, seen: map[string]bool{}})
		case json.Delim('['):
			stack = append(stack, &level{})
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			valueEnded()
		default:
			valueEnded()
		}
	}
}

// screenshotRequest is a Codex turn of the size the relay carries most: a
// few megabytes of base64 screenshot in a conversation of small items.
func screenshotRequest(mb int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"gpt-5","instructions":"be brief","input":[`)
	for i := range 200 {
		fmt.Fprintf(&b, `{"type":"message","role":"user","content":[{"type":"input_text","text":"step %d: run the tests and report"}]},`, i)
		fmt.Fprintf(&b, `{"type":"function_call","call_id":"c%d","name":"shell","arguments":"{\"cmd\":[\"go\",\"test\",\"./...\"]}"},`, i)
	}
	b.WriteString(`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`)
	b.WriteString(strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAA", mb<<20/25))
	b.WriteString(`"}]}],"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"array"}}}}],"stream":true}`)
	return b.Bytes()
}

func TestScreenshotRequestIsAccepted(t *testing.T) {
	if err := Check(screenshotRequest(1)); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkCheck(b *testing.B) {
	body := screenshotRequest(10)
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if err := Check(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCheckReader(b *testing.B) {
	body := screenshotRequest(10)
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if err := CheckReader(bytes.NewReader(body)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodingJSONValid(b *testing.B) {
	body := screenshotRequest(10)
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if !json.Valid(body) {
			b.Fatal("invalid")
		}
	}
}
