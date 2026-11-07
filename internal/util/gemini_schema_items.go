package util

import (
	"strings"

	"github.com/tidwall/gjson"
)

// addMissingArrayItems gives every array schema that declares no "items" an empty one.
//
// Antigravity only takes the protobuf `parameters` field (its executor renames
// `parametersJsonSchema` back to it), and that schema requires items on an array: one
// argument declared as a bare {"type":"array"} fails the whole request with
// "...parameters.properties[x].items: missing field." JSON Schema allows the bare form,
// where it means any element, and MCP servers emit it routinely.
//
// {} keeps that meaning. Upstream CLIProxyAPI fills {"type":"string"} instead, which
// declares an array of objects to be an array of strings; Antigravity's Gemini and Claude
// models were both measured to accept {}.
//
// Runs after flattenTypeArrays, so a ["array","null"] list is a plain "array" by now.
// Request contents are left alone: a function call replayed there can hold
// "type": "array" as data.
func addMissingArrayItems(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "type") {
		if strings.HasPrefix(p, "contents.") || strings.HasPrefix(p, "request.contents.") {
			continue
		}
		parentPath := trimSuffix(p, ".type")
		// A property named "type" is a definition, not the keyword.
		if isPropertyDefinition(parentPath) {
			continue
		}
		declared := gjson.Get(jsonStr, p)
		if declared.Type != gjson.String || !strings.EqualFold(declared.String(), "array") {
			continue
		}
		itemsPath := joinPath(parentPath, "items")
		if gjson.Get(jsonStr, itemsPath).Exists() {
			continue
		}
		jsonStr = setRawAt(jsonStr, itemsPath, "{}")
	}
	return jsonStr
}
