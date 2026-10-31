package contentmoderation

import (
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// ExtractModerationInput returns every text fragment of a request that the
// upstream model will read, classified (see Class) and in conversation order.
//
// It must cover the whole request, not just the newest turn: a client controls
// the entire payload, so moderating only the last message lets an attacker put
// banned text in an earlier turn (or in a system prompt) and append an empty or
// image-only final message to slip past pre_block. The same holds for text no
// person typed: tool results, documents and the assistant history are all
// written by the client and can carry anything. Only binary payloads (images,
// audio, files) and opaque signatures are left out.
func ExtractModerationInput(format sdktranslator.Format, payload []byte) Input {
	x := &extraction{}
	if !x.run(format, payload) {
		return NewInput(nil)
	}
	return NewInput(x.segments)
}

// extractWordUnits reads a request the way ExtractModerationInput does, and
// also records where each piece of text sits in the payload, so the word guard
// can rewrite it in place. The classification and the order are the same by
// construction: it is one walk.
func extractWordUnits(format sdktranslator.Format, payload []byte) []wordUnit {
	x := &extraction{locate: true, payload: payload}
	if !x.run(format, payload) {
		return nil
	}
	return x.units
}

// run walks payload in the layout of format and reports whether it was JSON.
func (x *extraction) run(format sdktranslator.Format, payload []byte) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	root := gjson.ParseBytes(payload)
	switch format {
	case sdktranslator.FormatOpenAIResponse:
		x.responses(root)
	case sdktranslator.FormatClaude:
		x.claude(root)
	case sdktranslator.FormatGemini:
		x.gemini(root)
	case sdktranslator.FormatGeminiCLI:
		// v1internal wraps the generateContent body in "request". Anything at
		// the top level is read too, before it, so the conversation in
		// "request" stays the newest text.
		if request := root.Get("request"); request.IsObject() {
			x.gemini(root)
			root = request
		}
		x.gemini(root)
	default:
		x.openAI(root)
	}
	return true
}

type extraction struct {
	segments []Segment
	turn     int
	// claudeReminders reclassifies Claude Code's <system-reminder> blocks; see add.
	claudeReminders bool
	// locate makes the walk also record wordUnits. payload is the document the
	// gjson results index into, which a unit's position is checked against.
	locate  bool
	payload []byte
	units   []wordUnit
	// nested is above zero while reading JSON that was found inside a string
	// (tool-call arguments): its offsets belong to the decoded string, not to
	// the payload, so its text can be matched but not rewritten.
	nested int
	// signed is set while reading text a signature covers (see wordUnit.signed).
	signed bool
}

func (x *extraction) nextTurn() { x.turn++ }

func (x *extraction) add(class Class, user bool, text string) {
	text = collapseSpaces(text)
	if text == "" {
		return
	}
	class, user = x.classify(class, user, text)
	x.segments = append(x.segments, Segment{Class: class, User: user, Turn: x.turn, Text: text})
}

// classify applies the one reclassification the extractor makes on the text
// itself. text is whitespace-collapsed.
func (x *extraction) classify(class Class, user bool, text string) (Class, bool) {
	if x.claudeReminders && class == ClassPrimary && strings.HasPrefix(text, "<system-reminder>") {
		// Claude Code injects <system-reminder> blocks into user turns: context
		// the client attaches, often large, not what the user typed. They used
		// to be skipped outright, which let any caller hide text behind the
		// prefix. As tool output they get keyword checks, reach the backend only
		// on opt-in, and are never taken for the newest user message.
		return ClassToolOutput, false
	}
	return class, user
}

// addValue adds the text of a JSON string value, and when locating records
// the value's place in the payload so the word guard can rewrite it. A value
// that is not a string adds nothing, as before.
func (x *extraction) addValue(class Class, user bool, value gjson.Result) {
	text := stringValue(value)
	x.add(class, user, text)
	if x.locate {
		x.recordUnit(class, user, text, value, unitText)
	}
}

// roleClass maps a message role onto a class. Roles that are neither caller
// prose nor assistant output (tool, function, anything unknown) are treated
// as tool output, so a made-up role cannot hide text from keyword checks.
func roleClass(role string) (Class, bool) {
	switch role {
	case "user", "human":
		return ClassPrimary, true
	case "system", "developer":
		return ClassPrimary, false
	case "assistant", "model":
		return ClassAssistantHistory, false
	default:
		return ClassToolOutput, false
	}
}

