package util

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// preFixCleanup is what buildRequest ran before the cleanup was confined to the
// schemas a request declares: it renamed every parametersJsonSchema in the payload
// and cleaned the whole payload. The tests hold the confined cleanup to it
// everywhere but in contents, and show what it did there.
func preFixCleanup(payload string, addPlaceholder bool) string {
	paths := make([]string, 0)
	Walk(gjson.Parse(payload), "", parametersJSONSchemaKey, &paths)
	for _, p := range paths {
		payload, _ = RenameKey(payload, p, p[:len(p)-len(parametersJSONSchemaKey)]+parametersKey)
	}
	return cleanJSONSchema(payload, addPlaceholder)
}

// cleanRequestSchemasReference is CleanAntigravityRequestSchemas with each schema
// cleaned by cleanJSONSchema itself, which the scoped cleanup must match.
func cleanRequestSchemasReference(payload string, addPlaceholder bool) string {
	return cleanRequestSchemas(payload, addPlaceholder, cleanJSONSchema)
}

func assertMatchesReference(t *testing.T, payload string, addPlaceholder bool) string {
	t.Helper()
	want := cleanRequestSchemasReference(payload, addPlaceholder)
	got := CleanAntigravityRequestSchemas(payload, addPlaceholder)
	if got != want {
		t.Fatalf("scoped cleanup differs from the reference (addPlaceholder=%v)\ninput: %s\nwant:  %s\ngot:   %s", addPlaceholder, payload, want, got)
	}
	return got
}

func contentsOf(payload string) string { return gjson.Get(payload, "request.contents").Raw }

// withContentsOf returns out with request.contents put back as payload has it.
func withContentsOf(out, payload string) string {
	contents := gjson.Get(payload, "request.contents")
	if !contents.Exists() {
		return out
	}
	restored, err := sjson.SetRaw(out, "request.contents", contents.Raw)
	if err != nil {
		return out
	}
	return restored
}

// claudeCodeHistory replays calls the way Claude Code makes them, with arguments
// that use schema keywords as data, and a result shaped like a schema.
const claudeCodeHistory = `[` +
	`{"role":"user","parts":[{"text":"find the TODOs"}]},` +
	`{"role":"model","parts":[{"functionCall":{"name":"Grep","args":{"pattern":"TODO|FIXME","path":"src","output_mode":"content","-n":true,"type":"go"}}}]},` +
	`{"role":"user","parts":[{"functionResponse":{"name":"Grep","response":{"output":"src/a.go:1: TODO"}}}]},` +
	`{"role":"model","parts":[{"functionCall":{"name":"Glob","args":{"pattern":"**/*_test.go"}}},` +
	`{"functionCall":{"name":"WebFetch","args":{"url":"https://example.com/doc","prompt":"summarise","format":"markdown","title":"Doc"}}}]},` +
	`{"role":"model","parts":[{"functionCall":{"name":"configure","args":{"type":"object","properties":{"a":{"type":["string","null"]}},"required":["a","b"],"$ref":"#/x","anyOf":[{"type":"string"}],"items":{"type":"array"}}}}]},` +
	`{"role":"user","parts":[{"functionResponse":{"name":"configure","response":{"schema":{"type":"object","additionalProperties":false,"properties":{"x":{"type":"array"}}},"nullable":true}}}]},` +
	`{"role":"model","parts":[{"functionCall":{"name":"make_tool","args":{"parametersJsonSchema":{"type":"object"}}}}]}` +
	`]`

const grepDeclaration = `{"name":"Grep","parametersJsonSchema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",` +
	`"properties":{"pattern":{"type":"string","minLength":1}},"required":["pattern"],"additionalProperties":false}}`

