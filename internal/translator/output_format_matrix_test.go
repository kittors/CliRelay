package translator

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
	"github.com/tidwall/gjson"
)

// This matrix sends one structured output requirement, a JSON Schema and a
// bare JSON object, through every registered request translator, for models
// that take the target's native field and models that do not, and checks
// that the upstream request still asks for it: in the target protocol's own
// field where the model is known to take it, and in the system prompt
// everywhere else (an Anthropic request cannot ask for a bare JSON object,
// and OpenAI-compatible Chat upstreams are always asked). Anthropic has no
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

// expectation is how a target model should be sent the requirement.
type expectation int

const (
	// expectNative is the target's own field.
	expectNative expectation = iota
	// expectPrompt is an instruction in the system prompt and nothing else.
	expectPrompt
	// expectJSONMode is Gemini before 2.5: responseMimeType, and a schema
	// asked for in the system instruction.
	expectJSONMode
)

// targetModel is a model behind a target and how it should be sent the
// requirement.
type targetModel struct {
	model  string
	expect expectation
}

// targetModels pairs, for every target, models that take its native field
// with models that do not. Codex has no model without text.format, and
// OpenAI-compatible Chat upstreams are asked whatever the model, gpt-4o
// included: the relay does not know what is behind a channel.
var targetModels = map[string][]targetModel{
	"claude":      {{"claude-sonnet-4-5-20250929", expectNative}, {"claude-sonnet-4-20250514", expectPrompt}, {"kimi-k2", expectPrompt}},
	"codex":       {{"gpt-5-codex", expectNative}, {"gpt-5.1-codex-mini", expectNative}},
	"openai":      {{"gpt-4o", expectPrompt}, {"deepseek-chat", expectPrompt}},
	"gemini":      {{"gemini-2.5-pro", expectNative}, {"gemini-2.0-flash", expectJSONMode}},
	"gemini-cli":  {{"gemini-3-pro-preview", expectNative}, {"gemini-2.0-flash", expectJSONMode}},
	"antigravity": {{"gemini-3.1-pro-high", expectNative}, {"claude-sonnet-4-5-thinking", expectPrompt}, {"gpt-oss-120b-medium", expectPrompt}},
}

// passThrough are the registered pairs that share one wire schema: the
// translator only re-wraps the body, and the client's own field goes through
// as the client wrote it, whatever the model. Nothing is gated there.
var passThrough = map[[2]string]bool{
	{"openai", "openai"}:         true,
	{"openai-response", "codex"}: true,
	{"gemini", "gemini"}:         true,
	{"gemini", "gemini-cli"}:     true,
	{"gemini-cli", "gemini"}:     true,
	{"gemini", "antigravity"}:    true,
}

// structuredCheck asserts the translated request still asks for kind, the
// way expect says.
type structuredCheck func(t *testing.T, out []byte, kind string, expect expectation)

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

// instructionMarker begins every instruction the translators write.
const instructionMarker = "Respond with only"

