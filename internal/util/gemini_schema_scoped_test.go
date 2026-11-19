package util

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// The scoped cleanup is only worth having if it is indistinguishable from the
// whole-payload one, so every test here compares the two byte for byte.

func assertScopedMatchesWhole(t *testing.T, payload string, addPlaceholder bool) (scoped bool) {
	t.Helper()
	want := renameAndCleanWholePayload(payload, addPlaceholder)
	got, ok := renameAndCleanScoped(payload, addPlaceholder)
	if ok && got != want {
		t.Fatalf("scoped cleanup differs (addPlaceholder=%v)\ninput: %s\nwant:  %s\ngot:   %s", addPlaceholder, payload, want, got)
	}
	if public := CleanAntigravityRequestSchemas(payload, addPlaceholder); public != want {
		t.Fatalf("CleanAntigravityRequestSchemas differs (addPlaceholder=%v)\ninput: %s\nwant:  %s\ngot:   %s", addPlaceholder, payload, want, public)
	}
	return ok
}

// scopedCleanupCases are small requests reaching each decision of the scoped
// path, the fallbacks included; the fuzz test starts from them too.
var scopedCleanupCases = map[string]string{
	"no schema at all":      `{"project":"p","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
	"function call as data": `{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"Grep","args":{"pattern":"TODO","type":"go","title":"t"}}}]}]}}`,
	"rename and clean":      `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"$schema":"x","type":"object","properties":{"p":{"type":["string","null"],"format":"uri"}},"required":["p"],"additionalProperties":false}}]}]}}`,
	"both spellings":        `{"request":{"tools":[{"function_declarations":[{"name":"a","parameters":{"type":"object"},"parametersJsonSchema":{"type":"object","properties":{"n":{"type":"integer","default":1}}}}]}]}}`,
	"response schema":       `{"request":{"generationConfig":{"responseJsonSchema":{"type":"object","properties":{"v":{"enum":[1,2]}},"required":["v","w"]}}}}`,
	"indented":              "{\n  \"request\": {\n    \"tools\": [\n      {\n        \"functionDeclarations\": [\n          {\"name\": \"a\", \"parametersJsonSchema\": {\"type\": \"object\", \"properties\": {\"s\": {\"type\": \"string\", \"minLength\": 1, \"maxLength\": 2}}}}\n        ]\n      }\n    ]\n  }\n}",
	"leading whitespace":    "  \n" + `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"type":"object","x-a":1}}]}]}}`,
	"nested rename":         `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"type":"object","properties":{"parametersJsonSchema":{"type":"string"}}}}]}]}}`,
	"keyword near root":     `{"request":{"title":"x","tools":[]}}`,
	"duplicate request":     `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"type":"object","format":"x"}}]}]},"request":{"contents":[]}}`,
	"duplicate inside":      `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"type":"object","format":"x","format":"y"}}]}]}}`,
	"unsafe step":           `{"request":{"contents":[{"parts":[{"functionCall":{"args":{"src/a.go":{"type":"object"},"pipe|x":{"format":"f"},"pipe":{"x":{"format":"g"}}}}}]}]}}`,
	"escaped properties":    `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"type":"object","properties":{"a.properties":{"_":{"type":"boolean"}},"a.required":["_"]}}}]}]}}`,
	"anyOf into type":       `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"type":"object","properties":{"k":{"type":{"anyOf":["object"]},"description":"d"}}}}]}]}}`,
	"not an object":         `[{"type":["a","null"]}]`,
	"invalid json":          `{"request":{"tools":[{"type":["a","null"]}]`,
}

func TestCleanAntigravityRequestSchemasMatchesWholePayload(t *testing.T) {
	for name, payload := range scopedCleanupCases {
		t.Run(name, func(t *testing.T) {
			assertScopedMatchesWhole(t, payload, true)
			assertScopedMatchesWhole(t, payload, false)
		})
	}
}

// TestCleanAntigravityRequestSchemasMatchesWholePayloadOnRandomRequests runs both
// cleanups over requests built from every keyword the cleanup knows, property
// names that collide with them or need escaping, nested and indented schemas and
// function calls whose arguments look like schemas.
func TestCleanAntigravityRequestSchemasMatchesWholePayloadOnRandomRequests(t *testing.T) {
	const n = 600
	scoped := 0
	for seed := int64(0); seed < n; seed++ {
		payload := randomSchemaRequest(rand.New(rand.NewSource(seed)))
		for _, addPlaceholder := range []bool{true, false} {
			if assertScopedMatchesWhole(t, payload, addPlaceholder) {
				scoped++
			}
		}
	}
	// Most requests must take the scoped path, or the comparison above proves
	// little about it.
	if scoped < 2*n*6/10 {
		t.Fatalf("only %d of %d runs took the scoped path", scoped, 2*n)
	}
	t.Logf("%d of %d runs took the scoped path", scoped, 2*n)
}

// TestScopeKeysCoverEveryKeyTheCleanupEdits feeds the scoped path every string
// literal of the cleanup's source, as a key, with values of every kind, in the
// places a key can stand in a schema. A key the cleanup edits but scopeKeys does
// not list would leave the scoped output behind the whole-payload one.
func TestScopeKeysCoverEveryKeyTheCleanupEdits(t *testing.T) {
	literals := map[string]struct{}{"x-anything": {}, "type": {}}
	fset := token.NewFileSet()
	for _, file := range []string{"gemini_schema.go", "gemini_schema_items.go", "gemini_schema_scoped.go"} {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, errUnquote := strconv.Unquote(lit.Value); errUnquote == nil {
					literals[value] = struct{}{}
				}
			}
			return true
		})
	}
	values := []string{`"x"`, `"object"`, `"array"`, `"ARRAY"`, `"string"`, `1`, `true`, `false`, `null`, `[]`, `["a","null"]`,
		`[1,"b"]`, `{}`, `{"type":"string"}`, `{"type":"string","description":"` + placeholderReasonDescription + `"}`}
	shapes := []string{
		`{"a":{"b":{"c":{"d":{%s}}}}}`,
		`{"a":{"b":{"c":{"d":{%s,"properties":{"p":{"type":"string"}}}}}}}`,
		`{"a":{"b":{"c":{"properties":{%s}}}}}`,
		`{"a":{"b":{"c":{"properties":{"p":{%s}}}}}}`,
	}
	for literal := range literals {
		key, _ := json.Marshal(literal)
		for _, value := range values {
			for _, shape := range shapes {
				payload := fmt.Sprintf(shape, string(key)+":"+value)
				for _, addPlaceholder := range []bool{true, false} {
					// A literal equal to a key the shape already has makes a
					// duplicate, which is meant to take the whole-payload path.
					duplicate := strings.Contains(shape, `{`+string(key)+`:`) || strings.Contains(shape, `,`+string(key)+`:`)
					if !assertScopedMatchesWhole(t, payload, addPlaceholder) && !duplicate {
						t.Fatalf("probe %q took the whole-payload path: %s", literal, payload)
					}
				}
			}
		}
	}
}

// FuzzCleanAntigravityRequestSchemas holds the scoped cleanup to the
// whole-payload one on arbitrary input. Plain `go test` runs the seeds; the seeds
// are small and large inputs are skipped, because the whole-payload cleanup is
// quadratic and would leave the fuzzer a handful of runs a minute.
func FuzzCleanAntigravityRequestSchemas(f *testing.F) {
	for _, payload := range scopedCleanupCases {
		f.Add(payload, true)
		f.Add(payload, false)
	}
	f.Fuzz(func(t *testing.T, payload string, addPlaceholder bool) {
		if len(payload) > 4096 {
			t.Skip()
		}
		want, panicked := wholePayloadOrPanic(payload, addPlaceholder)
		if panicked {
			t.Skip("the whole-payload cleanup itself panics on this input")
		}
		if got := CleanAntigravityRequestSchemas(payload, addPlaceholder); got != want {
			t.Fatalf("scoped cleanup differs (addPlaceholder=%v)\ninput: %q\nwant:  %q\ngot:   %q", addPlaceholder, payload, want, got)
		}
	})
}

func wholePayloadOrPanic(payload string, addPlaceholder bool) (out string, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	return renameAndCleanWholePayload(payload, addPlaceholder), false
}

// randomSchemaRequest builds an Antigravity request whose tools, response schema
// and function call arguments are drawn from schemaGen.
func randomSchemaRequest(r *rand.Rand) string {
	// One request in ten may hold keys gjson paths misread, duplicate keys or a
	// parametersJsonSchema nested in another (which the old rename empties the
	// payload over); each must take the whole-payload path and still match.
	g := schemaGen{r: r, misread: r.Intn(10) == 0, duplicates: r.Intn(10) == 0, nested: r.Intn(10) == 0}
	var decls []string
	for i := r.Intn(5); i >= 0; i-- {
		decl := `"name":"tool_` + strconv.Itoa(i) + `"`
		switch r.Intn(4) {
		case 0:
			decl += `,"parameters":` + g.schema(0)
		case 1:
			decl += `,"parameters":` + g.schema(0) + `,"parametersJsonSchema":` + g.schema(0)
		default:
			decl += `,"description":"d","parametersJsonSchema":` + g.schema(0)
		}
		decls = append(decls, "{"+g.ws()+decl+g.ws()+"}")
	}
	declKey := g.pick(`"functionDeclarations"`, `"function_declarations"`)
	var parts []string
	for i := r.Intn(6); i >= 0; i-- {
		switch r.Intn(5) {
		case 0:
			parts = append(parts, `{"text":"turn `+strconv.Itoa(i)+`"}`)
		case 1:
			parts = append(parts, `{"inlineData":{"mimeType":"image/png","data":"`+strings.Repeat("iVBORw0K", 1+r.Intn(40))+`"}}`)
		case 2:
			parts = append(parts, `{"functionCall":{"name":"f","args":`+g.schema(1)+`}}`)
		case 3:
			parts = append(parts, `{"functionResponse":{"name":"f","response":{"output":"o","schema":`+g.schema(2)+`}}}`)
		default:
			parts = append(parts, `{"functionCall":{"name":"Grep","args":{"pattern":"TODO","type":"go","path":"src"}}}`)
		}
	}
	request := `"contents":[{"role":"model","parts":[` + strings.Join(parts, ",") + `]}],"tools":[{` + declKey + `:[` +
		strings.Join(decls, ","+g.ws()) + `]},{"googleSearch":{}}]`
	if g.chance(3) {
		request += `,"generationConfig":{"responseSchema":` + g.schema(0) + `}`
	}
	if g.chance(20) {
		request += `,` + g.pick(`"title":"near root"`, `"x-root":1`, `"request":{}`)
	}
	payload := `{"project":"p",` + g.ws() + `"request":{` + request + `},"model":"m"}`
	if g.chance(15) {
		payload = "\n " + payload
	}
	return payload
}

type schemaGen struct {
	r                           *rand.Rand
	misread, duplicates, nested bool
}

func (g schemaGen) chance(n int) bool             { return g.r.Intn(n) == 0 }
func (g schemaGen) pick(options ...string) string { return options[g.r.Intn(len(options))] }

func (g schemaGen) ws() string { return g.pick("", "", "", " ", "\n  ") }

// propertyName mixes plain names, names of keywords (which the cleanup must leave
// alone as property names), names it escapes, and now and then one that gjson
// paths read as something else, which sends the request down the whole-payload
// path.
func (g schemaGen) propertyName() string {
	name := g.pick("a", "b", "path", "mode", "value", "items", "description",
		"format", "type", "title", "pattern", "required", "properties", "default", "enum", "const", "$ref", "_", "reason",
		"x-custom", "additionalProperties", "anyOf", "nullable", "minLength",
		"a.b", "star*", "q?", "$id", "with space", "src/a.go", "中文")
	if g.nested && g.chance(10) {
		name = "parametersJsonSchema"
	}
	if g.misread && g.chance(10) {
		name = g.pick("pipe|name", "#hash", "@type", "", `back\slash`, `quote"d`, "!bang", ":colon")
	}
	quoted, _ := json.Marshal(name)
	return string(quoted)
}

func (g schemaGen) scalar() string {
	return g.pick(`"x"`, `"object"`, `"array"`, `"string"`, `1`, `0.5`, `true`, `false`, `null`, `"<&>"`)
}

// schema returns a random schema object; deeper levels get fewer children.
func (g schemaGen) schema(depth int) string {
	// Three levels and a few children per level reach every keyword pairing the
	// cleanup has; deeper trees only grow the payload, and the whole-payload
	// cleanup this is compared with is quadratic in it.
	if depth > 3 {
		return `{"type":"string"}`
	}
	var fields []string
	seen := map[string]bool{}
	add := func(key, value string) {
		if seen[key] && !(g.duplicates && g.chance(3)) {
			return
		}
		seen[key] = true
		fields = append(fields, `"`+key+`":`+g.ws()+value)
	}
	if g.chance(2) || depth == 0 {
		add("type", g.pick(`"object"`, `"OBJECT"`, `"array"`, `"ARRAY"`, `"string"`, `"integer"`, `"null"`,
			`["string","null"]`, `["integer","string"]`, `["null"]`, `[]`, `["object","null"]`))
	}
	var names []string
	if g.chance(2) {
		var props []string
		taken := map[string]bool{}
		for i := g.r.Intn(3); i >= 0; i-- {
			name := g.propertyName()
			if taken[name] {
				continue
			}
			taken[name] = true
			names = append(names, name)
			props = append(props, name+":"+g.ws()+g.schema(depth+1))
		}
		add("properties", "{"+strings.Join(props, ","+g.ws())+"}")
	}
	if g.chance(3) {
		required := append([]string(nil), names...)
		if g.chance(2) {
			required = append(required, `"ghost"`)
		}
		add("required", "["+strings.Join(required, ",")+"]")
	}
	if g.chance(4) {
		add("items", g.schema(depth+1))
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if g.chance(7) {
			var branches []string
			for i := g.r.Intn(2); i >= 0; i-- {
				branches = append(branches, g.schema(depth+1))
			}
			if g.chance(8) {
				branches = append(branches, g.scalar())
			}
			add(key, "["+strings.Join(branches, ",")+"]")
		}
	}
	if g.chance(8) {
		add("$ref", g.pick(`"#/$defs/A"`, `"#/definitions/B"`, `"C"`))
	}
	if g.chance(8) {
		add(g.pick("$defs", "definitions"), `{"A":`+g.schema(depth+1)+`}`)
	}
	if g.chance(5) {
		var values []string
		for i := g.r.Intn(12); i > 0; i-- {
			values = append(values, g.scalar())
		}
		add("enum", "["+strings.Join(values, ",")+"]")
	}
	if g.chance(8) {
		add("const", g.pick(`"c"`, `3`, `{"k":"<v>"}`, `[1,2]`, `null`))
	}
	if g.chance(5) {
		add("additionalProperties", g.pick(`false`, `true`, `{"type":"string"}`))
	}
	for _, key := range unsupportedConstraints {
		if g.chance(14) {
			add(key, g.pick(g.scalar(), `{"a":1}`, `[1]`))
		}
	}
	for _, key := range []string{"nullable", "title", "description", "$schema", "$id", "propertyNames", "patternProperties", "enumTitles", "prefill", "x-ext", "x-google-enum-descriptions"} {
		if g.chance(12) {
			add(key, g.pick(g.scalar(), `{"type":["string","null"],"format":"f"}`, `["a"]`))
		}
	}
	if g.chance(10) {
		add("properties", `{"reason":{"type":"string","description":"`+placeholderReasonDescription+`"}}`)
		add("required", `["reason"]`)
	}
	if g.chance(10) {
		add("properties", `{"_":{"type":"boolean"},"v":{"type":"string"}}`)
		add("required", `["_"]`)
	}
	if g.nested && g.chance(4) {
		add("parametersJsonSchema", g.schema(depth+1))
	}
	g.r.Shuffle(len(fields), func(i, j int) { fields[i], fields[j] = fields[j], fields[i] })
	return "{" + g.ws() + strings.Join(fields, ","+g.ws()) + g.ws() + "}"
}
