package common

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const weatherSchema = `{"type":"object","properties":{"city":{"type":"string"},"temp_c":{"type":"number"}},"required":["city","temp_c"],"additionalProperties":false}`

func TestOutputFormatReaders(t *testing.T) {
	strict := func(f OutputFormat) string {
		if f.Strict == nil {
			return "unset"
		}
		if *f.Strict {
			return "true"
		}
		return "false"
	}
	cases := []struct {
		name       string
		read       func(gjson.Result) OutputFormat
		request    string
		kind       OutputFormatKind
		schemaHas  string
		formatName string
		strict     string
	}{
		{"chat json_schema", OutputFormatFromChat, `{"response_format":{"type":"json_schema","json_schema":{"name":"weather","description":"d","strict":true,"schema":` + weatherSchema + `}}}`, OutputFormatJSONSchema, "temp_c", "weather", "true"},
		{"chat json_schema without schema", OutputFormatFromChat, `{"response_format":{"type":"json_schema","json_schema":{"name":"weather"}}}`, OutputFormatJSONObject, "", "", "unset"},
		{"chat json_object", OutputFormatFromChat, `{"response_format":{"type":"json_object"}}`, OutputFormatJSONObject, "", "", "unset"},
		{"chat text", OutputFormatFromChat, `{"response_format":{"type":"text"}}`, OutputFormatText, "", "", "unset"},
		{"chat none", OutputFormatFromChat, `{"messages":[]}`, OutputFormatNone, "", "", "unset"},
		{"responses json_schema", OutputFormatFromResponses, `{"text":{"verbosity":"low","format":{"type":"json_schema","name":"weather","strict":false,"schema":` + weatherSchema + `}}}`, OutputFormatJSONSchema, "temp_c", "weather", "false"},
		{"responses json_object", OutputFormatFromResponses, `{"text":{"format":{"type":"json_object"}}}`, OutputFormatJSONObject, "", "", "unset"},
		{"claude output_config", OutputFormatFromClaude, `{"output_config":{"effort":"high","format":{"type":"json_schema","schema":` + weatherSchema + `}}}`, OutputFormatJSONSchema, "temp_c", "", "unset"},
		{"claude deprecated output_format", OutputFormatFromClaude, `{"output_format":{"type":"json_schema","schema":` + weatherSchema + `}}`, OutputFormatJSONSchema, "temp_c", "", "unset"},
		{"claude effort only", OutputFormatFromClaude, `{"output_config":{"effort":"high"}}`, OutputFormatNone, "", "", "unset"},
		{"gemini json schema", OutputFormatFromGemini, `{"generationConfig":{"responseMimeType":"application/json","responseJsonSchema":` + weatherSchema + `}}`, OutputFormatJSONSchema, "temp_c", "", "unset"},
		{"gemini snake case", OutputFormatFromGemini, `{"generation_config":{"response_mime_type":"application/json","response_json_schema":` + weatherSchema + `}}`, OutputFormatJSONSchema, "temp_c", "", "unset"},
		{"gemini json mode", OutputFormatFromGemini, `{"generationConfig":{"responseMimeType":"application/json"}}`, OutputFormatJSONObject, "", "", "unset"},
		{"gemini enum mode", OutputFormatFromGemini, `{"generationConfig":{"responseMimeType":"text/x.enum","responseSchema":{"type":"STRING","enum":["a","b"]}}}`, OutputFormatNone, "", "", "unset"},
		{"gemini plain text", OutputFormatFromGemini, `{"generationConfig":{"responseMimeType":"text/plain","temperature":1}}`, OutputFormatNone, "", "", "unset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.read(gjson.Parse(tc.request))
			if f.Kind != tc.kind {
				t.Fatalf("kind = %v, want %v", f.Kind, tc.kind)
			}
			if tc.schemaHas != "" && !gjson.Get(f.Schema, "properties."+tc.schemaHas).Exists() {
				t.Fatalf("schema lost %s: %s", tc.schemaHas, f.Schema)
			}
			if f.Name != tc.formatName {
				t.Fatalf("name = %q, want %q", f.Name, tc.formatName)
			}
			if got := strict(f); got != tc.strict {
				t.Fatalf("strict = %s, want %s", got, tc.strict)
			}
		})
	}
}

