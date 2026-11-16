package common

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// LowerToolSchemaTypes lower-cases the value of every "type" key inside the
// "tools" value of out, for the Gemini -> Claude and Gemini -> Codex
// translators: Gemini clients write schema types in upper case.
//
// The tools value is edited on its own and written back once. Editing
// "tools.<path>" on the whole request, as those translators did, copied the
// request, history included, once per type in every tool schema. sjson edits
// a nested path by recursing into the tools value and copying the bytes
// around it, so the result is byte for byte the same, including for a schema
// property that is itself named "type".
func LowerToolSchemaTypes(out string) string {
	var paths []string
	tools := gjson.Get(out, "tools")
	util.Walk(tools, "", "type", &paths)
	if len(paths) == 0 {
		return out
	}
	raw := tools.Raw
	for _, p := range paths {
		raw, _ = sjson.Set(raw, p, strings.ToLower(gjson.Get(raw, p).String()))
	}
	out, _ = sjson.SetRaw(out, "tools", raw)
	return out
}
