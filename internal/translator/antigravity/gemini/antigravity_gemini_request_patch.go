package gemini

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// skipThoughtSignatureSentinel asks the upstream to skip thought-signature
// validation for Gemini-family models.
const skipThoughtSignatureSentinel = common.SkipThoughtSignatureSentinel

// The request edits used to run sjson on the whole Antigravity request once
// per content, part or declaration, which cost seconds of CPU per Codex
// session on 2026-11-16. The role and parameter edits are now the shared
// patches in gemini/common; the thought-signature edit below follows
// Antigravity's own rule and patches the same way (see common.RawPatch).

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
	var patches []common.RawPatch
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
			patches = append(patches, common.NewRawPatch(part, patched))
			return true
		})
		return fast
	})
	if fast {
		if out, ok := common.ApplyRawPatches(rawJSON, patches); ok {
			return out
		}
	}
	return markThoughtSignaturesToSkipSlow(rawJSON)
}