func TestGeminiOpenAPISchemaBecomesJSONSchema(t *testing.T) {
	request := `{"generationConfig":{"responseMimeType":"application/json","responseSchema":{
		"type":"OBJECT",
		"properties":{
			"b":{"type":"INTEGER","nullable":true,"example":3},
			"a":{"type":"ARRAY","items":{"type":"STRING","format":"date-time"},"max_items":2},
			"c":{"anyOf":[{"type":"STRING"},{"type":"NUMBER"}]}
		},
		"propertyOrdering":["a","b"],
		"required":["a"]}}}`
	f := OutputFormatFromGemini(gjson.Parse(request))
	if f.Kind != OutputFormatJSONSchema {
		t.Fatalf("kind = %v, want JSON schema", f.Kind)
	}
	schema := gjson.Parse(f.Schema)
	for path, want := range map[string]string{
		"type":                      `"object"`,
		"properties.a.type":         `"array"`,
		"properties.a.items.type":   `"string"`,
		"properties.a.items.format": `"date-time"`,
		"properties.a.maxItems":     `2`,
		"properties.b.type":         `["integer","null"]`,
		"properties.c.anyOf.1.type": `"number"`,
		"required":                  `["a"]`,
	} {
		if got := schema.Get(path).Raw; got != want {
			t.Errorf("%s = %s, want %s; schema %s", path, got, want, f.Schema)
		}
	}
	for _, gone := range []string{"propertyOrdering", "properties.b.nullable", "properties.b.example"} {
		if schema.Get(gone).Exists() {
			t.Errorf("%s should not be in JSON Schema: %s", gone, f.Schema)
		}
	}
	var order []string
	schema.Get("properties").ForEach(func(key, _ gjson.Result) bool {
		order = append(order, key.String())
		return true
	})
	if strings.Join(order, ",") != "a,b,c" {
		t.Fatalf("property order = %v, want propertyOrdering first, then the rest", order)
	}
}

