package common

import "github.com/tidwall/gjson"

// Gemini-family translators used to edit a whole request once per content,
// part or declaration with sjson. Every such call re-parses and copies the
// whole request, so a Codex session with hundreds of tool calls and inline
// screenshots cost seconds of CPU per request: on 2026-11-16 those edits took
// two thirds of hk-155's CPU, and every database read in the process timed
// out behind them.
//
// The fast paths built on RawPatch make the same sjson edit on the element
// alone and splice the edited elements back in one pass. sjson rewrites a
// nested path by recursing into the element and copying the bytes around it,
// so the result is byte for byte what the per-path edits produce. When an
// element cannot be placed (gjson reports no offset, or the bytes there
// differ) or the request has an unusual shape (an array that is not an array,
// an element that is not an object), the callers run their original per-path
// code instead, so such requests keep the result they always had.

// RawPatch replaces doc[start:end], which must read orig, with repl.
type RawPatch struct {
	start, end int
	orig, repl string
}

// NewRawPatch replaces v, a value gjson found in the document the patch is
// applied to, with repl.
func NewRawPatch(v gjson.Result, repl string) RawPatch {
	return RawPatch{start: v.Index, end: v.Index + len(v.Raw), orig: v.Raw, repl: repl}
}

// ApplyRawPatches applies patches, which must be in document order and must
// not overlap, in one copy of doc. It reports false and leaves doc alone when a
// patch's range is unknown or no longer holds its original bytes.
func ApplyRawPatches(doc []byte, patches []RawPatch) ([]byte, bool) {
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