// openAI reads Chat Completions and the endpoints sharing its format.
func (x *extraction) openAI(root gjson.Result) {
	for _, message := range arrayItems(root.Get("messages")) {
		x.nextTurn()
		class, user := roleClass(normalizedField(message, "role"))
		x.content(message.Get("content"), class, user)
		if class != ClassAssistantHistory {
			continue
		}
		x.addValue(class, false, message.Get("refusal"))
		x.addValue(class, false, message.Get("reasoning_content"))
		for _, call := range arrayItems(message.Get("tool_calls")) {
			x.arguments(call.Get("function.arguments"), class)
		}
		x.arguments(message.Get("function_call.arguments"), class)
	}
	// Image, video and legacy completion requests carry the prompt at the top
	// level. Legacy completions also take an array of prompts; arrays of token
	// ids carry no text that can be read without the model's tokenizer.
	x.nextTurn()
	prompt := root.Get("prompt")
	if prompt.IsArray() {
		for _, item := range prompt.Array() {
			x.addValue(ClassPrimary, true, item)
		}
	} else {
		x.addValue(ClassPrimary, true, prompt)
	}
	x.addValue(ClassPrimary, true, root.Get("suffix"))
}

// claude reads the Anthropic Messages format.
func (x *extraction) claude(root gjson.Result) {
	x.claudeReminders = true
	// Claude carries the system prompt outside messages; it is caller-controlled too.
	x.content(root.Get("system"), ClassPrimary, false)
	for _, message := range arrayItems(root.Get("messages")) {
		x.nextTurn()
		class, user := roleClass(normalizedField(message, "role"))
		x.content(message.Get("content"), class, user)
	}
}

// responses reads the OpenAI Responses format.
func (x *extraction) responses(root gjson.Result) {
	// Instructions are the system prompt, so they come first in conversation
	// order: placed last, they would pass for the newest message.
	instructions := root.Get("instructions")
	if instructions.IsArray() {
		for _, item := range instructions.Array() {
			x.responsesItem(item)
		}
	} else {
		x.addValue(ClassPrimary, false, instructions)
	}
	// Prompt template variables are substituted into the prompt the model reads.
	if variables := root.Get("prompt.variables"); variables.IsObject() {
		x.nextTurn()
		variables.ForEach(func(_, value gjson.Result) bool {
			x.content(value, ClassPrimary, false)
			return true
		})
	}
	input := root.Get("input")
	switch {
	case input.IsArray():
		for _, item := range input.Array() {
			x.responsesItem(item)
		}
	case input.Exists():
		x.responsesItem(input)
	}
}

func (x *extraction) responsesItem(item gjson.Result) {
	x.nextTurn()
	if item.Type == gjson.String {
		x.addValue(ClassPrimary, true, item)
		return
	}
	switch typeName := normalizedField(item, "type"); {
	case typeName == "" || typeName == "message":
		class, user := roleClass(normalizedField(item, "role"))
		x.content(item.Get("content"), class, user)
	case typeName == "input_text":
		// A bare content block in the input list is caller input.
		x.content(item, ClassPrimary, true)
	case typeName == "item_reference":
		// Points at stored output by id; carries no text.
	case typeName == "reasoning":
		x.content(item.Get("summary"), ClassAssistantHistory, false)
		x.content(item.Get("content"), ClassAssistantHistory, false)
	case typeName == "mcp_call":
		x.arguments(item.Get("arguments"), ClassAssistantHistory)
		x.content(item.Get("output"), ClassToolOutput, false)
		x.addValue(ClassToolOutput, false, item.Get("error"))
	case strings.HasSuffix(typeName, "_output"):
		// function_call_output, custom_tool_call_output and the built-in tools'
		// outputs: a string, a list of content blocks, or an object.
		x.content(item.Get("output"), ClassToolOutput, false)
	case strings.HasSuffix(typeName, "_call"):
		// function_call, custom_tool_call and built-in tool calls: model output
		// the client replays, so it can be fabricated like any other turn.
		x.arguments(item.Get("arguments"), ClassAssistantHistory)
		item.ForEach(func(key, value gjson.Result) bool {
			if name := key.String(); name != "arguments" && name != "result" && !protocolKeys[name] {
				// "result" of image_generation_call is the image itself.
				x.jsonText(value, ClassAssistantHistory)
			}
			return true
		})
	default:
		x.jsonText(item, ClassToolOutput)
	}
}