// requestSchemaCases are requests reaching each decision of
// CleanAntigravityRequestSchemas; the fuzz test starts from them too.
var requestSchemaCases = map[string]string{
	"claude code session": `{"project":"p","request":{"contents":` + claudeCodeHistory + `,"tools":[{"functionDeclarations":[` + grepDeclaration + `]}]},"model":"m"}`,
	"nested declaration key": `{"request":{"tools":[{"functionDeclarations":[{"name":"make_tool","parametersJsonSchema":{"type":"object",` +
		`"properties":{"parametersJsonSchema":{"type":"object","properties":{"parametersJsonSchema":{"type":"string"}}}},"required":["parametersJsonSchema"]}}]}]}}`,
	"both parameters": `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parameters":{"type":"object","properties":{"old":{"type":"string"}}},` +
		`"parametersJsonSchema":{"type":"object","properties":{"new":{"type":"string","format":"f"}}}}]}]}}`,
	"duplicate declaration key": `{"request":{"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"type":"object"},` +
		`"parametersJsonSchema":{"type":"object","properties":{"b":{"type":"string","default":"x"}}}}]}]}}`,
	"every site": `{"request":{"tools":[{"googleSearch":{}},{"functionDeclarations":[{"name":"a"},{"name":"b","parameters":{"type":"object","title":"t"},` +
		`"response":{"type":"object","properties":{"r":{"type":["integer","null"]}}}}],"function_declarations":[{"name":"c","parameters_json_schema":{"type":"array"},` +
		`"response_json_schema":{"type":"object","x-r":1},"responseJsonSchema":{"const":1}}]}],"generationConfig":{"responseSchema":{"type":"OBJECT","nullable":true},` +
		`"_responseJsonSchema":{"$ref":"#/A"},"responseFormat":{"text":{"mimeType":"APPLICATION_JSON","schema":{"type":"object","additionalProperties":false}}}},` +
		`"generation_config":{"response_schema":{"enum":[1,2]},"response_json_schema":{"oneOf":[{"type":"string"}]},"response_format":{"text":{"schema":{"format":"date"}}}}}}`,
	"not a schema": `{"request":{"title":"x","x-ext":1,"labels":{"type":"object","format":"f"},"systemInstruction":{"parts":[{"text":"hi"}],"title":"s"},` +
		`"toolConfig":{"functionCallingConfig":{"mode":"AUTO","format":"f"}},"tools":[{"googleSearch":{"type":"object"}},{"functionDeclarations":"not a list"},` +
		`{"functionDeclarations":[{"name":"f","description":"d","extra":{"type":"object","title":"t"}},"scalar declaration"]}],"generationConfig":{"thinkingConfig":{"title":"t"}}}}`,
	"tools not a list": `{"request":{"tools":{"functionDeclarations":[{"name":"f","parameters":{"type":"object","title":"t"}}]}}}`,
	"no request":       `{"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","title":"t"}}]}]}`,
	"indented": "{\n  \"request\": {\n    \"tools\": [\n      {\n        \"functionDeclarations\": [\n          {\"name\": \"a\", \"parametersJsonSchema\": " +
		"{\"type\": \"object\", \"properties\": {\"s\": {\"type\": \"string\", \"minLength\": 1}}}}\n        ]\n      }\n    ],\n" +
		"    \"contents\": [{\"role\": \"model\", \"parts\": [{\"functionCall\": {\"name\": \"a\", \"args\": {\"s\": \"x\", \"format\": \"y\"}}}]}]\n  }\n}",
}

func TestCleanAntigravityRequestSchemasMatchesReference(t *testing.T) {
	for name, cases := range map[string]map[string]string{"request": requestSchemaCases, "scoped": scopedCleanupCases} {
		for caseName, payload := range cases {
			t.Run(name+"/"+caseName, func(t *testing.T) {
				for _, addPlaceholder := range []bool{true, false} {
					got := assertMatchesReference(t, payload, addPlaceholder)
					if gjson.Valid(payload) && contentsOf(got) != contentsOf(payload) {
						t.Fatalf("contents changed (addPlaceholder=%v)\nwant: %s\ngot:  %s", addPlaceholder, contentsOf(payload), contentsOf(got))
					}
				}
			})
		}
	}
}

