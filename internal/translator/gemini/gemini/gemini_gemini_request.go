// Package gemini provides in-provider request normalization for Gemini API.
// It ensures incoming v1beta requests meet minimal schema requirements
// expected by Google's Generative Language API.
package gemini

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
)

// ConvertGeminiRequestToGemini normalizes Gemini v1beta requests.
//   - Adds a default role for each content if missing or invalid.
//     The first message defaults to "user", then alternates user/model when needed.
//
// It keeps the payload otherwise unchanged.
func ConvertGeminiRequestToGemini(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	// Fast path: if no contents field, only attach safety settings
	contents := gjson.GetBytes(rawJSON, "contents")
	if !contents.Exists() {
		return common.AttachDefaultSafetySettings(rawJSON, "safetySettings")
	}

	// The edits below used to rewrite the whole request once per tool,
	// declaration, content and part, which grew with the square of a long
	// session. Each now patches the elements it changes in one pass (see
	// common.RawPatch). The roles are read from the renamed request, where
	// the contents are unchanged.
	rawJSON = renameToolDeclarations(rawJSON)
	out := common.NormalizeContentRoles(rawJSON, "contents")
	out = common.SkipThoughtSignatureValidation(out, "contents")

	if gjson.GetBytes(rawJSON, "generationConfig.responseSchema").Exists() {
		strJson, _ := util.RenameKey(string(out), "generationConfig.responseSchema", "generationConfig.responseJsonSchema")
		out = []byte(strJson)
	}

	out = common.AttachDefaultSafetySettings(out, "safetySettings")
	return out
}

// renameToolDeclarations renames each tool's functionDeclarations to
// function_declarations and then each declaration's parameters to
// parametersJsonSchema, as renameToolDeclarationsSlow does, editing every tool
// on its own and splicing the edited tools back in one pass.
func renameToolDeclarations(rawJSON []byte) []byte {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return rawJSON
	}
	var patches []common.RawPatch
	fast := true
	tools.ForEach(func(_, tool gjson.Result) bool {
		edited, changed, ok := renameToolDeclarationsIn(tool)
		if !ok {
			fast = false
			return false
		}
		if changed {
			patches = append(patches, common.NewRawPatch(tool, edited))
		}
		return true
	})
	if fast {
		if out, ok := common.ApplyRawPatches(rawJSON, patches); ok {
			return out
		}
	}
	return renameToolDeclarationsSlow(rawJSON)
}

// renameToolDeclarationsIn makes one tool's renames on the tool alone. It
// reports whether the tool changed, and false for ok when a rename failed or
// the tool cannot be patched in place.
func renameToolDeclarationsIn(tool gjson.Result) (edited string, changed, ok bool) {
	edited = tool.Raw
	if tool.Get("functionDeclarations").Exists() {
		renamed, err := util.RenameKey(edited, "functionDeclarations", "function_declarations")
		if err != nil || !tool.IsObject() {
			return "", false, false
		}
		edited, changed = renamed, true
	}
	decls := gjson.Get(edited, "function_declarations")
	if !decls.Exists() || !decls.IsArray() {
		return edited, changed, true
	}
	var patches []common.RawPatch
	ok = true
	decls.ForEach(func(_, decl gjson.Result) bool {
		if !decl.Get("parameters").Exists() {
			return true
		}
		renamed, err := util.RenameKey(decl.Raw, "parameters", "parametersJsonSchema")
		if err != nil || !decl.IsObject() {
			ok = false
			return false
		}
		patches = append(patches, common.NewRawPatch(decl, renamed))
		return true
	})
	if !ok {
		return "", false, false
	}
	if len(patches) == 0 {
		return edited, changed, true
	}
	patched, applied := common.ApplyRawPatches([]byte(edited), patches)
	if !applied {
		return "", false, false
	}
	return string(patched), true, true
}

// renameToolDeclarationsSlow is the original per-path rename of the whole
// request, kept for the requests renameToolDeclarations cannot patch.
func renameToolDeclarationsSlow(rawJSON []byte) []byte {
	toolsResult := gjson.GetBytes(rawJSON, "tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		toolResults := toolsResult.Array()
		for i := 0; i < len(toolResults); i++ {
			if gjson.GetBytes(rawJSON, fmt.Sprintf("tools.%d.functionDeclarations", i)).Exists() {
				strJson, _ := util.RenameKey(string(rawJSON), fmt.Sprintf("tools.%d.functionDeclarations", i), fmt.Sprintf("tools.%d.function_declarations", i))
				rawJSON = []byte(strJson)
			}

			functionDeclarationsResult := gjson.GetBytes(rawJSON, fmt.Sprintf("tools.%d.function_declarations", i))
			if functionDeclarationsResult.Exists() && functionDeclarationsResult.IsArray() {
				functionDeclarationsResults := functionDeclarationsResult.Array()
				for j := 0; j < len(functionDeclarationsResults); j++ {
					parametersResult := gjson.GetBytes(rawJSON, fmt.Sprintf("tools.%d.function_declarations.%d.parameters", i, j))
					if parametersResult.Exists() {
						strJson, _ := util.RenameKey(string(rawJSON), fmt.Sprintf("tools.%d.function_declarations.%d.parameters", i, j), fmt.Sprintf("tools.%d.function_declarations.%d.parametersJsonSchema", i, j))
						rawJSON = []byte(strJson)
					}
				}
			}
		}
	}
	return rawJSON
}