// gemini reads the Gemini generateContent format.
func (x *extraction) gemini(root gjson.Result) {
	// The REST API accepts snake_case field names as well as lowerCamelCase.
	x.geminiParts(firstField(root, "systemInstruction", "system_instruction"), ClassPrimary, false)
	for _, content := range arrayItems(root.Get("contents")) {
		x.nextTurn()
		role := normalizedField(content, "role")
		if role == "" {
			// Single-turn requests omit the role; they are user input.
			role = "user"
		}
		class, user := roleClass(role)
		x.geminiParts(content, class, user)
	}
}

func (x *extraction) geminiParts(content gjson.Result, class Class, user bool) {
	for _, part := range arrayItems(content.Get("parts")) {
		x.addValue(class, user, part.Get("text"))
		if response := firstField(part, "functionResponse", "function_response"); response.Exists() {
			// Arbitrary JSON the model reads serialized, keys included.
			x.jsonText(response.Get("response"), ClassToolOutput)
		}
		if call := firstField(part, "functionCall", "function_call"); call.Exists() {
			x.jsonText(call.Get("args"), ClassAssistantHistory)
		}
		if code := firstField(part, "executableCode", "executable_code"); code.Exists() {
			x.addValue(ClassAssistantHistory, false, code.Get("code"))
		}
		if result := firstField(part, "codeExecutionResult", "code_execution_result"); result.Exists() {
			x.addValue(ClassToolOutput, false, result.Get("output"))
		}
	}
}

// binaryBlockTypes are content blocks with no text to read: media, files, and
// encrypted reasoning.
var binaryBlockTypes = map[string]bool{
	"image": true, "image_url": true, "input_image": true, "input_audio": true, "audio": true,
	"file": true, "input_file": true, "redacted_thinking": true, "computer_screenshot": true,
	"container_upload": true,
}

// protocolKeys hold identifiers, signatures and encrypted blobs, not text a
// model reads as prose.
var protocolKeys = map[string]bool{
	"type": true, "id": true, "call_id": true, "tool_use_id": true, "status": true,
	"signature": true, "encrypted_content": true, "cache_control": true,
}

// content walks message content: a string, a list of content blocks, or one
// block. class and user describe the enclosing message; blocks that carry
// another kind of text (tool results, documents, tool calls) reclassify it.
func (x *extraction) content(value gjson.Result, class Class, user bool) {
	switch {
	case value.Type == gjson.String:
		x.addValue(class, user, value)
	case value.IsArray():
		for _, block := range value.Array() {
			x.content(block, class, user)
		}
	case value.IsObject():
		x.block(value, class, user)
	}
}

func (x *extraction) block(block gjson.Result, class Class, user bool) {
	switch typeName := normalizedField(block, "type"); typeName {
	case "", "text", "input_text", "output_text", "message", "summary_text", "reasoning_text":
		text, content := block.Get("text"), block.Get("content")
		if typeName == "" && !text.Exists() && !content.Exists() {
			// Not a text block: tool output in a shape of its own.
			x.jsonText(block, class)
			return
		}
		x.addValue(class, user, text)
		x.content(content, class, user)
	case "refusal":
		x.addValue(class, user, block.Get("refusal"))
	case "thinking":
		x.addValue(ClassAssistantHistory, false, block.Get("thinking"))
	case "tool_use", "server_tool_use", "mcp_tool_use":
		x.jsonText(block.Get("input"), ClassAssistantHistory)
	case "tool_result":
		// Claude: the content is a string or a list of blocks.
		x.content(block.Get("content"), ClassToolOutput, false)
	case "document":
		x.addValue(ClassToolOutput, false, block.Get("title"))
		x.addValue(ClassToolOutput, false, block.Get("context"))
		// Plain-text sources carry their text inline and content sources hold
		// blocks; base64 PDFs and URLs carry nothing readable here.
		source := block.Get("source")
		switch normalizedField(source, "type") {
		case "text":
			x.addValue(ClassToolOutput, false, source.Get("data"))
		case "content":
			x.content(source.Get("content"), ClassToolOutput, false)
		}
	case "search_result":
		x.addValue(ClassToolOutput, false, block.Get("title"))
		x.content(block.Get("content"), ClassToolOutput, false)
	default:
		if binaryBlockTypes[typeName] {
			return
		}
		if strings.HasSuffix(typeName, "_tool_result") {
			// Server tool results the client replays (web search, code execution).
			x.jsonText(block.Get("content"), ClassToolOutput)
			return
		}
		// An unknown block type is read whole rather than trusted: a new or
		// invented type must not be a way around moderation.
		x.jsonText(block, class)
	}
}