func TestWriteChat(t *testing.T) {
	schema := OutputFormat{Kind: OutputFormatJSONSchema, Name: "weather", Description: "today", Schema: weatherSchema}
	jsonMode := OutputFormat{Kind: OutputFormatJSONObject}
	for _, tc := range []struct {
		name, request string
		format        OutputFormat
		check         func(t *testing.T, out []byte)
	}{
		{"no system message", `{"messages":[{"role":"user","content":"weather?"}]}`, schema, func(t *testing.T, out []byte) {
			if gjson.GetBytes(out, "messages.#").Int() != 2 || gjson.GetBytes(out, "messages.0.role").String() != "system" || gjson.GetBytes(out, "messages.1.content").String() != "weather?" {
				t.Fatalf("instruction not put first: %s", out)
			}
		}},
		{"string system", `{"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"weather?"}]}`, jsonMode, func(t *testing.T, out []byte) {
			if got := gjson.GetBytes(out, "messages.0.content").String(); got != "Be brief.\n\n"+jsonOnlyInstruction {
				t.Fatalf("system content = %q", got)
			}
		}},
		{"list system", `{"messages":[{"role":"system","content":[{"type":"text","text":"Be brief."}]},{"role":"user","content":"weather?"}]}`, jsonMode, func(t *testing.T, out []byte) {
			parts := gjson.GetBytes(out, "messages.0.content").Array()
			if len(parts) != 2 || parts[0].Get("text").String() != "Be brief." || parts[1].Get("type").String() != "text" || parts[1].Get("text").String() != jsonOnlyInstruction {
				t.Fatalf("system parts = %s", gjson.GetBytes(out, "messages.0.content").Raw)
			}
		}},
		{"developer first", `{"messages":[{"role":"developer","content":"Be brief."}]}`, jsonMode, func(t *testing.T, out []byte) {
			if gjson.GetBytes(out, "messages.0.role").String() != "system" || gjson.GetBytes(out, "messages.1.content").String() != "Be brief." {
				t.Fatalf("instruction not put first: %s", out)
			}
		}},
		{"no messages", `{"model":"m"}`, jsonMode, func(t *testing.T, out []byte) {
			if gjson.GetBytes(out, "messages.0.role").String() != "system" {
				t.Fatalf("no system message: %s", out)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.format.WriteChat([]byte(tc.request))
			if !gjson.ValidBytes(out) {
				t.Fatalf("invalid JSON: %s", out)
			}
			// OpenAI-compatible upstreams are never sent response_format.
			if gjson.GetBytes(out, "response_format").Exists() {
				t.Fatalf("response_format written: %s", out)
			}
			tc.check(t, out)
		})
	}

	out := schema.WriteChat([]byte(`{"messages":[]}`))
	instruction := gjson.GetBytes(out, "messages.0.content").String()
	if !strings.Contains(instruction, "JSON Schema") || !strings.Contains(instruction, "weather: today") || !strings.HasSuffix(instruction, weatherSchema) {
		t.Fatalf("schema instruction = %q", instruction)
	}
	// Plain text is the default: nothing to ask for.
	if out := (OutputFormat{Kind: OutputFormatText}).WriteChat([]byte(`{"messages":[]}`)); string(out) != `{"messages":[]}` {
		t.Fatalf("text wrote %s", out)
	}
}

func TestResponsesJSONModeMentionsJSON(t *testing.T) {
	jsonMode := OutputFormat{Kind: OutputFormatJSONObject}
	out := jsonMode.WriteResponses([]byte(`{"instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]}],"text":{"verbosity":"low"}}`))
	if gjson.GetBytes(out, "text.format.type").String() != "json_object" || gjson.GetBytes(out, "text.verbosity").String() != "low" {
		t.Fatalf("text = %s", gjson.GetBytes(out, "text").Raw)
	}
	if role := gjson.GetBytes(out, "input.0.role").String(); role != "developer" || !strings.Contains(gjson.GetBytes(out, "input.0.content.0.text").String(), "JSON") {
		t.Fatalf("no JSON instruction ahead of the input: %s", out)
	}
	out = jsonMode.WriteResponses([]byte(`{"instructions":"Reply in JSON.","input":[]}`))
	if gjson.GetBytes(out, "input.#").Int() != 0 {
		t.Fatalf("instructions that mention JSON need no instruction: %s", out)
	}
	out = jsonMode.WriteResponses([]byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"answer in Json"}]}]}`))
	if gjson.GetBytes(out, "input.#").Int() != 1 {
		t.Fatalf("an input that mentions JSON needs no instruction: %s", out)
	}
	out = jsonMode.WriteResponses([]byte(`{"input":"weather?"}`))
	if !strings.Contains(gjson.GetBytes(out, "instructions").String(), "JSON") {
		t.Fatalf("string input: instruction not added to instructions: %s", out)
	}

	yes := true
	schema := OutputFormat{Kind: OutputFormatJSONSchema, Name: "weather report!", Description: "today", Schema: weatherSchema, Strict: &yes}
	format := gjson.GetBytes(schema.WriteResponses([]byte(`{"input":[]}`)), "text.format")
	if format.Get("type").String() != "json_schema" || format.Get("name").String() != "weather_report_" || format.Get("description").String() != "today" ||
		!format.Get("strict").Bool() || !format.Get("schema.properties.city").Exists() {
		t.Fatalf("text.format = %s", format.Raw)
	}
}

func TestOpenAISchemaName(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "response",
		"weather":               "weather",
		"a.b c":                 "a_b_c",
		strings.Repeat("x", 70): strings.Repeat("x", 64),
	} {
		if got := openAISchemaName(in); got != want {
			t.Errorf("openAISchemaName(%q) = %q, want %q", in, got, want)
		}
	}
}

// claudeStructuredModel takes output_config.format.
const claudeStructuredModel = "claude-sonnet-4-5-20250929"

func TestWriteClaude(t *testing.T) {
	schema := OutputFormat{Kind: OutputFormatJSONSchema, Schema: weatherSchema}
	out := schema.WriteClaude([]byte(`{"model":"m","output_config":{"effort":"high"},"messages":[]}`), claudeStructuredModel)
	if gjson.GetBytes(out, "output_config.effort").String() != "high" {
		t.Fatalf("effort lost: %s", out)
	}
	if gjson.GetBytes(out, "output_config.format.type").String() != "json_schema" || !gjson.GetBytes(out, "output_config.format.schema.properties.city").Exists() {
		t.Fatalf("output_config.format = %s", gjson.GetBytes(out, "output_config.format").Raw)
	}
	if gjson.GetBytes(out, "system").Exists() {
		t.Fatalf("a schema Anthropic compiles needs no instruction: %s", out)
	}

	// No JSON mode: asked in the system prompt, as a text block the cloaking
	// executor keeps.
	jsonMode := OutputFormat{Kind: OutputFormatJSONObject}
	for _, tc := range []struct{ name, request string }{
		{"no system", `{"messages":[]}`},
		{"string system", `{"system":"Be brief.","messages":[]}`},
		{"block system", `{"system":[{"type":"text","text":"Be brief."}],"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := jsonMode.WriteClaude([]byte(tc.request), claudeStructuredModel)
			system := gjson.GetBytes(out, "system")
			if !system.IsArray() {
				t.Fatalf("system is not a list of blocks: %s", system.Raw)
			}
			blocks := system.Array()
			last := blocks[len(blocks)-1]
			if last.Get("type").String() != "text" || !strings.Contains(last.Get("text").String(), "JSON") {
				t.Fatalf("last system block = %s", last.Raw)
			}
			if tc.name != "no system" && blocks[0].Get("text").String() != "Be brief." {
				t.Fatalf("existing system prompt lost: %s", system.Raw)
			}
			if gjson.GetBytes(out, "output_config").Exists() {
				t.Fatalf("json_object must not become output_config: %s", out)
			}
		})
	}

	// A map cannot be compiled: the schema goes into the system prompt.
	mapSchema := OutputFormat{Kind: OutputFormatJSONSchema, Name: "scores", Schema: `{"type":"object","additionalProperties":{"type":"number"}}`}
	out = mapSchema.WriteClaude([]byte(`{"messages":[]}`), claudeStructuredModel)
	if gjson.GetBytes(out, "output_config").Exists() {
		t.Fatalf("map schema sent as output_config: %s", out)
	}
	text := gjson.GetBytes(out, "system.0.text").String()
	if !strings.Contains(text, "JSON Schema") || !strings.Contains(text, `"additionalProperties":{"type":"number"}`) || !strings.Contains(text, "scores") {
		t.Fatalf("system instruction = %q", text)
	}

	// A model that does not take output_config.format, Claude or not, is
	// asked in the system prompt, with effort and the rest left as they were.
	for _, model := range []string{"claude-sonnet-4-20250514", "claude-opus-4-1", "kimi-k2", ""} {
		out := schema.WriteClaude([]byte(`{"model":"m","output_config":{"effort":"high"},"messages":[]}`), model)
		if gjson.GetBytes(out, "output_config.format").Exists() || gjson.GetBytes(out, "output_config.effort").String() != "high" {
			t.Fatalf("%s: output_config = %s", model, gjson.GetBytes(out, "output_config").Raw)
		}
		if text := gjson.GetBytes(out, "system.0.text").String(); !strings.Contains(text, "JSON Schema") || !strings.HasSuffix(text, weatherSchema) {
			t.Fatalf("%s: system instruction = %q", model, text)
		}
	}
}