// assertInstruction checks that prompt asks for kind: a JSON object, or JSON
// that conforms to the schema, which ends the instruction.
func assertInstruction(t *testing.T, where, prompt, kind string) {
	t.Helper()
	at := strings.Index(prompt, instructionMarker)
	if at < 0 {
		t.Fatalf("%s does not ask for JSON: %q", where, prompt)
	}
	instruction := prompt[at:]
	if kind != "json_schema" {
		if !strings.Contains(instruction, "JSON object") {
			t.Fatalf("%s does not ask for a JSON object: %q", where, instruction)
		}
		return
	}
	if !strings.Contains(instruction, "JSON Schema") {
		t.Fatalf("%s does not ask for the schema: %q", where, instruction)
	}
	schema := instruction[strings.LastIndex(instruction, "\n\n")+2:]
	assertMatrixSchema(t, where, gjson.Parse(strings.TrimSpace(schema)))
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

func checkClaudeStructured(t *testing.T, out []byte, kind string, expect expectation) {
	t.Helper()
	format := gjson.GetBytes(out, "output_config.format")
	system := claudeSystemText(out)
	if kind == "json_schema" && expect == expectNative {
		if got := format.Get("type").String(); got != "json_schema" {
			t.Fatalf("output_config.format.type = %q, want json_schema: %s", got, out)
		}
		assertMatrixSchema(t, "output_config.format.schema", format.Get("schema"))
		if strings.Contains(system, instructionMarker) {
			t.Fatalf("schema sent natively is also asked for: %q", system)
		}
		return
	}
	// Anthropic has no JSON mode, and a model without the field is asked.
	if format.Exists() {
		t.Fatalf("output_config.format sent: %s", format.Raw)
	}
	assertInstruction(t, "system prompt", system, kind)
}

func checkResponsesStructured(t *testing.T, out []byte, kind string, _ expectation) {
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

func checkChatStructured(t *testing.T, out []byte, kind string, expect expectation) {
	t.Helper()
	format := gjson.GetBytes(out, "response_format")
	if expect == expectNative {
		// Only on the Chat pass-through, where the client wrote it.
		if got := format.Get("type").String(); got != kind {
			t.Fatalf("response_format.type = %q, want %s: %s", got, kind, out)
		}
		if kind == "json_schema" {
			assertMatrixSchema(t, "response_format.json_schema.schema", format.Get("json_schema.schema"))
		}
		return
	}
	if format.Exists() {
		t.Fatalf("response_format sent to an OpenAI-compatible upstream: %s", format.Raw)
	}
	first := gjson.GetBytes(out, "messages.0")
	if first.Get("role").String() != "system" {
		t.Fatalf("first message is not a system message: %s", out)
	}
	content := first.Get("content")
	text := content.String()
	if content.IsArray() {
		var b strings.Builder
		for _, part := range content.Array() {
			b.WriteString(part.Get("text").String())
		}
		text = b.String()
	}
	assertInstruction(t, "system message", text, kind)
}

// geminiSystemText joins the parts of a Gemini system instruction, under
// either spelling.
func geminiSystemText(out []byte, prefix string) string {
	instruction := gjson.GetBytes(out, prefix+"systemInstruction")
	if !instruction.Exists() {
		instruction = gjson.GetBytes(out, prefix+"system_instruction")
	}
	var b strings.Builder
	for _, part := range instruction.Get("parts").Array() {
		b.WriteString(part.Get("text").String())
		b.WriteByte('\n')
	}
	return b.String()
}

func checkGeminiStructured(prefix string) structuredCheck {
	return func(t *testing.T, out []byte, kind string, expect expectation) {
		t.Helper()
		config := gjson.GetBytes(out, prefix+"generationConfig")
		mime := config.Get("responseMimeType").String()
		schema := config.Get("responseJsonSchema")
		system := geminiSystemText(out, prefix)
		switch expect {
		case expectNative:
			if mime != "application/json" {
				t.Fatalf("%sgenerationConfig.responseMimeType = %q, want application/json: %s", prefix, mime, out)
			}
			if kind == "json_schema" {
				assertMatrixSchema(t, prefix+"generationConfig.responseJsonSchema", schema)
			}
			if strings.Contains(system, instructionMarker) {
				t.Fatalf("requirement sent natively is also asked for: %q", system)
			}
		case expectJSONMode:
			if mime != "application/json" {
				t.Fatalf("%sgenerationConfig.responseMimeType = %q, want application/json: %s", prefix, mime, out)
			}
			if schema.Exists() || config.Get("responseSchema").Exists() {
				t.Fatalf("schema sent to a model before Gemini 2.5: %s", config.Raw)
			}
			if kind == "json_schema" {
				assertInstruction(t, "system instruction", system, kind)
			} else if strings.Contains(system, instructionMarker) {
				t.Fatalf("JSON mode sent natively is also asked for: %q", system)
			}
		case expectPrompt:
			if config.Get("responseMimeType").Exists() || schema.Exists() {
				t.Fatalf("native fields sent to a model that is not Gemini: %s", config.Raw)
			}
			assertInstruction(t, "system instruction", system, kind)
		}
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
			build, check, models := structuredRequests[from], structuredChecks[to], targetModels[to]
			if build == nil || check == nil || len(models) == 0 {
				t.Errorf("%s -> %s is registered but not covered by this matrix", from, to)
				continue
			}
			for _, kind := range []string{"json_schema", "json_object"} {
				body, ok := build(kind)
				if !ok {
					continue
				}
				for _, target := range models {
					expect := target.expect
					if passThrough[[2]string{from, to}] {
						expect = expectNative
					}
					for _, stream := range []bool{false, true} {
						name := from + "->" + to + "/" + kind + "/" + target.model
						if stream {
							name += "/stream"
						}
						t.Run(name, func(t *testing.T) {
							out := translator.Request(from, to, target.model, []byte(body), stream)
							if !gjson.ValidBytes(out) {
								t.Fatalf("translated request is not valid JSON: %.300s", out)
							}
							check(t, out, kind, expect)
						})
					}
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
		for _, target := range targetModels[to] {
			t.Run(to+"/"+target.model, func(t *testing.T) {
				out := translator.Request("gemini", to, target.model, []byte(request), false)
				structuredChecks[to](t, out, "json_schema", target.expect)
			})
		}
	}
}

// A schema Anthropic cannot compile (here a map) is not sent as
// output_config.format, which would fail the request with a 400: the model
// is asked for it in the system prompt instead.
func TestUncompilableSchemaReachesAnthropicAsInstruction(t *testing.T) {
	request := `{"model":"m","messages":[{"role":"user","content":"Score the teams."}],"response_format":{"type":"json_schema","json_schema":{"name":"scores","schema":{"type":"object","additionalProperties":{"type":"number"}}}}}`
	out := translator.Request("openai", "claude", "claude-opus-4-5", []byte(request), false)
	if format := gjson.GetBytes(out, "output_config.format"); format.Exists() {
		t.Fatalf("uncompilable schema sent as output_config.format: %s", format.Raw)
	}
	system := claudeSystemText(out)
	if !strings.Contains(system, "JSON Schema") || !strings.Contains(system, `"additionalProperties":{"type":"number"}`) {
		t.Fatalf("system prompt does not carry the schema: %q", system)
	}
}
