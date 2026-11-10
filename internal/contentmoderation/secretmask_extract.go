package contentmoderation

import (
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// What privacy masking reads of a request besides what the word guard reads.
//
// Masking walks a request the way the word guard does (extractWordUnits), and
// reads two more kinds of text the model reads:
//
//   - The data of a tool call or result: the arguments of a call, the input of
//     a Claude tool_use, the args and the response of a Gemini function call.
//     That JSON is the caller's own, so none of its keys is the protocol's:
//     "id", "type", "status" or "signature" there names a field of the
//     caller's data, and an object whose "type" is "image" is not a media
//     block. The word guard leaves the values under those keys alone, which is
//     right for the request's own messages and blocks, but here it sent
//     {"id": "<an API key>"} upstream as it came.
//   - What the request declares besides its messages: the tools it offers the
//     model, and the schema it asks the reply to follow. Their descriptions,
//     and those of every property, however deep the schema nests them: the
//     model reads them as instructions, and a key pasted into one went
//     upstream as it came.
//
// Neither is added to what the word guard reads: its policies were written
// against what it reads, and a tenant's banned words are not meant to refuse
// a tool or a reply schema. The protocol of the request itself (the ids,
// types and signatures of its messages and blocks) and text a signature
// covers are left as they were.

// extractSecretUnits is extractWordUnits for privacy masking.
func extractSecretUnits(format sdktranslator.Format, payload []byte) []wordUnit {
	x := &extraction{locate: true, secrets: true, doc: &wordDoc{data: payload, located: true}}
	if !x.run(format, payload) {
		return nil
	}
	return x.units
}

// toolData reads the data of a tool call or result: as jsonText does for the
// word guard, and with no key taken for the protocol's for masking.
func (x *extraction) toolData(value gjson.Result, class Class) {
	if !x.secrets {
		x.jsonText(value, class)
		return
	}
	x.jsonTextBy(class, func(visit jsonTextVisitor) { walkToolData(value, 0, visit) })
}

// walkToolData is walkJSONText for the data of a tool call or result. Inline
// media is still left out: a data: URL is base64, not text, and rewriting a
// match inside it would break the file.
func walkToolData(value gjson.Result, depth int, visit jsonTextVisitor) {
	switch {
	case value.Type == gjson.String:
		text := value.String()
		if strings.HasPrefix(text, "data:") && strings.Contains(text, ";base64,") {
			return
		}
		visit(text, value, jsonTextValue, gjson.Result{})
	case value.IsArray() || value.IsObject():
		if depth >= maxJSONTextDepth {
			visit(value.Raw, value, jsonTextRaw, gjson.Result{})
			return
		}
		value.ForEach(func(key, item gjson.Result) bool {
			if key.Type == gjson.String {
				visit(key.String(), key, jsonTextKey, value)
			}
			walkToolData(item, depth+1, visit)
			return true
		})
	}
}

// declaredDescriptions reads, for masking, the descriptions in what root
// declares: the tools it offers and the schema of the reply it asks for
// (declaredFields). Nothing else of them is read: names, types and
// constraints are structure the upstream checks, not prose.
//
// The fields are found in one walk of root, not looked up one by one: a
// lookup of a field that is not there scans the whole request, megabytes for
// one that carries screenshots, and a lookup finds only the first copy of a
// field the request repeats, where the upstream's parser may take the last.
func (x *extraction) declaredDescriptions(root gjson.Result) {
	if !x.secrets {
		return
	}
	root.ForEach(func(key, value gjson.Result) bool {
		if key.Type != gjson.String {
			return true
		}
		under, declared := declaredFields[key.String()]
		switch {
		case !declared:
		case under == "":
			x.descriptions(value, 0)
		default:
			value.ForEach(func(key, value gjson.Result) bool {
				if key.Type == gjson.String && key.String() == under {
					x.descriptions(value, 0)
				}
				return true
			})
		}
		return true
	})
}

// declaredFields are the fields of a request that declare tools or a reply
// schema, each with the field of it that holds the declaration, "" for the
// field itself. Each is read in every format, as tools always were: a request
// passed through to an upstream of its own format keeps the fields it
// carries, its format's or not.
var declaredFields = map[string]string{
	// The tools: "tools" in every format, and the "functions" of Chat
	// Completions before tools.
	"tools":     "",
	"functions": "",
	// The reply's schema, where the translators read it
	// (translator/common/outputformat.go), which a model reads whether it is
	// sent as a constraint or written into the prompt instead. Chat
	// Completions puts the schema and a description of it in
	// response_format.json_schema; the Responses API puts both in
	// text.format beside the type.
	"response_format": "",
	"text":            "format",
	// Anthropic's output_config.format, and output_format, which it replaced
	// and the API still takes under a beta header.
	"output_config": "format",
	"output_format": "",
	// Gemini's responseJsonSchema or responseSchema. Google's parser takes
	// camelCase and snake_case names alike, and nothing else in
	// generationConfig has a description, so all of generationConfig is read,
	// in both spellings, rather than each spelling of each schema field.
	"generationConfig":  "",
	"generation_config": "",
}

// descriptions reads every string under a "description" key in value. A
// parameter named "description" holds a schema, not a string, and is walked
// into like any other.
func (x *extraction) descriptions(value gjson.Result, depth int) {
	if !value.IsObject() && !value.IsArray() {
		return
	}
	if depth >= maxJSONTextDepth {
		// Read whole and not rewritable, as jsonText does: a value found down
		// there refuses the request instead of going upstream.
		x.recordUnit(ClassPrimary, false, value.Raw, value, gjson.Result{}, unitOpaque)
		return
	}
	value.ForEach(func(key, item gjson.Result) bool {
		if key.Type == gjson.String && key.String() == "description" && item.Type == gjson.String {
			x.addValue(ClassPrimary, false, item)
			return true
		}
		x.descriptions(item, depth+1)
		return true
	})
}
