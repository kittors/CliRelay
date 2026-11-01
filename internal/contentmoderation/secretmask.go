package contentmoderation

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// secretTable is what the masking of one request replaced: every placeholder
// with the value it stands for. It is built for each request and restores only
// what that request sent, so a reply cannot be made to reveal the secrets of
// another request, let alone of another tenant, by naming their placeholders.
type secretTable struct {
	// key is the tenant's (see SecretMaskingSettings.Key).
	key    []byte
	values map[string]string
	// replacer swaps every placeholder for its value; made on first use.
	replacer *strings.Replacer
}

func newSecretTable(key []byte) *secretTable {
	return &secretTable{key: key, values: make(map[string]string)}
}

// secretTagEncoding writes a placeholder's tag in lower case base32.
var secretTagEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// placeholder returns {{KIND_tag}} for value and records it. The tag is the
// start of HMAC-SHA256(tenant key, kind and value): the same value always gets
// the same placeholder in a tenant, so a conversation the client sends again
// turn after turn reaches the upstream byte for byte the same and its prompt
// cache keeps working, while another tenant, with another key, gets another
// placeholder for it. The upstream cannot try candidate values against a tag
// without the key.
func (t *secretTable) placeholder(kind secretKind, value string) string {
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	sum := mac.Sum(nil)
	p := "{{" + string(kind) + "_" + secretTagEncoding.EncodeToString(sum[:5]) + "}}"
	if known, taken := t.values[p]; taken && known != value {
		// Two values of one request on the same 40-bit tag: the longer tag tells
		// them apart, and is as stable.
		p = "{{" + string(kind) + "_" + secretTagEncoding.EncodeToString(sum[:10]) + "}}"
	}
	if _, known := t.values[p]; !known {
		t.values[p] = value
		t.replacer = nil
	}
	return p
}

// restore returns text with every placeholder of the table replaced by its
// value.
func (t *secretTable) restore(text string) string {
	if len(t.values) == 0 || !strings.Contains(text, "{{") {
		return text
	}
	if t.replacer == nil {
		t.seal()
	}
	return t.replacer.Replace(text)
}

// seal makes the replacer restore uses. The table is read-only after it: the
// reply filters of every execution of one request share the table (the
// request guard's outcome is handed from one execution to the next), and must
// not build it at the same time.
func (t *secretTable) seal() {
	pairs := make([]string, 0, 2*len(t.values))
	for p, value := range t.values {
		pairs = append(pairs, p, value)
	}
	t.replacer = strings.NewReplacer(pairs...)
}

// partialTail returns where the end of text could be the start of one of the
// table's placeholders, or len(text) if it cannot. A stream sends a
// placeholder in as many pieces as the model wrote it in; the start of one is
// held back until the rest arrives.
func (t *secretTable) partialTail(text string) int {
	longest := 0
	for p := range t.values {
		longest = max(longest, len(p))
	}
	for i := max(0, len(text)-longest+1); i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		tail := text[i:]
		for p := range t.values {
			if len(tail) < len(p) && strings.HasPrefix(p, tail) {
				return i
			}
		}
	}
	return len(text)
}

// maskRequestSecrets replaces the secrets in the text of body, a request in
// the layout of format, with placeholders it records in table. It returns the
// new body and how many secrets it replaced, nil and 0 when there were none.
// failure says why a request that holds a secret could not be rewritten; such
// a request must not be sent.
//
// It shares the word guard conversation walk, adds tool descriptions, and treats
// arbitrary tool arguments as data rather than protocol metadata. The actual
// protocol is left alone. Text a signature covers (a replayed thinking
// block) is never changed: the upstream would refuse the request.
func maskRequestSecrets(format sdktranslator.Format, body []byte, table *secretTable) (out []byte, masked int, failure string) {
	type slot struct {
		doc  *wordDoc
		lit0 int
	}
	done := make(map[slot]bool)
	var patches docPatches
	for _, unit := range extractSecretUnits(format, body) {
		if unit.signed {
			continue
		}
		spans := findSecrets(unit.text)
		if len(spans) == 0 {
			continue
		}
		where := withoutSecrets(unitField(unit))
		if unit.kind != unitText || !unit.rewritable {
			why := unit.why
			if why == "" {
				why = "it is not a plain string value"
			}
			return nil, 0, where + ": " + why
		}
		key := slot{unit.doc, unit.lit0}
		if done[key] {
			continue // the same string reached twice
		}
		done[key] = true
		lit := unit.doc.data[unit.lit0:unit.lit1]
		if text, ok := decodeJSONLiteral(lit); !ok || text != unit.text {
			return nil, 0, where + ": " + whyUnlocated
		}
		edits := make([]literalEdit, len(spans))
		for i, span := range spans {
			edits[i] = literalEdit{start: span.start, end: span.end, text: table.placeholder(span.kind, unit.text[span.start:span.end])}
		}
		literal, ok := spliceLiteral(lit, edits)
		if !ok {
			return nil, 0, where + ": " + whyEscapes
		}
		patches.add(unit.doc, patch{lit0: unit.lit0, lit1: unit.lit1, literal: literal})
		masked += len(spans)
	}
	if masked == 0 {
		return nil, 0, ""
	}
	written, rewriteFailed := patches.write()
	if rewriteFailed != nil {
		return nil, 0, withoutSecrets(rewriteFailed.field) + ": " + rewriteFailed.why
	}
	return written, masked, ""
}

// withoutSecrets returns a field's path with the secrets in it blanked out. A
// path names object keys, and a key may itself be a secret; the path goes to
// the client and to the process log.
func withoutSecrets(path string) string {
	spans := findSecrets(path)
	for i := len(spans) - 1; i >= 0; i-- {
		path = path[:spans[i].start] + "<" + string(spans[i].kind) + ">" + path[spans[i].end:]
	}
	return path
}
