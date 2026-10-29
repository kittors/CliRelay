package common

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Every protocol lets a client ask for its reply as JSON, each in its own
// field: Chat Completions in response_format, the Responses API in
// text.format, Anthropic in output_config.format, and Gemini in
// generationConfig, with responseMimeType and responseJsonSchema or
// responseSchema. A translator that does not read the field drops the
// requirement without a word, and a client that parses the reply as JSON
// gets prose back.
//
// Every request translator reads the requirement with one of the
// OutputFormatFrom* functions and writes it with one of the OutputFormat
// Write* methods, so every pair of protocols carries it the same way. Where
// the target protocol has no field for what was asked (Anthropic has no JSON
// mode, and compiles only a subset of JSON Schema), the requirement goes into
// the system prompt instead: the model is then asked rather than constrained,
// which is still what the client asked for, where dropping it is not.

// OutputFormatKind is the shape of reply a request asks for.
type OutputFormatKind int

const (
	// OutputFormatNone is a request that does not say: plain text.
	OutputFormatNone OutputFormatKind = iota
	// OutputFormatText is a request that explicitly asks for plain text.
	OutputFormatText
	// OutputFormatJSONObject asks for any valid JSON object (JSON mode).
	OutputFormatJSONObject
	// OutputFormatJSONSchema asks for JSON that conforms to Schema.
	OutputFormatJSONSchema
)

// OutputFormat is a request's structured output requirement, in terms every
// protocol can express.
type OutputFormat struct {
	Kind OutputFormatKind
	// Name and Description label the schema. Only Chat Completions and the
	// Responses API carry them, and both require a name.
	Name        string
	Description string
	// Schema is the JSON Schema, as raw JSON, for OutputFormatJSONSchema.
	Schema string
	// Strict is the client's strict flag (Chat Completions and Responses API
	// only), nil when it did not say. It is not invented for other clients:
	// OpenAI's strict mode rejects schemas with optional properties, which
	// Anthropic and Gemini schemas may have.
	Strict *bool
}

// jsonOnlyInstruction asks for a JSON object where the target protocol has no
// field for it. It also carries the word "json", which OpenAI requires
// somewhere in the conversation before it accepts JSON mode.
const jsonOnlyInstruction = "Respond with only a valid JSON object. Do not wrap it in Markdown code fences or add any text before or after it."

// OutputFormatFromChat reads a Chat Completions request's response_format.
func OutputFormatFromChat(root gjson.Result) OutputFormat {
	format := root.Get("response_format")
	return outputFormatFromOpenAI(format.Get("type").String(), format.Get("json_schema"))
}

// OutputFormatFromResponses reads a Responses API request's text.format,
// which carries the json_schema fields beside its type.
func OutputFormatFromResponses(root gjson.Result) OutputFormat {
	format := root.Get("text.format")
	return outputFormatFromOpenAI(format.Get("type").String(), format)
}

func outputFormatFromOpenAI(kind string, spec gjson.Result) OutputFormat {
	switch kind {
	case "text":
		return OutputFormat{Kind: OutputFormatText}
	case "json_object":
		return OutputFormat{Kind: OutputFormatJSONObject}
	case "json_schema":
		schema := spec.Get("schema")
		if !schema.IsObject() {
			// Both APIs require a schema here. Without one, JSON mode is the
			// closest thing the client can still be given.
			return OutputFormat{Kind: OutputFormatJSONObject}
		}
		format := OutputFormat{
			Kind:        OutputFormatJSONSchema,
			Name:        spec.Get("name").String(),
			Description: spec.Get("description").String(),
			Schema:      schema.Raw,
		}
		if strict := spec.Get("strict"); strict.IsBool() {
			value := strict.Bool()
			format.Strict = &value
		}
		return format
	}
	return OutputFormat{}
}

// OutputFormatFromClaude reads an Anthropic request's output_config.format,
// or the top-level output_format it replaced, which the API still takes
// under a beta header. Anthropic has no JSON mode: json_schema is its only
// format.
func OutputFormatFromClaude(root gjson.Result) OutputFormat {
	format := root.Get("output_config.format")
	if !format.Exists() {
		format = root.Get("output_format")
	}
	if format.Get("type").String() != "json_schema" {
		return OutputFormat{}
	}
	if schema := format.Get("schema"); schema.IsObject() {
		return OutputFormat{Kind: OutputFormatJSONSchema, Schema: schema.Raw}
	}
	return OutputFormat{}
}