// TestCleanAntigravityRequestSchemasLeavesContentsAlone is the defect this cleanup
// was confined to schemas for: the whole-payload cleanup rewrote the calls Claude
// Code had made as if they were schemas, so the model was shown a Grep without the
// pattern it searched for and a configure call it never made.
func TestCleanAntigravityRequestSchemasLeavesContentsAlone(t *testing.T) {
	payload := requestSchemaCases["claude code session"]
	for _, addPlaceholder := range []bool{true, false} {
		out := assertMatchesReference(t, payload, addPlaceholder)
		if contentsOf(out) != claudeCodeHistory {
			t.Fatalf("contents changed (addPlaceholder=%v)\nwant: %s\ngot:  %s", addPlaceholder, claudeCodeHistory, contentsOf(out))
		}
		// The tool schema is still cleaned and renamed.
		decl := gjson.Get(out, "request.tools.0.functionDeclarations.0")
		if decl.Get("parametersJsonSchema").Exists() || decl.Get("parameters.$schema").Exists() || decl.Get("parameters.additionalProperties").Exists() {
			t.Fatalf("tool schema not cleaned (addPlaceholder=%v): %s", addPlaceholder, decl.Raw)
		}
		if desc := decl.Get("parameters.properties.pattern.description").String(); !strings.Contains(desc, "minLength: 1") {
			t.Fatalf("pattern constraint not moved to the description (addPlaceholder=%v): %s", addPlaceholder, decl.Raw)
		}

		before := preFixCleanup(payload, addPlaceholder)
		if args := gjson.Get(before, "request.contents.1.parts.0.functionCall.args"); args.Get("pattern").Exists() {
			t.Fatalf("the case no longer shows the defect: the old cleanup kept Grep's pattern: %s", args.Raw)
		}
	}
}

func TestCleanAntigravityRequestSchemasCleansEverySchemaSite(t *testing.T) {
	const schema = `{"$schema":"x","type":"object","properties":{"p":{"type":["string","null"],"format":"uri"}},"additionalProperties":false}`
	sites := []struct{ request, at string }{
		{`"tools":[{"functionDeclarations":[{"name":"f","parameters":%s}]}]`, "request.tools.0.functionDeclarations.0.parameters"},
		{`"tools":[{"functionDeclarations":[{"name":"f","parametersJsonSchema":%s}]}]`, "request.tools.0.functionDeclarations.0.parameters"},
		{`"tools":[{"functionDeclarations":[{"name":"f","parameters_json_schema":%s}]}]`, "request.tools.0.functionDeclarations.0.parameters_json_schema"},
		{`"tools":[{"functionDeclarations":[{"name":"f","response":%s}]}]`, "request.tools.0.functionDeclarations.0.response"},
		{`"tools":[{"functionDeclarations":[{"name":"f","responseJsonSchema":%s}]}]`, "request.tools.0.functionDeclarations.0.responseJsonSchema"},
		{`"tools":[{"functionDeclarations":[{"name":"f","response_json_schema":%s}]}]`, "request.tools.0.functionDeclarations.0.response_json_schema"},
		{`"tools":[{"function_declarations":[{"name":"f","parameters":%s}]}]`, "request.tools.0.function_declarations.0.parameters"},
		{`"tools":[{"googleSearch":{}},{"functionDeclarations":[{"name":"e"},{"name":"f","parameters":%s}]}]`, "request.tools.1.functionDeclarations.1.parameters"},
		{`"generationConfig":{"responseSchema":%s}`, "request.generationConfig.responseSchema"},
		{`"generationConfig":{"response_schema":%s}`, "request.generationConfig.response_schema"},
		{`"generationConfig":{"responseJsonSchema":%s}`, "request.generationConfig.responseJsonSchema"},
		{`"generationConfig":{"response_json_schema":%s}`, "request.generationConfig.response_json_schema"},
		{`"generationConfig":{"_responseJsonSchema":%s}`, "request.generationConfig._responseJsonSchema"},
		{`"generation_config":{"responseSchema":%s}`, "request.generation_config.responseSchema"},
		{`"generationConfig":{"responseFormat":{"text":{"mimeType":"APPLICATION_JSON","schema":%s}}}`, "request.generationConfig.responseFormat.text.schema"},
		{`"generationConfig":{"response_format":{"text":{"schema":%s}}}`, "request.generationConfig.response_format.text.schema"},
	}
	for _, site := range sites {
		// The same schema sits in contents as a call's arguments, where it is data.
		payload := `{"project":"p","request":{` + fmt.Sprintf(site.request, schema) +
			`,"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":` + schema + `}}]}]},"model":"m"}`
		for _, addPlaceholder := range []bool{true, false} {
			out := assertMatchesReference(t, payload, addPlaceholder)
			cleaned := gjson.Get(out, site.at)
			if !cleaned.IsObject() || cleaned.Get("$schema").Exists() || cleaned.Get("additionalProperties").Exists() ||
				cleaned.Get("properties.p.type").String() != "string" || !strings.Contains(cleaned.Get("properties.p.description").String(), "format: uri") {
				t.Fatalf("%s not cleaned (addPlaceholder=%v): %s", site.at, addPlaceholder, out)
			}
			if contentsOf(out) != contentsOf(payload) {
				t.Fatalf("%s: contents changed (addPlaceholder=%v): %s", site.at, addPlaceholder, out)
			}
			// Where the schema is the only thing that differs, the result is what
			// the whole-payload cleanup gave, contents aside.
			if want := withContentsOf(preFixCleanup(payload, addPlaceholder), payload); out != want {
				t.Fatalf("%s differs from the old cleanup outside contents (addPlaceholder=%v)\nwant: %s\ngot:  %s", site.at, addPlaceholder, want, out)
			}
		}
	}
}