func TestWriteGemini(t *testing.T) {
	schema := OutputFormat{Kind: OutputFormatJSONSchema, Schema: weatherSchema}
	jsonMode := OutputFormat{Kind: OutputFormatJSONObject}
	const request = `{"contents":[{"role":"model","parts":[{"text":"Hi."}]},{"role":"user","parts":[{"text":"weather?"}]}]}`
	systemText := func(out []byte, prefix string) string {
		instruction := gjson.GetBytes(out, prefix+"systemInstruction")
		if !instruction.Exists() {
			instruction = gjson.GetBytes(out, prefix+"system_instruction")
		}
		var b strings.Builder
		for _, part := range instruction.Get("parts").Array() {
			b.WriteString(part.Get("text").String())
		}
		return b.String()
	}
	for _, tc := range []struct {
		name, model    string
		format         OutputFormat
		mime, inSystem bool
		schemaField    bool
	}{
		{"2.5 schema", "gemini-2.5-pro", schema, true, false, true},
		{"2.5 json mode", "gemini-2.5-pro", jsonMode, true, false, false},
		{"2.0 schema", "gemini-2.0-flash", schema, true, true, false},
		{"2.0 json mode", "gemini-2.0-flash", jsonMode, true, false, false},
		{"claude on antigravity", "claude-sonnet-4-5-thinking", schema, false, true, false},
		{"gpt-oss on antigravity", "gpt-oss-120b-medium", jsonMode, false, true, false},
	} {
		for _, prefix := range []string{"", "request."} {
			t.Run(tc.name+"/"+prefix, func(t *testing.T) {
				in := request
				if prefix != "" {
					in = `{"model":"m","request":` + request + `}`
				}
				out := tc.format.WriteGemini([]byte(in), prefix, tc.model)
				config := gjson.GetBytes(out, prefix+"generationConfig")
				if got := config.Get("responseMimeType").Exists(); got != tc.mime {
					t.Fatalf("responseMimeType written = %v, want %v: %s", got, tc.mime, out)
				}
				if got := config.Get("responseJsonSchema").Exists(); got != tc.schemaField {
					t.Fatalf("responseJsonSchema written = %v, want %v: %s", got, tc.schemaField, out)
				}
				if tc.schemaField {
					if !config.Get("responseJsonSchema.properties.city").Exists() {
						t.Fatalf("schema lost: %s", out)
					}
				}
				if got := systemText(out, prefix) != ""; got != tc.inSystem {
					t.Fatalf("system instruction written = %v, want %v: %s", got, tc.inSystem, out)
				}
				if gjson.GetBytes(out, prefix+"contents.1.parts.#").Int() != 1 {
					t.Fatalf("user turn changed: %s", out)
				}
			})
		}
	}

	// An instruction joins the system instruction the request has, under the
	// spelling it has; a new one is spelled as each target's translators do.
	out := schema.WriteGemini([]byte(`{"system_instruction":{"role":"user","parts":[{"text":"Be brief."}]},"contents":[]}`), "", "claude-sonnet-4-5")
	if parts := gjson.GetBytes(out, "system_instruction.parts").Array(); len(parts) != 2 || parts[0].Get("text").String() != "Be brief." || gjson.GetBytes(out, "systemInstruction").Exists() {
		t.Fatalf("system_instruction = %s", out)
	}
	out = schema.WriteGemini([]byte(`{"request":{"systemInstruction":{"parts":[{"text":"Be brief."}]},"contents":[]}}`), "request.", "gpt-oss-120b-medium")
	if parts := gjson.GetBytes(out, "request.systemInstruction.parts").Array(); len(parts) != 2 || gjson.GetBytes(out, "request.system_instruction").Exists() {
		t.Fatalf("systemInstruction = %s", out)
	}
	if out := jsonMode.WriteGemini([]byte(`{"contents":[]}`), "", "tab_flash_lite_preview"); !gjson.GetBytes(out, "system_instruction.parts.0.text").Exists() {
		t.Fatalf("new system instruction not spelled system_instruction: %s", out)
	}
	if out := jsonMode.WriteGemini([]byte(`{"request":{"contents":[]}}`), "request.", "claude-opus-4-5"); !gjson.GetBytes(out, "request.systemInstruction.parts.0.text").Exists() {
		t.Fatalf("new system instruction not spelled systemInstruction: %s", out)
	}

	// Gemma takes no system instruction: the first user turn starts with it.
	out = schema.WriteGemini([]byte(request), "", "gemma-3-27b-it")
	if gjson.GetBytes(out, "system_instruction").Exists() || gjson.GetBytes(out, "generationConfig").Exists() {
		t.Fatalf("gemma got fields it does not take: %s", out)
	}
	if parts := gjson.GetBytes(out, "contents.1.parts").Array(); len(parts) != 2 || !strings.Contains(parts[0].Get("text").String(), "JSON Schema") || parts[1].Get("text").String() != "weather?" {
		t.Fatalf("gemma user turn = %s", gjson.GetBytes(out, "contents.1").Raw)
	}

	// Image, speech and embedding models answer with no text to constrain.
	for _, model := range []string{"gemini-2.5-flash-image", "gemini-3-pro-image", "gemini-2.5-flash-preview-tts", "gemini-embedding-001"} {
		if out := schema.WriteGemini([]byte(request), "", model); string(out) != request {
			t.Fatalf("%s: request changed: %s", model, out)
		}
	}
	// Plain text asks for nothing.
	if out := (OutputFormat{Kind: OutputFormatText}).WriteGemini([]byte(request), "", "claude-sonnet-4-5"); string(out) != request {
		t.Fatalf("text changed the request: %s", out)
	}
}

