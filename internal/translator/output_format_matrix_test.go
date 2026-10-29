package translator

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
	"github.com/tidwall/gjson"
)

// This matrix sends one structured output requirement, a JSON Schema and a
// bare JSON object, through every registered request translator and checks
// that the upstream request still asks for it: in the target protocol's own
// field where it has one, and in the system prompt where it has none (an
// Anthropic request cannot ask for a bare JSON object). Anthropic has no
// json_object form, so a Claude client only sends the schema.

const matrixOutputSchema = `{"type":"object","properties":{"city":{"type":"string","description":"City name"},"temp_c":{"type":"number"}},"required":["city","temp_c"],"additionalProperties":false}`

// matrixFormats are the protocol names the registry knows.
var matrixFormats = []string{"claude", "codex", "openai", "openai-response", "gemini", "gemini-cli", "antigravity"}

// structuredRequest builds a request in the client protocol that asks for
// kind ("json_schema" or "json_object"); ok is false when the protocol has
// no way to say it.
type structuredRequest func(kind string) (body string, ok bool)

var structuredRequests = map[string]structuredRequest{
	"claude": func(kind string) (string, bool) {
		if kind != "json_schema" {
			return "", false
		}
		return `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"What is the weather?"}],"output_config":{"format":{"type":"json_schema","schema":` + matrixOutputSchema + `}}}`, true
	},
	"openai": func(kind string) (string, bool) {
		format := `{"type":"json_object"}`
		if kind == "json_schema" {
			format = `{"type":"json_schema","json_schema":{"name":"weather","strict":true,"schema":` + matrixOutputSchema + `}}`
		}
		return `{"model":"m","messages":[{"role":"user","content":"What is the weather?"}],"response_format":` + format + `}`, true
	},
	"openai-response": func(kind string) (string, bool) {
		format := `{"type":"json_object"}`
		if kind == "json_schema" {
			format = `{"type":"json_schema","name":"weather","strict":true,"schema":` + matrixOutputSchema + `}`
		}
		return `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather?"}]}],"text":{"format":` + format + `}}`, true
	},
	"gemini": func(kind string) (string, bool) {
		return geminiStructuredRequest(kind), true
	},
	"gemini-cli": func(kind string) (string, bool) {
		return `{"model":"m","request":` + geminiStructuredRequest(kind) + `}`, true
	},
}

func geminiStructuredRequest(kind string) string {
	config := `{"responseMimeType":"application/json"}`
	if kind == "json_schema" {
		config = `{"responseMimeType":"application/json","responseJsonSchema":` + matrixOutputSchema + `}`
	}
	return `{"contents":[{"role":"user","parts":[{"text":"What is the weather?"}]}],"generationConfig":` + config + `}`
}

// structuredCheck asserts the translated request still asks for kind.
type structuredCheck func(t *testing.T, out []byte, kind string)

var structuredChecks = map[string]structuredCheck{
	"claude":      checkClaudeStructured,
	"codex":       checkResponsesStructured,
	"openai":      checkChatStructured,
	"gemini":      checkGeminiStructured(""),
	"gemini-cli":  checkGeminiStructured("request."),
	"antigravity": checkGeminiStructured("request."),
}

// assertMatrixSchema checks the schema arrived with its properties.
func assertMatrixSchema(t *testing.T, field string, schema gjson.Result) {
	t.Helper()
	if got := schema.Get("properties.city.type").String(); got != "string" {
		t.Fatalf("%s lost the schema: %s", field, schema.Raw)
	}
	if !schema.Get("properties.temp_c").Exists() {
		t.Fatalf("%s lost a property: %s", field, schema.Raw)
	}
}

// claudeSystemText joins the system prompt, which Anthropic takes as a
// string or as text blocks.
func claudeSystemText(out []byte) string {
	system := gjson.GetBytes(out, "system")
	if system.Type == gjson.String {
		return system.String()
	}
	var b strings.Builder
	for _, block := range system.Array() {
		b.WriteString(block.Get("text").String())
		b.WriteByte('\n')
	}
	return b.String()
}

func checkClaudeStructured(t *testing.T, out []byte, kind string) {
	t.Helper()
	if kind == "json_schema" {
		format := gjson.GetBytes(out, "output_config.format")
		if got := format.Get("type").String(); got != "json_schema" {
			t.Fatalf("output_config.format.type = %q, want json_schema: %s", got, out)
		}
		assertMatrixSchema(t, "output_config.format.schema", format.Get("schema"))
		return
	}
	// Anthropic has no JSON mode: the requirement goes into the system prompt.
	if format := gjson.GetBytes(out, "output_config.format"); format.Exists() {
		t.Fatalf("json_object must not reach Anthropic as output_config.format: %s", format.Raw)
	}
	if system := claudeSystemText(out); !strings.Contains(system, "JSON") {
		t.Fatalf("system prompt does not ask for JSON: %q", system)
	}
}