func TestCleanAntigravityRequestSchemasLeavesWhatIsNotASchema(t *testing.T) {
	for _, name := range []string{"not a schema", "tools not a list", "no request"} {
		payload := requestSchemaCases[name]
		for _, addPlaceholder := range []bool{true, false} {
			if out := assertMatchesReference(t, payload, addPlaceholder); out != payload {
				t.Fatalf("%s: changed (addPlaceholder=%v)\nwant: %s\ngot:  %s", name, addPlaceholder, payload, out)
			}
		}
	}
	for _, payload := range []string{scopedCleanupCases["invalid json"], scopedCleanupCases["not an object"], `"text"`, ``} {
		if out := CleanAntigravityRequestSchemas(payload, true); out != payload {
			t.Fatalf("changed %q into %q", payload, out)
		}
	}
}

// TestCleanAntigravityRequestSchemasRenamesOnlyDeclarations renames the declaration's
// own parametersJsonSchema, and no property of that name: the request-wide rename
// renamed those too, and emptied the whole payload when one sat inside another.
func TestCleanAntigravityRequestSchemasRenamesOnlyDeclarations(t *testing.T) {
	payload := requestSchemaCases["nested declaration key"]
	for _, addPlaceholder := range []bool{true, false} {
		out := assertMatchesReference(t, payload, addPlaceholder)
		decl := gjson.Get(out, "request.tools.0.functionDeclarations.0")
		if decl.Get("parametersJsonSchema").Exists() || !decl.Get("parameters.properties.parametersJsonSchema.properties.parametersJsonSchema").Exists() {
			t.Fatalf("renamed the wrong keys (addPlaceholder=%v): %s", addPlaceholder, out)
		}
		if got := decl.Get("parameters.required").Raw; got != `["parametersJsonSchema"]` {
			t.Fatalf("required = %s, want the property kept (addPlaceholder=%v)", got, addPlaceholder)
		}
		if before := preFixCleanup(payload, addPlaceholder); before != "" {
			t.Fatalf("the case no longer shows the defect: the old rename kept the payload: %s", before)
		}
	}
	// A declaration with both forms, or the same one twice, ends up as the
	// request-wide rename left it.
	for _, name := range []string{"both parameters", "duplicate declaration key"} {
		payload := requestSchemaCases[name]
		for _, addPlaceholder := range []bool{true, false} {
			if out, want := assertMatchesReference(t, payload, addPlaceholder), preFixCleanup(payload, addPlaceholder); out != want {
				t.Fatalf("%s differs from the old rename (addPlaceholder=%v)\nwant: %s\ngot:  %s", name, addPlaceholder, want, out)
			}
		}
	}
}

// TestCleanAntigravityRequestSchemasKeepsTheRootPlaceholder pins why each schema is
// cleaned at its real path: Claude's VALIDATED mode wants a required property, and
// the Antigravity cleanup adds the "_" placeholder to a tool's root schema only
// when the schema does not sit at the root of what it cleans.
func TestCleanAntigravityRequestSchemasKeepsTheRootPlaceholder(t *testing.T) {
	const schema = `{"type":"object","properties":{"a":{"type":"string"}}}`
	payload := `{"request":{"tools":[{"functionDeclarations":[{"name":"f","parameters":` + schema + `}]}]}}`
	out := CleanAntigravityRequestSchemas(payload, true)
	root := gjson.Get(out, "request.tools.0.functionDeclarations.0.parameters")
	if root.Get("properties._.type").String() != "boolean" || root.Get("required").Raw != `["_"]` {
		t.Fatalf("root schema lost its placeholder: %s", root.Raw)
	}
	if out != preFixCleanup(payload, true) {
		t.Fatalf("differs from the whole-payload cleanup\nwant: %s\ngot:  %s", preFixCleanup(payload, true), out)
	}
	if bare := CleanJSONSchemaForAntigravity(schema); gjson.Get(bare, "properties._").Exists() {
		t.Fatalf("a bare schema now gets the placeholder too, so the reason above is stale: %s", bare)
	}
}

