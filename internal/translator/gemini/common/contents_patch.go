package common

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// SkipThoughtSignatureSentinel asks the upstream to skip thought-signature
// validation.
const SkipThoughtSignatureSentinel = "skip_thought_signature_validator"

// The edits below are shared by the Gemini-family translators, which differ
// only in where the Gemini body sits: "contents" and "tools" for Gemini,
// "request.contents" and "request.tools" for Gemini CLI and Antigravity. Each
// patches the elements it changes and splices them back in one pass (see
// RawPatch); the *Slow variants are the original per-path edits, kept for the
// requests the fast path cannot place.

// NormalizeContentRoles gives every entry of the contents array at path a
// valid role: a missing or unknown one becomes "user", or "model" right after
// a "user" entry.
func NormalizeContentRoles(rawJSON []byte, path string) []byte {
	contents := gjson.GetBytes(rawJSON, path)
	if !contents.Exists() {
		return rawJSON
	}
	if !contents.IsArray() {
		return normalizeContentRolesSlow(rawJSON, path)
	}
	var patches []RawPatch
	prevRole := ""
	fast := true
	contents.ForEach(func(_, content gjson.Result) bool {
		role := content.Get("role").String()
		if role != "user" && role != "model" {
			newRole := "user"
			if prevRole == "user" {
				newRole = "model"
			}
			patched, err := sjson.Set(content.Raw, "role", newRole)
			if err != nil || !content.IsObject() {
				fast = false
				return false
			}
			patches = append(patches, NewRawPatch(content, patched))
			role = newRole
		}
		prevRole = role
		return true
	})
	if fast {
		if out, ok := ApplyRawPatches(rawJSON, patches); ok {
			return out
		}
	}
	return normalizeContentRolesSlow(rawJSON, path)
}

func normalizeContentRolesSlow(rawJSON []byte, path string) []byte {
	contents := gjson.GetBytes(rawJSON, path)
	if contents.Exists() {
		prevRole := ""
		idx := 0
		contents.ForEach(func(_ gjson.Result, value gjson.Result) bool {
			role := value.Get("role").String()
			valid := role == "user" || role == "model"
			if role == "" || !valid {
				var newRole string
				if prevRole == "" {
					newRole = "user"
				} else if prevRole == "user" {
					newRole = "model"
				} else {
					newRole = "user"
				}
				rawJSON, _ = sjson.SetBytes(rawJSON, fmt.Sprintf("%s.%d.role", path, idx), newRole)
				role = newRole
			}
			prevRole = role
			idx++
			return true
		})
	}
	return rawJSON
}

// RenameFunctionDeclarationParameters renames "parameters" to
// "parametersJsonSchema" in every function_declarations entry of the tools
// array at toolsPath.
func RenameFunctionDeclarationParameters(rawJSON []byte, toolsPath string) []byte {
	tools := gjson.GetBytes(rawJSON, toolsPath)
	if !tools.Exists() || !tools.IsArray() {
		return rawJSON
	}
	var patches []RawPatch
	fast := true
	tools.ForEach(func(_, tool gjson.Result) bool {
		decls := tool.Get("function_declarations")
		if !decls.Exists() || !decls.IsArray() {
			return true
		}
		decls.ForEach(func(_, decl gjson.Result) bool {
			if !decl.Get("parameters").Exists() {
				return true
			}
			renamed, err := util.RenameKey(decl.Raw, "parameters", "parametersJsonSchema")
			if err != nil || !decl.IsObject() {
				fast = false
				return false
			}
			patches = append(patches, NewRawPatch(decl, renamed))
			return true
		})
		return fast
	})
	if fast {
		if out, ok := ApplyRawPatches(rawJSON, patches); ok {
			return out
		}
	}
	return renameFunctionDeclarationParametersSlow(rawJSON, toolsPath)
}

func renameFunctionDeclarationParametersSlow(rawJSON []byte, toolsPath string) []byte {
	toolsResult := gjson.GetBytes(rawJSON, toolsPath)
	if toolsResult.Exists() && toolsResult.IsArray() {
		toolResults := toolsResult.Array()
		for i := 0; i < len(toolResults); i++ {
			functionDeclarationsResult := gjson.GetBytes(rawJSON, fmt.Sprintf("%s.%d.function_declarations", toolsPath, i))
			if functionDeclarationsResult.Exists() && functionDeclarationsResult.IsArray() {
				functionDeclarationsResults := functionDeclarationsResult.Array()
				for j := 0; j < len(functionDeclarationsResults); j++ {
					parametersResult := gjson.GetBytes(rawJSON, fmt.Sprintf("%s.%d.function_declarations.%d.parameters", toolsPath, i, j))
					if parametersResult.Exists() {
						strJson, _ := util.RenameKey(string(rawJSON), fmt.Sprintf("%s.%d.function_declarations.%d.parameters", toolsPath, i, j), fmt.Sprintf("%s.%d.function_declarations.%d.parametersJsonSchema", toolsPath, i, j))
						rawJSON = []byte(strJson)
					}
				}
			}
		}
	}
	return rawJSON
}

// SkipThoughtSignatureValidation sets SkipThoughtSignatureSentinel as the
// thoughtSignature of every part of a model content that has a functionCall
// or already carries a thoughtSignature, in the contents array at path. This
// is the Gemini and Gemini CLI passthrough rule; Antigravity has its own.
func SkipThoughtSignatureValidation(rawJSON []byte, path string) []byte {
	contents := gjson.GetBytes(rawJSON, path)
	if !contents.Exists() {
		return rawJSON
	}
	if !contents.IsArray() {
		return skipThoughtSignatureValidationSlow(rawJSON, path)
	}
	var patches []RawPatch
	fast := true
	contents.ForEach(func(_, content gjson.Result) bool {
		if content.Get("role").String() != "model" {
			return true
		}
		parts := content.Get("parts")
		if !parts.Exists() {
			return true
		}
		if !parts.IsArray() {
			fast = false
			return false
		}
		parts.ForEach(func(_, part gjson.Result) bool {
			if !part.Get("functionCall").Exists() && !part.Get("thoughtSignature").Exists() {
				return true
			}
			patched, err := sjson.Set(part.Raw, "thoughtSignature", SkipThoughtSignatureSentinel)
			if err != nil || !part.IsObject() {
				fast = false
				return false
			}
			patches = append(patches, NewRawPatch(part, patched))
			return true
		})
		return fast
	})
	if fast {
		if out, ok := ApplyRawPatches(rawJSON, patches); ok {
			return out
		}
	}
	return skipThoughtSignatureValidationSlow(rawJSON, path)
}

func skipThoughtSignatureValidationSlow(rawJSON []byte, path string) []byte {
	gjson.GetBytes(rawJSON, path).ForEach(func(key, content gjson.Result) bool {
		if content.Get("role").String() == "model" {
			content.Get("parts").ForEach(func(partKey, part gjson.Result) bool {
				if part.Get("functionCall").Exists() {
					rawJSON, _ = sjson.SetBytes(rawJSON, fmt.Sprintf("%s.%d.parts.%d.thoughtSignature", path, key.Int(), partKey.Int()), SkipThoughtSignatureSentinel)
				} else if part.Get("thoughtSignature").Exists() {
					rawJSON, _ = sjson.SetBytes(rawJSON, fmt.Sprintf("%s.%d.parts.%d.thoughtSignature", path, key.Int(), partKey.Int()), SkipThoughtSignatureSentinel)
				}
				return true
			})
		}
		return true
	})
	return rawJSON
}
