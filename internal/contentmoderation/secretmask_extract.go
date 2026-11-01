package contentmoderation

import (
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// extractSecretUnits shares the conversation walk with word policies, while
// treating arbitrary tool data as data rather than a protocol envelope. Actual
// signed thinking and protocol metadata retain the existing exclusions.
func extractSecretUnits(format sdktranslator.Format, payload []byte) []wordUnit {
	x := &extraction{locate: true, secrets: true, doc: &wordDoc{data: payload, located: true}}
	if !x.run(format, payload) {
		return nil
	}
	return x.units
}

func (x *extraction) secretToolData(value gjson.Result, class Class) {
	x.jsonTextMode(value, class, x.secrets)
}

// Tool schemas/names are not rewritten wholesale: only their descriptive text
// is prose for the model. This extra coverage is not added to word policies.
func (x *extraction) secretToolDescriptions(value gjson.Result, depth int) {
	if !value.IsObject() && !value.IsArray() {
		return
	}
	if depth >= maxJSONTextDepth {
		x.recordUnit(ClassPrimary, false, value.Raw, value, gjson.Result{}, unitOpaque)
		return
	}
	value.ForEach(func(key, item gjson.Result) bool {
		if key.String() == "description" && item.Type == gjson.String {
			x.addValue(ClassPrimary, false, item)
		} else if item.IsObject() || item.IsArray() {
			x.secretToolDescriptions(item, depth+1)
		}
		return true
	})
}

// Unlike a message/content-block envelope, ordinary tool arguments/results may
// use id, type, signature or encrypted_content as business keys. None of these
// makes the value opaque or signed; media-looking objects here are data too.
func walkSecretToolData(value gjson.Result, depth int, visit func(string, gjson.Result, jsonTextKind, gjson.Result)) {
	switch {
	case value.Type == gjson.String:
		visit(value.String(), value, jsonTextValue, gjson.Result{})
	case value.IsArray() || value.IsObject():
		if depth >= maxJSONTextDepth {
			visit(value.Raw, value, jsonTextRaw, gjson.Result{})
			return
		}
		value.ForEach(func(key, item gjson.Result) bool {
			if key.Type == gjson.String {
				visit(key.String(), key, jsonTextKey, value)
			}
			walkSecretToolData(item, depth+1, visit)
			return true
		})
	}
}
