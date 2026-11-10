package contentmoderation

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// Outbound secret masking: a tenant may switch it on to keep the credentials
// that travel in its requests away from the upstream (a key pasted into a
// prompt, a .env file a coding agent read, a token in a tool result). Every
// value found in the text the model reads is replaced with a placeholder
// before the request leaves, and the placeholder is turned back into the
// value in the reply, so the client sees its own text again
// (secretmask_reply.go). It protects against the upstream and any relay in
// front of it, not against this server.
//
// What is found, and how a placeholder is made and put back, is
// internal/privacymask's (ported from magpie). This file reads the request the
// way the word guard does and writes the placeholders into it.

// maskRequestSecrets replaces what masker finds in the text of body, a request
// in the layout of format, with placeholders. It returns the new body, nil when
// nothing was replaced. failure says why a request that holds a value to mask
// could not be rewritten; such a request must not be sent.
//
// It reads the request as the word guard does, and also the data of tool calls
// and results whatever their keys, and the descriptions of the tools and of
// the reply's schema (extractSecretUnits), so it sees the text the model
// reads, and leaves the protocol alone. Text a signature covers (a replayed
// thinking block) is never changed: the upstream would refuse the request.
// quiet says the body is a second copy of a request whose first copy was
// masked already: its values are not counted again.
func maskRequestSecrets(format sdktranslator.Format, body []byte, masker *privacymask.Masker, quiet bool) (out []byte, failure string) {
	return maskRequest(format, body, masker, quiet, nil)
}

// maskFailureUnreadable is the failure of a body that is not one strict JSON
// text. It names no field: the problem may sit in a member name, and a name may
// be a secret.
const maskFailureUnreadable = "unreadable"

// maskRequest is maskRequestSecrets that also notes, for the request's record,
// where each value was found and the text around it (notes, nil for none).
func maskRequest(format sdktranslator.Format, body []byte, masker *privacymask.Masker, quiet bool, notes *maskNotes) (out []byte, failure string) {
	units, unreadable := readSecretUnits(format, body)
	if unreadable != nil {
		// Masking cannot see the values of a body it cannot read as the upstream
		// will, so it cannot keep them here (see extraction.run).
		return nil, maskFailureUnreadable
	}
	type slot struct {
		doc  *wordDoc
		lit0 int
	}
	texts := make([]string, len(units))
	skip := make([]bool, len(units))
	seen := make(map[slot]bool, len(units))
	for i, unit := range units {
		texts[i] = unit.text
		key := slot{unit.doc, unit.lit0}
		// the same string reached twice is read once, so its values are counted
		// once
		skip[i] = unit.signed || unit.rewritable && seen[key]
		if unit.rewritable {
			seen[key] = true
		}
	}
	var plans [][]privacymask.Edit
	if quiet {
		plans = masker.PlanQuiet(texts, skip)
	} else {
		plans = masker.Plan(texts, skip)
	}
	var patches docPatches
	changed := false
	for i, unit := range units {
		edits := plans[i]
		if notes != nil && !quiet {
			notes.add(masker, i, unit, edits)
		}
		if len(edits) == 0 {
			continue
		}
		where := withoutSecrets(unitField(unit))
		if unit.kind != unitText || !unit.rewritable {
			why := unit.why
			if why == "" {
				why = "it is not a plain string value"
			}
			return nil, where + ": " + why
		}
		lit := unit.doc.data[unit.lit0:unit.lit1]
		if text, ok := decodeJSONLiteral(lit); !ok || text != unit.text {
			return nil, where + ": " + whyUnlocated
		}
		literalEdits := make([]literalEdit, len(edits))
		for j, e := range edits {
			literalEdits[j] = literalEdit{start: e.Start, end: e.End, text: e.Text}
		}
		literal, ok := spliceLiteral(lit, literalEdits)
		if !ok {
			// An edge that falls inside an escape: the whole string is written
			// again. The upstream reads the same text; only how it is escaped
			// changes, the same way every time.
			literal = append(appendJSONEscaped([]byte{'"'}, privacymask.Apply(unit.text, edits)), '"')
		}
		patches.add(unit.doc, patch{lit0: unit.lit0, lit1: unit.lit1, literal: literal})
		changed = true
	}
	if !changed {
		return nil, ""
	}
	written, rewriteFailed := patches.write()
	if rewriteFailed != nil {
		return nil, withoutSecrets(rewriteFailed.field) + ": " + rewriteFailed.why
	}
	return written, ""
}

// withoutSecrets returns a field's path with the secrets in it blanked out. A
// path names object keys, and a key may itself be a secret; the path goes to
// the client and to the process log.
func withoutSecrets(path string) string {
	m := privacymask.NewMasker(nil, privacymask.Options{Secrets: true}, nil, "")
	edits := m.Plan([]string{path}, nil)[0]
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		path = path[:e.Start] + "<" + kindOfPlaceholder(e.Text) + ">" + path[e.End:]
	}
	return path
}

// kindOfPlaceholder is the kind a placeholder names: SECRET of
// {{SECRET_abcdefgh}}.
func kindOfPlaceholder(p string) string {
	for i := len(p) - 1; i > 2; i-- {
		if p[i] == '_' {
			return p[2:i]
		}
	}
	return ""
}