// TestCleanAntigravityRequestSchemasChangesOnlyContentsOnRandomRequests holds the
// cleanup to its reference on random requests, and to the whole-payload cleanup it
// replaced everywhere but in contents, which it must leave as they are.
func TestCleanAntigravityRequestSchemasChangesOnlyContentsOnRandomRequests(t *testing.T) {
	const n = 600
	compared := 0
	for seed := int64(0); seed < n; seed++ {
		payload, preFixComparable := randomSchemaRequest(rand.New(rand.NewSource(seed)))
		for _, addPlaceholder := range []bool{true, false} {
			out := assertMatchesReference(t, payload, addPlaceholder)
			if contentsOf(out) != contentsOf(payload) {
				t.Fatalf("seed %d: contents changed (addPlaceholder=%v)\ninput: %s\ngot:   %s", seed, addPlaceholder, payload, out)
			}
			if !preFixComparable {
				continue
			}
			if want := withContentsOf(preFixCleanup(payload, addPlaceholder), payload); out != want {
				t.Fatalf("seed %d: differs from the old cleanup outside contents (addPlaceholder=%v)\ninput: %s\nwant:  %s\ngot:   %s", seed, addPlaceholder, payload, want, out)
			}
			compared++
		}
	}
	if compared < n {
		t.Fatalf("only %d of %d runs were compared with the old cleanup", compared, 2*n)
	}
	t.Logf("%d of %d runs compared with the old cleanup", compared, 2*n)
}

func TestApplySchemaEditsFallsBackToPaths(t *testing.T) {
	const doc = `{"a":{"b":1},"c":[2,3]}`
	if got := applySchemaEdits(doc, []schemaEdit{newSchemaEdit(gjson.Get(doc, "c.1"), "30", "c.1")}); got != `{"a":{"b":1},"c":[2,30]}` {
		t.Fatalf("spliced edit: got %s", got)
	}
	misplaced := newSchemaEdit(gjson.Get(doc, "a.b"), "10", "a.b")
	misplaced.start, misplaced.end = misplaced.start+1, misplaced.end+1
	if got := applySchemaEdits(doc, []schemaEdit{misplaced}); got != `{"a":{"b":10},"c":[2,3]}` {
		t.Fatalf("edit made by path: got %s", got)
	}
}

// FuzzCleanAntigravityRequestSchemas holds the cleanup to its reference on
// arbitrary input, and checks that valid JSON stays valid and contents stay as
// they are. Large inputs are skipped, as for FuzzCleanSchemaDoc.
func FuzzCleanAntigravityRequestSchemas(f *testing.F) {
	for _, cases := range []map[string]string{requestSchemaCases, scopedCleanupCases} {
		for _, payload := range cases {
			f.Add(payload, true)
			f.Add(payload, false)
		}
	}
	f.Fuzz(func(t *testing.T, payload string, addPlaceholder bool) {
		if len(payload) > 4096 {
			t.Skip()
		}
		want, panicked := referenceOrPanic(payload, addPlaceholder)
		if panicked {
			t.Skip("cleanJSONSchema itself panics on a schema in this input")
		}
		got := CleanAntigravityRequestSchemas(payload, addPlaceholder)
		if got != want {
			t.Fatalf("scoped cleanup differs from the reference (addPlaceholder=%v)\ninput: %q\nwant:  %q\ngot:   %q", addPlaceholder, payload, want, got)
		}
		if gjson.Valid(payload) {
			if !gjson.Valid(got) {
				t.Fatalf("valid input became invalid (addPlaceholder=%v)\ninput: %q\ngot:   %q", addPlaceholder, payload, got)
			}
			if contentsOf(got) != contentsOf(payload) {
				t.Fatalf("contents changed (addPlaceholder=%v)\ninput: %q\ngot:   %q", addPlaceholder, payload, got)
			}
		}
	})
}

func referenceOrPanic(payload string, addPlaceholder bool) (out string, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	return cleanRequestSchemasReference(payload, addPlaceholder), false
}