func checkResponsesStructured(t *testing.T, out []byte, kind string) {
	t.Helper()
	format := gjson.GetBytes(out, "text.format")
	if got := format.Get("type").String(); got != kind {
		t.Fatalf("text.format.type = %q, want %s: %s", got, kind, out)
	}
	if kind == "json_schema" {
		if format.Get("name").String() == "" {
			t.Fatalf("text.format.name is required by the Responses API: %s", format.Raw)
		}
		assertMatrixSchema(t, "text.format.schema", format.Get("schema"))
	}
}

func checkChatStructured(t *testing.T, out []byte, kind string) {
	t.Helper()
	format := gjson.GetBytes(out, "response_format")
	if got := format.Get("type").String(); got != kind {
		t.Fatalf("response_format.type = %q, want %s: %s", got, kind, out)
	}
	if kind == "json_schema" {
		if format.Get("json_schema.name").String() == "" {
			t.Fatalf("response_format.json_schema.name is required by Chat Completions: %s", format.Raw)
		}
		assertMatrixSchema(t, "response_format.json_schema.schema", format.Get("json_schema.schema"))
	}
}

func checkGeminiStructured(prefix string) structuredCheck {
	return func(t *testing.T, out []byte, kind string) {
		t.Helper()
		config := gjson.GetBytes(out, prefix+"generationConfig")
		if got := config.Get("responseMimeType").String(); got != "application/json" {
			t.Fatalf("%sgenerationConfig.responseMimeType = %q, want application/json: %s", prefix, got, out)
		}
		if kind != "json_schema" {
			return
		}
		schema := config.Get("responseJsonSchema")
		if !schema.Exists() {
			schema = config.Get("responseSchema")
		}
		assertMatrixSchema(t, prefix+"generationConfig.responseJsonSchema", schema)
	}
}

func TestStructuredOutputSurvivesEveryRequestTranslator(t *testing.T) {
	paths := 0
	for _, from := range matrixFormats {
		for _, to := range matrixFormats {
			if !translator.NeedConvert(from, to) {
				continue
			}
			paths++
			build, check := structuredRequests[from], structuredChecks[to]
			if build == nil || check == nil {
				t.Errorf("%s -> %s is registered but not covered by this matrix", from, to)
				continue
			}
			for _, kind := range []string{"json_schema", "json_object"} {
				body, ok := build(kind)
				if !ok {
					continue
				}
				for _, stream := range []bool{false, true} {
					name := from + "->" + to + "/" + kind
					if stream {
						name += "/stream"
					}
					t.Run(name, func(t *testing.T) {
						out := translator.Request(from, to, "m", []byte(body), stream)
						if !gjson.ValidBytes(out) {
							t.Fatalf("translated request is not valid JSON: %.300s", out)
						}
						check(t, out, kind)
					})
				}
			}
		}
	}
	// Every request translator registered today: 27 pairs.
	if paths < 27 {
		t.Fatalf("only %d translator paths exercised", paths)
	}
}

// Gemini SDKs write the schema in Gemini's OpenAPI subset: responseSchema,
// with upper-case type names. Other protocols take JSON Schema, so it must
// arrive there with JSON Schema's lower-case names.
func TestGeminiResponseSchemaArrivesAsJSONSchema(t *testing.T) {
	request := `{"contents":[{"role":"user","parts":[{"text":"What is the weather?"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"OBJECT","properties":{"city":{"type":"STRING","description":"City name"},"temp_c":{"type":"NUMBER"}},"required":["city","temp_c"],"propertyOrdering":["city","temp_c"]}}}`
	for _, to := range []string{"claude", "codex", "openai"} {
		t.Run(to, func(t *testing.T) {
			out := translator.Request("gemini", to, "m", []byte(request), false)
			structuredChecks[to](t, out, "json_schema")
		})
	}
}

// A schema Anthropic cannot compile (here a map) is not sent as
// output_config.format, which would fail the request with a 400: the model
// is asked for it in the system prompt instead.
func TestUncompilableSchemaReachesAnthropicAsInstruction(t *testing.T) {
	request := `{"model":"m","messages":[{"role":"user","content":"Score the teams."}],"response_format":{"type":"json_schema","json_schema":{"name":"scores","schema":{"type":"object","additionalProperties":{"type":"number"}}}}}`
	out := translator.Request("openai", "claude", "m", []byte(request), false)
	if format := gjson.GetBytes(out, "output_config.format"); format.Exists() {
		t.Fatalf("uncompilable schema sent as output_config.format: %s", format.Raw)
	}
	system := claudeSystemText(out)
	if !strings.Contains(system, "JSON Schema") || !strings.Contains(system, `"additionalProperties":{"type":"number"}`) {
		t.Fatalf("system prompt does not carry the schema: %q", system)
	}
}