func TestAnthropicOutputSchemaRewrite(t *testing.T) {
	raw := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"title":"Report",
		"type":"object",
		"properties":{
			"zone":{"type":"string","format":"date-time"},
			"site":{"type":"string","format":"uri-reference","pattern":"^https"},
			"score":{"type":"integer","description":"Out of ten","minimum":0,"maximum":10},
			"tags":{"type":"array","items":{"type":"string","maxLength":20},"minItems":1,"maxItems":5},
			"pair":{"type":"array","items":{"type":"number"},"minItems":2},
			"owner":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},
			"kind":{"oneOf":[{"type":"string"},{"type":"null"}]},
			"x-note":{"type":"string"}
		},
		"required":["zone","site","score","tags","pair","owner","kind","x-note"],
		"x-vendor":true
	}`
	rewritten, ok := anthropicOutputSchema(raw)
	if !ok {
		t.Fatal("schema was not rewritten")
	}
	got := gjson.Parse(rewritten)
	for path, want := range map[string]string{
		"additionalProperties":                  `false`,
		"properties.owner.additionalProperties": `false`,
		"properties.zone.format":                `"date-time"`,
		"properties.tags.minItems":              `1`,
		"properties.kind.anyOf.1.type":          `"null"`,
		"title":                                 `"Report"`,
	} {
		if g := got.Get(path).Raw; g != want {
			t.Errorf("%s = %s, want %s", path, g, want)
		}
	}
	for path, want := range map[string]string{
		"properties.site.description":       `{format: "uri-reference", pattern: "^https"}`,
		"properties.score.description":      "Out of ten\n\n{minimum: 0, maximum: 10}",
		"properties.tags.description":       `{maxItems: 5}`,
		"properties.tags.items.description": `{maxLength: 20}`,
		"properties.pair.description":       `{minItems: 2}`,
	} {
		if g := got.Get(path).String(); g != want {
			t.Errorf("%s = %q, want %q", path, g, want)
		}
	}
	for _, gone := range []string{"$schema", "x-vendor", "properties.kind.oneOf", "properties.site.format", "properties.score.minimum", "properties.pair.minItems"} {
		if got.Get(strings.ReplaceAll(gone, "$", `\$`)).Exists() {
			t.Errorf("%s is still there: %s", gone, rewritten)
		}
	}
	// Annotations about the document are dropped, not noted in a description.
	if got.Get("description").Exists() {
		t.Errorf("root description = %s, want none", got.Get("description").Raw)
	}
	// A property named like a vendor extension is still a property.
	if !got.Get("properties.x-note").Exists() {
		t.Errorf("property x-note dropped: %s", rewritten)
	}
	var order []string
	got.Get("properties").ForEach(func(key, _ gjson.Result) bool {
		order = append(order, key.String())
		return true
	})
	if strings.Join(order, ",") != "zone,site,score,tags,pair,owner,kind,x-note" {
		t.Errorf("property order changed: %v", order)
	}
}

func TestAnthropicOutputSchemaDefinitions(t *testing.T) {
	// definitions (draft 7) arrive as $defs, with every reference moved.
	rewritten, ok := anthropicOutputSchema(`{"type":"object","properties":{"home":{"$ref":"#/definitions/Address","description":"dropped"}},"required":["home"],"definitions":{"Address":{"type":"object","properties":{"street":{"type":"string"}},"required":["street"]}}}`)
	if !ok {
		t.Fatal("schema with definitions was not rewritten")
	}
	got := gjson.Parse(rewritten)
	if ref := got.Get(`properties.home.\$ref`).String(); ref != "#/$defs/Address" {
		t.Fatalf("$ref = %q: %s", ref, rewritten)
	}
	if got.Get("properties.home.description").Exists() {
		t.Fatalf("siblings of $ref are not sent: %s", rewritten)
	}
	if got.Get(`\$defs.Address.additionalProperties`).Raw != "false" || got.Get("definitions").Exists() {
		t.Fatalf("definitions not moved to $defs and closed: %s", rewritten)
	}

	// A root that is a reference to an object is still an object.
	if _, ok := anthropicOutputSchema(`{"$ref":"#/$defs/Root","$defs":{"Root":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}}`); !ok {
		t.Fatal("root reference to an object was refused")
	}
}

func TestAnthropicOutputSchemaRefusals(t *testing.T) {
	// many writes an object with n properties of type kind, all required or none.
	many := func(n int, kind string, required bool) string {
		var props, names []string
		for i := 0; i < n; i++ {
			name := "p" + strings.Repeat("x", i)
			props = append(props, `"`+name+`":{"type":`+kind+`}`)
			names = append(names, `"`+name+`"`)
		}
		req := "[]"
		if required {
			req = "[" + strings.Join(names, ",") + "]"
		}
		return `{"type":"object","properties":{` + strings.Join(props, ",") + `},"required":` + req + `}`
	}
	for name, raw := range map[string]string{
		"root recursion":        `{"type":"object","properties":{"child":{"$ref":"#"}},"required":["child"]}`,
		"definition cycle":      `{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},"B":{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}}}}}`,
		"dangling reference":    `{"type":"object","properties":{"a":{"$ref":"#/$defs/Missing"}}}`,
		"external reference":    `{"type":"object","properties":{"a":{"$ref":"https://example.com/a.json"}}}`,
		"map":                   `{"type":"object","properties":{"scores":{"type":"object","additionalProperties":{"type":"number"}}}}`,
		"free-form object":      `{"type":"object","properties":{"meta":{"type":"object"}}}`,
		"open root":             `{"type":"object"}`,
		"tuple":                 `{"type":"object","properties":{"point":{"type":"array","items":[{"type":"number"},{"type":"number"}]}}}`,
		"array root":            `{"type":"array","items":{"type":"string"}}`,
		"boolean schema":        `{"type":"object","properties":{"anything":true}}`,
		"nested definitions":    `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"string"}},"$defs":{"X":{"type":"string"}}}}}`,
		"too many optional":     many(25, `"string"`, false),
		"too many union params": many(17, `["string","null"]`, true),
	} {
		if rewritten, ok := anthropicOutputSchema(raw); ok {
			t.Errorf("%s: accepted as %s", name, rewritten)
		}
	}
	// At the limits it is still sent.
	if _, ok := anthropicOutputSchema(many(16, `["string","null"]`, true)); !ok {
		t.Error("16 union parameters refused")
	}
	if _, ok := anthropicOutputSchema(many(24, `"string"`, false)); !ok {
		t.Error("24 optional parameters refused")
	}
}
