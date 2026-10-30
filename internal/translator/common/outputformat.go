package common

import (
	"encoding/json"
	"strconv"
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
// Write* methods, so every pair of protocols carries it the same way.
//
// The target's own field is only written where it is known to be taken: an
// upstream that does not know it fails the request with a 400, and the
// request used to be served with the requirement dropped. So Anthropic gets
// output_config.format, and Gemini responseJsonSchema, only for the models
// documented to take them (outputformat_models.go); OpenAI-compatible Chat
// upstreams never get response_format; Codex always gets text.format. Every
// other case, and what a target cannot express at all (Anthropic has no JSON
// mode and compiles only a subset of JSON Schema), goes into the system
// prompt: the model is then asked rather than constrained, which is still
// what the client asked for, where dropping it is not.

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

// WriteChat writes f into a Chat Completions request for an OpenAI-compatible
// upstream, as an instruction in the system message. It never writes
// response_format: the upstreams behind these channels take it unevenly
// (DeepSeek only json_object, some reject the field outright), the relay does
// not know which one a channel leads to, and a field the upstream rejects
// fails a request that would otherwise be served. A request for plain text
// writes nothing.
func (f OutputFormat) WriteChat(out []byte) []byte {
	if text := f.instruction(); text != "" {
		return appendChatSystemText(out, text)
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
		return f.responsesSchemaFormat(), true
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

// WriteClaude writes f into an Anthropic request for model: a schema as
// output_config.format when the model takes the field and Anthropic can
// compile the schema, and everything else into the system prompt.
func (f OutputFormat) WriteClaude(out []byte, model string) []byte {
	if f.Kind == OutputFormatJSONSchema && claudeModelTakesOutputFormat(model) {
		if schema, ok := anthropicOutputSchema(f.Schema); ok {
			// output_config may already carry the effort; only format is set.
			out, _ = sjson.SetRawBytes(out, "output_config.format", []byte(`{"type":"json_schema","schema":`+schema+`}`))
			return out
		}
	}
	if text := f.instruction(); text != "" {
		return appendClaudeSystemText(out, text)
	}
	return out
}

// WriteGemini writes f into a Gemini request for model. prefix is "request."
// for Gemini CLI and Antigravity, which wrap the Gemini body. Gemini 2.5 and
// later get responseMimeType and responseJsonSchema, which takes JSON Schema
// as every other protocol writes it (responseSchema would need Gemini's
// OpenAPI subset); earlier Gemini models get responseMimeType, with a schema
// asked for in systemInstruction; other models are asked in the prompt.
// geminiOutputSupport has the exceptions: Gemma, and models that answer with
// images, speech or embeddings.
func (f OutputFormat) WriteGemini(out []byte, prefix, model string) []byte {
	text := f.instruction()
	if text == "" {
		return out
	}
	switch geminiStructuredOutputSupport(model) {
	case geminiJSONSchema:
		out, _ = sjson.SetBytes(out, prefix+"generationConfig.responseMimeType", "application/json")
		if f.Kind == OutputFormatJSONSchema {
			out, _ = sjson.SetRawBytes(out, prefix+"generationConfig.responseJsonSchema", []byte(f.Schema))
		}
	case geminiJSONMode:
		out, _ = sjson.SetBytes(out, prefix+"generationConfig.responseMimeType", "application/json")
		if f.Kind == OutputFormatJSONSchema {
			out = appendGeminiSystemText(out, prefix, text)
		}
	case geminiAskInSystem:
		out = appendGeminiSystemText(out, prefix, text)
	case geminiAskInUserTurn:
		out = prependGeminiUserText(out, prefix, text)
	}
	return out
}

// responsesSchemaFormat writes the text.format object of the Responses API for
// a schema: its name, description, schema and strict flag beside the type.
func (f OutputFormat) responsesSchemaFormat() string {
	var b strings.Builder
	b.WriteString(`{"type":"json_schema","name":`)
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

// openAISchemaName returns a schema name the Responses API accepts: it
// requires one, of at most 64 letters, digits, underscores and dashes.
// Clients of other protocols have no name to give.
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

// instruction is f put into words, for a target that is asked rather than
// given a field; empty when f asks for nothing beyond plain text.
func (f OutputFormat) instruction() string {
	switch f.Kind {
	case OutputFormatJSONObject:
		return jsonOnlyInstruction
	case OutputFormatJSONSchema:
		return f.schemaInstruction()
	}
	return ""
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

// appendChatSystemText adds text to the system message that opens a Chat
// Completions conversation: at the end of its content when it has one (a new
// paragraph of a string, a new text part of a list), or as a system message
// put first.
func appendChatSystemText(out []byte, text string) []byte {
	first := gjson.GetBytes(out, "messages.0")
	if first.Get("role").String() != "system" {
		return prependArrayItem(out, "messages", `{"role":"system","content":`+jsonString(text)+`}`)
	}
	content := first.Get("content")
	switch {
	case content.IsArray():
		out, _ = sjson.SetRawBytes(out, "messages.0.content.-1", []byte(`{"type":"text","text":`+jsonString(text)+`}`))
	case content.Type == gjson.String && content.String() != "":
		out, _ = sjson.SetBytes(out, "messages.0.content", content.String()+"\n\n"+text)
	default:
		out, _ = sjson.SetBytes(out, "messages.0.content", text)
	}
	return out
}

// appendGeminiSystemText adds text as a part at the end of a Gemini request's
// system instruction, under either spelling the request already uses, or in
// a new one spelled the way the target's translators write it.
func appendGeminiSystemText(out []byte, prefix, text string) []byte {
	part := []byte(`{"text":` + jsonString(text) + `}`)
	for _, key := range []string{"systemInstruction", "system_instruction"} {
		instruction := gjson.GetBytes(out, prefix+key)
		if !instruction.IsObject() {
			continue
		}
		if instruction.Get("parts").IsArray() {
			out, _ = sjson.SetRawBytes(out, prefix+key+".parts.-1", part)
		} else {
			out, _ = sjson.SetRawBytes(out, prefix+key+".parts", append(append([]byte{'['}, part...), ']'))
		}
		return out
	}
	// The Gemini translators write system_instruction; the Gemini CLI and
	// Antigravity ones systemInstruction, which the Antigravity executor
	// reads back when it adds its own.
	key := "system_instruction"
	if prefix != "" {
		key = "systemInstruction"
	}
	out, _ = sjson.SetRawBytes(out, prefix+key, []byte(`{"role":"user","parts":[`+string(part)+`]}`))
	return out
}

// prependGeminiUserText puts text as the first part of the first user turn,
// for a model that takes no system instruction.
func prependGeminiUserText(out []byte, prefix, text string) []byte {
	part := `{"text":` + jsonString(text) + `}`
	turn := -1
	gjson.GetBytes(out, prefix+"contents").ForEach(func(key, content gjson.Result) bool {
		if role := content.Get("role").String(); role == "" || role == "user" {
			turn = int(key.Int())
			return false
		}
		return true
	})
	if turn < 0 {
		return prependArrayItem(out, prefix+"contents", `{"role":"user","parts":[`+part+`]}`)
	}
	return prependArrayItem(out, prefix+"contents."+strconv.Itoa(turn)+".parts", part)
}

// The Responses API rejects JSON mode unless the word "json" appears in the
// conversation. A client of another protocol never had to write it, so a
// request that lacks it is given an instruction that has it.
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