// arguments reads tool-call arguments, which usually arrive JSON-encoded in a
// string. They are decoded first: in the encoded form a JSON escape such as
// \u4f60 (for 你) hides a keyword from a substring scan.
func (x *extraction) arguments(value gjson.Result, class Class) {
	if value.Type != gjson.String {
		x.jsonText(value, class)
		return
	}
	raw := value.String()
	if decoded := gjson.Parse(raw); gjson.Valid(raw) && (decoded.IsObject() || decoded.IsArray()) {
		// The decoded document is a different text from the payload, so its
		// offsets mean nothing there: its words are matched, not rewritten.
		x.nested++
		x.jsonText(decoded, class)
		x.nested--
		return
	}
	x.addValue(class, false, value)
}

// maxJSONTextDepth bounds the recursion of jsonText. Deeper values are read as
// raw JSON instead of being skipped.
const maxJSONTextDepth = 64

// jsonText reads every string in a JSON value as one fragment, object keys
// included: tool arguments and function responses reach the model serialized,
// so a key is as visible to it as a value.
func (x *extraction) jsonText(value gjson.Result, class Class) {
	var b strings.Builder
	walkJSONText(value, 0, false, func(text string, at gjson.Result, kind jsonTextKind) {
		b.WriteString(text)
		b.WriteByte(' ')
		if !x.locate || kind == jsonTextProtocol {
			return
		}
		unit := unitText
		if kind != jsonTextValue {
			// A key or a whole deep subtree: matched, but changing it would change
			// the structure of the document, not just its words.
			unit = unitOpaque
		}
		x.recordUnit(class, false, text, at, unit)
	})
	x.add(class, false, b.String())
}

// jsonTextKind says what walkJSONText found.
type jsonTextKind uint8

const (
	// jsonTextValue is a string value.
	jsonTextValue jsonTextKind = iota
	// jsonTextProtocol is a string value under one of protocolKeys: an
	// identifier or a type tag. It is read as text for keyword checks, as it
	// always was, but it is not prose a person wrote.
	jsonTextProtocol
	// jsonTextKey is an object key.
	jsonTextKey
	// jsonTextRaw is a subtree below maxJSONTextDepth, read as raw JSON.
	jsonTextRaw
)

// walkJSONText visits the text in a JSON value in document order. at is the
// JSON value the text was read from (the key itself for a key).
func walkJSONText(value gjson.Result, depth int, protocol bool, visit func(text string, at gjson.Result, kind jsonTextKind)) {
	switch {
	case value.Type == gjson.String:
		text := value.String()
		// Inline media is base64, not text.
		if strings.HasPrefix(text, "data:") && strings.Contains(text, ";base64,") {
			return
		}
		if protocol {
			visit(text, value, jsonTextProtocol)
			return
		}
		visit(text, value, jsonTextValue)
	case value.IsArray() || value.IsObject():
		if depth >= maxJSONTextDepth {
			visit(value.Raw, value, jsonTextRaw)
			return
		}
		if value.IsObject() && binaryBlockTypes[normalizedField(value, "type")] {
			return
		}
		value.ForEach(func(key, item gjson.Result) bool {
			underProtocolKey := false
			if key.Type == gjson.String {
				name := key.String()
				if name == "signature" || name == "encrypted_content" {
					return true
				}
				visit(name, key, jsonTextKey)
				underProtocolKey = protocolKeys[name]
			}
			walkJSONText(item, depth+1, protocol || underProtocolKey, visit)
			return true
		})
	}
}

func arrayItems(value gjson.Result) []gjson.Result {
	if !value.IsArray() {
		return nil
	}
	return value.Array()
}

func stringValue(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return value.String()
}

func normalizedField(value gjson.Result, field string) string {
	return strings.ToLower(strings.TrimSpace(value.Get(field).String()))
}

// firstField returns the first of the given fields that is present.
func firstField(value gjson.Result, names ...string) gjson.Result {
	for _, name := range names {
		if field := value.Get(name); field.Exists() {
			return field
		}
	}
	return gjson.Result{}
}
