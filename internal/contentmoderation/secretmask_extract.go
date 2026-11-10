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
//   - The tools the request declares: their descriptions and those of their
//     parameters, however deep the schema nests them. The model reads them as
//     instructions; a key pasted into one went upstream as it came.
//
// Neither is added to what the word guard reads: its policies were written
// against what it reads, and a tenant's banned words are not meant to refuse
// a tool schema. The protocol of the request itself (the ids, types and
// signatures of its messages and blocks) and text a signature covers are left
// as they were.

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

// toolDescriptions reads, for masking, the descriptions of the tools root
// declares: "tools" in every format, and the "functions" of Chat Completions
// before tools. Nothing else of a tool is read: its name and schema are
// structure the upstream checks, not prose.
func (x *extraction) toolDescriptions(root gjson.Result) {
	if !x.secrets {
		return
	}
	x.descriptions(root.Get("tools"), 0)
	x.descriptions(root.Get("functions"), 0)
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
