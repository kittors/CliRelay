package gemini

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// skipThoughtSignatureSentinel asks the upstream to skip thought-signature
// validation for Gemini-family models.
const skipThoughtSignatureSentinel = "skip_thought_signature_validator"

// The three request edits in this file used to run sjson on the whole
// Antigravity request once per content, part or declaration. Every such call
// re-parses and copies the whole request, so a Codex session with hundreds of
// tool calls and inline screenshots cost seconds of CPU per request: on
// 2026-11-16 these edits, fixCLIToolResponse and the Responses -> Gemini
// translator took two thirds of hk-155's CPU, and every database read in the
// process timed out behind them.
//
// Each fast path makes the same sjson edit on the element alone and splices
// the edited elements back in one pass. sjson rewrites a nested path by
// recursing into the element and copying the bytes around it, so the result is
// byte for byte what the per-path edits produce. When an element cannot be
// placed (gjson reports no offset, or the bytes there differ) or the request
// has an unusual shape (contents or parts that are not arrays, an element that
// is not an object), the original per-path code runs instead, so such requests
// keep the result they always had.

// rawPatch replaces doc[start:end], which must read orig, with repl.
type rawPatch struct {
	start, end int
	orig, repl string
}

func newRawPatch(v gjson.Result, repl string) rawPatch {
	return rawPatch{start: v.Index, end: v.Index + len(v.Raw), orig: v.Raw, repl: repl}
}

// applyRawPatches applies patches, which must be in document order and must
// not overlap, in one copy of doc. It reports false and leaves doc alone when a
// patch's range is unknown or no longer holds its original bytes.
func applyRawPatches(doc []byte, patches []rawPatch) ([]byte, bool) {
	if len(patches) == 0 {
		return doc, true
	}
	size, prev := len(doc), 0
	for _, p := range patches {
		// gjson reports Index 0 when it cannot place a value, and no element of
		// the request object starts at offset 0.
		if p.start <= 0 || p.start < prev || p.end > len(doc) || p.end-p.start != len(p.orig) || string(doc[p.start:p.end]) != p.orig {
			return doc, false
		}
		size += len(p.repl) - len(p.orig)
		prev = p.end
	}
	out := make([]byte, 0, size)
	prev = 0
	for _, p := range patches {
		out = append(out, doc[prev:p.start]...)
		out = append(out, p.repl...)
		prev = p.end
	}
	return append(out, doc[prev:]...), true
}

// normalizeContentRoles gives every entry of request.contents a valid role: a
// missing or unknown one becomes "user", or "model" right after a "user"
// entry, exactly as normalizeContentRolesSlow does.
func normalizeContentRoles(rawJSON []byte) []byte {
	contents := gjson.GetBytes(rawJSON, "request.contents")
	if !contents.Exists() {
		return rawJSON
	}
	if !contents.IsArray() {
		return normalizeContentRolesSlow(rawJSON)
	}
	var patches []rawPatch
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
			patches = append(patches, newRawPatch(content, patched))
			role = newRole
		}
		prevRole = role
		return true
	})
	if fast {
		if out, ok := applyRawPatches(rawJSON, patches); ok {
			return out
		}
	}
	return normalizeContentRolesSlow(rawJSON)
}

// renameFunctionDeclarationParameters renames "parameters" to
// "parametersJsonSchema" in every request.tools[].function_declarations[]
// entry, exactly as renameFunctionDeclarationParametersSlow does.
func renameFunctionDeclarationParameters(rawJSON []byte) []byte {
	tools := gjson.GetBytes(rawJSON, "request.tools")
	if !tools.Exists() || !tools.IsArray() {
		return rawJSON
	}
	var patches []rawPatch
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
			patches = append(patches, newRawPatch(decl, renamed))
			return true
		})
		return fast
	})
	if fast {
		if out, ok := applyRawPatches(rawJSON, patches); ok {
			return out
		}
	}
	return renameFunctionDeclarationParametersSlow(rawJSON)
}

// markThoughtSignaturesToSkip sets skipThoughtSignatureSentinel as the
// thoughtSignature of every thinking part, and of every functionCall part whose
// signature is missing or shorter than 50 bytes, in model contents, exactly as
// markThoughtSignaturesToSkipSlow does. A part that is both gets the sentinel
// once; the slow path writes it twice with the same bytes.
func markThoughtSignaturesToSkip(rawJSON []byte) []byte {
	contents := gjson.GetBytes(rawJSON, "request.contents")
	if !contents.Exists() {
		return rawJSON
	}
	if !contents.IsArray() {
		return markThoughtSignaturesToSkipSlow(rawJSON)
	}
	var patches []rawPatch
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
			mark := part.Get("thought").Bool()
			if !mark && part.Get("functionCall").Exists() {
				sig := part.Get("thoughtSignature").String()
				mark = sig == "" || len(sig) < 50
			}
			if !mark {
				return true
			}
			patched, err := sjson.Set(part.Raw, "thoughtSignature", skipThoughtSignatureSentinel)
			if err != nil || !part.IsObject() {
				fast = false
				return false
			}
			patches = append(patches, newRawPatch(part, patched))
			return true
		})
		return fast
	})
	if fast {
		if out, ok := applyRawPatches(rawJSON, patches); ok {
			return out
		}
	}
	return markThoughtSignaturesToSkipSlow(rawJSON)
}
