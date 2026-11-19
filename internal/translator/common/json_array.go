package common

// JSONArrayBuilder joins raw JSON values into an array literal byte for byte
// the way repeated sjson.SetRaw(doc, "arr.-1", raw) appends build one: "[a,b]",
// or "[]" when nothing was added.
//
// Translators collect elements here and write the array into the request once.
// Appending each element to the request itself makes sjson re-parse and copy
// the whole request per element, which is quadratic for a Codex session with
// hundreds of tool calls and inline screenshots: on 2026-11-16 the
// Responses -> Gemini -> Antigravity translators took two thirds of hk-155's
// CPU that way and starved every other request in the process.
//
// The result only matches the appends when the array starts out empty and the
// translator never reads it back while it is being built; each caller says why
// that holds for it.
type JSONArrayBuilder struct {
	buf []byte
	n   int
}

// Append adds one raw JSON value, written verbatim as sjson.SetRaw writes it.
func (b *JSONArrayBuilder) Append(raw string) {
	if b.n > 0 {
		b.buf = append(b.buf, ',')
	}
	b.buf = append(b.buf, raw...)
	b.n++
}

// Len reports how many values were appended.
func (b *JSONArrayBuilder) Len() int { return b.n }

// String returns the array literal.
func (b *JSONArrayBuilder) String() string {
	out := make([]byte, 0, len(b.buf)+2)
	out = append(out, '[')
	out = append(out, b.buf...)
	out = append(out, ']')
	return string(out)
}