// OutputFormatFromGemini reads a Gemini request's generationConfig: a JSON
// responseMimeType, with responseJsonSchema (JSON Schema) or responseSchema
// (Gemini's OpenAPI subset) when the reply has a schema. Google's JSON
// parser takes camelCase and snake_case field names alike, so both are read.
func OutputFormatFromGemini(root gjson.Result) OutputFormat {
	config := firstExisting(root, "generationConfig", "generation_config")
	mime := strings.ToLower(strings.TrimSpace(firstExisting(config, "responseMimeType", "response_mime_type").String()))
	if mime != "" && mime != "application/json" {
		// text/plain, or text/x.enum: a bare enum value rather than JSON,
		// which no other protocol has a way to ask for.
		return OutputFormat{}
	}
	if schema := firstExisting(config, "responseJsonSchema", "response_json_schema"); schema.IsObject() {
		return OutputFormat{Kind: OutputFormatJSONSchema, Schema: schema.Raw}
	}
	if schema := firstExisting(config, "responseSchema", "response_schema"); schema.IsObject() {
		return OutputFormat{Kind: OutputFormatJSONSchema, Schema: jsonSchemaFromGemini(schema)}
	}
	if mime == "application/json" {
		return OutputFormat{Kind: OutputFormatJSONObject}
	}
	return OutputFormat{}
}

func firstExisting(node gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if value := node.Get(path); value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

// WriteChat writes f into a Chat Completions request as response_format. A
// request for plain text writes nothing: it is the default, and some
// OpenAI-compatible upstreams reject response_format altogether.
func (f OutputFormat) WriteChat(out []byte) []byte {
	switch f.Kind {
	case OutputFormatJSONObject:
		out, _ = sjson.SetRawBytes(out, "response_format", []byte(`{"type":"json_object"}`))
		return ensureChatMentionsJSON(out)
	case OutputFormatJSONSchema:
		out, _ = sjson.SetRawBytes(out, "response_format", []byte(`{"type":"json_schema","json_schema":`+f.openAISchemaSpec(false)+`}`))
	}
	return out
}

// ResponsesFormat returns f as a Responses API text.format object, for a
// translator that writes its envelope by hand; ok is false when f asks for
// nothing. Unlike WriteResponses it does not check that a JSON mode request
// mentions JSON, which only matters for OutputFormatJSONObject.
func (f OutputFormat) ResponsesFormat() (format string, ok bool) {
	switch f.Kind {
	case OutputFormatText:
		return `{"type":"text"}`, true
	case OutputFormatJSONObject:
		return `{"type":"json_object"}`, true
	case OutputFormatJSONSchema:
		return f.openAISchemaSpec(true), true
	}
	return "", false
}

// WriteResponses writes f into a Responses API request as text.format,
// keeping the other text settings (verbosity) that are already there.
func (f OutputFormat) WriteResponses(out []byte) []byte {
	format, ok := f.ResponsesFormat()
	if !ok {
		return out
	}
	out, _ = sjson.SetRawBytes(out, "text.format", []byte(format))
	if f.Kind == OutputFormatJSONObject {
		out = ensureResponsesMentionsJSON(out)
	}
	return out
}

// WriteClaude writes f into an Anthropic request: a schema as
// output_config.format when Anthropic can compile it, and everything else
// into the system prompt.
func (f OutputFormat) WriteClaude(out []byte) []byte {
	switch f.Kind {
	case OutputFormatJSONSchema:
		if schema, ok := anthropicOutputSchema(f.Schema); ok {
			// output_config may already carry the effort; only format is set.
			out, _ = sjson.SetRawBytes(out, "output_config.format", []byte(`{"type":"json_schema","schema":`+schema+`}`))
			return out
		}
		return appendClaudeSystemText(out, f.schemaInstruction())
	case OutputFormatJSONObject:
		return appendClaudeSystemText(out, jsonOnlyInstruction)
	}
	return out
}

// WriteGemini writes f into a Gemini request's generationConfig. prefix is
// "request." for Gemini CLI and Antigravity, which wrap the Gemini body.
// The schema goes in responseJsonSchema, which takes JSON Schema as every
// other protocol writes it; responseSchema would need Gemini's OpenAPI
// subset.
func (f OutputFormat) WriteGemini(out []byte, prefix string) []byte {
	if f.Kind != OutputFormatJSONObject && f.Kind != OutputFormatJSONSchema {
		return out
	}
	out, _ = sjson.SetBytes(out, prefix+"generationConfig.responseMimeType", "application/json")
	if f.Kind == OutputFormatJSONSchema {
		out, _ = sjson.SetRawBytes(out, prefix+"generationConfig.responseJsonSchema", []byte(f.Schema))
	}
	return out
}

// openAISchemaSpec writes the json_schema object Chat Completions nests under
// response_format.json_schema, or, with withType, the text.format object of
// the Responses API, which carries the same fields beside its type.
func (f OutputFormat) openAISchemaSpec(withType bool) string {
	var b strings.Builder
	b.WriteByte('{')
	if withType {
		b.WriteString(`"type":"json_schema",`)
	}
	b.WriteString(`"name":`)
	b.WriteString(jsonString(openAISchemaName(f.Name)))
	if f.Description != "" {
		b.WriteString(`,"description":`)
		b.WriteString(jsonString(f.Description))
	}
	b.WriteString(`,"schema":`)
	b.WriteString(f.Schema)
	if f.Strict != nil {
		if *f.Strict {
			b.WriteString(`,"strict":true`)
		} else {
			b.WriteString(`,"strict":false`)
		}
	}
	b.WriteByte('}')
	return b.String()
}

// openAISchemaName returns a schema name OpenAI accepts: it requires one, of
// at most 64 letters, digits, underscores and dashes. Clients of other
// protocols have no name to give.
func openAISchemaName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if b.Len() == 64 {
			break
		}
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "response"
	}
	return b.String()
}

// schemaInstruction asks for JSON that conforms to the schema, for a target
// that cannot be given the schema itself.
func (f OutputFormat) schemaInstruction() string {
	var b strings.Builder
	b.WriteString("Respond with only a JSON value that conforms to the JSON Schema below. Do not wrap it in Markdown code fences or add any text before or after it.")
	if f.Name != "" || f.Description != "" {
		b.WriteString("\n\nThe value is ")
		if f.Name != "" {
			b.WriteString(f.Name)
			if f.Description != "" {
				b.WriteString(": ")
			}
		}
		b.WriteString(f.Description)
	}
	b.WriteString("\n\n")
	b.WriteString(f.Schema)
	return b.String()
}

// appendClaudeSystemText adds a text block at the end of an Anthropic system
// prompt. It writes a block rather than a string: with cloaking on, the Claude
// executor keeps the text blocks it finds in system but replaces a plain
// string with its own prompt.
func appendClaudeSystemText(out []byte, text string) []byte {
	block := `{"type":"text","text":` + jsonString(text) + `}`
	system := gjson.GetBytes(out, "system")
	switch {
	case system.IsArray():
		out, _ = sjson.SetRawBytes(out, "system.-1", []byte(block))
	case system.Type == gjson.String && system.String() != "":
		out, _ = sjson.SetRawBytes(out, "system", []byte(`[{"type":"text","text":`+jsonString(system.String())+`},`+block+`]`))
	default:
		out, _ = sjson.SetRawBytes(out, "system", []byte(`[`+block+`]`))
	}
	return out
}

// OpenAI rejects JSON mode unless the word "json" appears in the
// conversation, and so does the Responses API. A client of another protocol
// never had to write it, so a request that lacks it is given an instruction
// that has it.

func ensureChatMentionsJSON(out []byte) []byte {
	mentions := false
	gjson.GetBytes(out, "messages").ForEach(func(_, message gjson.Result) bool {
		mentions = mentionsJSON(message.Get("content"))
		return !mentions
	})
	if mentions {
		return out
	}
	return prependArrayItem(out, "messages", `{"role":"system","content":`+jsonString(jsonOnlyInstruction)+`}`)
}

func ensureResponsesMentionsJSON(out []byte) []byte {
	if mentionsJSON(gjson.GetBytes(out, "instructions")) {
		return out
	}
	input := gjson.GetBytes(out, "input")
	if !input.IsArray() {
		if mentionsJSON(input) {
			return out
		}
		instructions := gjson.GetBytes(out, "instructions").String()
		if instructions != "" {
			instructions += "\n\n"
		}
		out, _ = sjson.SetBytes(out, "instructions", instructions+jsonOnlyInstruction)
		return out
	}
	mentions := false
	input.ForEach(func(_, item gjson.Result) bool {
		mentions = mentionsJSON(item.Get("content"))
		return !mentions
	})
	if mentions {
		return out
	}
	return prependArrayItem(out, "input", `{"type":"message","role":"developer","content":[{"type":"input_text","text":`+jsonString(jsonOnlyInstruction)+`}]}`)
}

// mentionsJSON reports whether a message content, a string or a list of
// parts, contains the word "json" in any case.
func mentionsJSON(content gjson.Result) bool {
	if content.Type == gjson.String {
		return strings.Contains(strings.ToLower(content.String()), "json")
	}
	found := false
	content.ForEach(func(_, part gjson.Result) bool {
		found = strings.Contains(strings.ToLower(part.Get("text").String()), "json")
		return !found
	})
	return found
}

// prependArrayItem puts item first in the array at path, creating the array
// when there is none.
func prependArrayItem(out []byte, path, item string) []byte {
	array := gjson.GetBytes(out, path)
	if !array.IsArray() {
		out, _ = sjson.SetRawBytes(out, path, []byte(`[`+item+`]`))
		return out
	}
	inner := strings.TrimSpace(array.Raw[1 : len(array.Raw)-1])
	if inner == "" {
		out, _ = sjson.SetRawBytes(out, path, []byte(`[`+item+`]`))
		return out
	}
	out, _ = sjson.SetRawBytes(out, path, []byte(`[`+item+`,`+inner+`]`))
	return out
}

func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
