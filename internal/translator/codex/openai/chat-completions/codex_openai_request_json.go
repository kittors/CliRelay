package chat_completions

import (
	"bytes"
	"encoding/json"

	"github.com/tidwall/gjson"
)

// chatRequestFields are the top-level members of a Chat Completions request
// that the translation reads.
type chatRequestFields struct {
	messages        gjson.Result
	tools           gjson.Result
	toolChoice      gjson.Result
	reasoningEffort gjson.Result
	responseFormat  gjson.Result
	verbosity       gjson.Result // text.verbosity
}

// readChatRequestFields finds the members in one walk of the request. A
// gjson.Get for each scans the request up to the member, and all of it for a
// member the request does not have; clients put these members after messages,
// so every lookup skipped over every screenshot in the history again.
//
// It finds what those lookups found. Of members that share a name it keeps the
// first (the gateway refuses such requests, but the translation does not
// depend on that), and text.verbosity is the verbosity of the first text
// object that has one. The request is a single JSON text: the gateway refuses
// anything else before a translator sees it.
func readChatRequestFields(rawJSON string) chatRequestFields {
	var f chatRequestFields
	root := gjson.Parse(rawJSON)
	if !root.IsObject() {
		return f
	}
	root.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "messages":
			keepFirst(&f.messages, value)
		case "tools":
			keepFirst(&f.tools, value)
		case "tool_choice":
			keepFirst(&f.toolChoice, value)
		case "reasoning_effort":
			keepFirst(&f.reasoningEffort, value)
		case "response_format":
			keepFirst(&f.responseFormat, value)
		case "text":
			if !f.verbosity.Exists() && value.IsObject() {
				f.verbosity = value.Get("verbosity")
			}
		}
		return true
	})
	return f
}

// keepFirst sets *found to value unless a member of the same name came first.
// A member that is there exists even when it is null, so found also says
// whether one was seen.
func keepFirst(found *gjson.Result, value gjson.Result) {
	if !found.Exists() {
		*found = value
	}
}

// Content parts are written straight into the input array, byte for byte as
// sjson.Set built them from `{}`: "type" first, then each member in the order
// the translation set it.

// startPart opens a content part of type partType, which needs no escaping, as
// the next element of the content array.
func startPart(buf *bytes.Buffer, first *bool, partType string) {
	appendArrayElement(buf, first, `{"type":"`)
	buf.WriteString(partType)
	buf.WriteByte('"')
}

// writeStringMember adds to the open part a member named name, which needs no
// escaping, whose value is the string value.
func writeStringMember(buf *bytes.Buffer, name, value string) {
	buf.WriteString(`,"`)
	buf.WriteString(name)
	buf.WriteString(`":`)
	writeSJSONString(buf, value)
}

// writeTextPart writes a text part: input_text, or output_text for what the
// assistant said.
func writeTextPart(buf *bytes.Buffer, first *bool, role, text string) {
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	startPart(buf, first, partType)
	writeStringMember(buf, "text", text)
	buf.WriteByte('}')
}

// writeSJSONString writes s as the JSON string sjson.Set writes for it (sjson
// v1.2.5, appendStringify): s between quotes when nothing in it needs
// escaping, otherwise encoding/json's encoding of it, which also escapes <, >
// and & and replaces invalid UTF-8. The upstream gets the bytes it got when
// sjson built the parts.
func writeSJSONString(buf *bytes.Buffer, s string) {
	if sjsonMustMarshal(s) {
		encoded, _ := json.Marshal(s)
		buf.Write(encoded)
		return
	}
	buf.WriteByte('"')
	buf.WriteString(s)
	buf.WriteByte('"')
}

// sjsonMustMarshal is sjson's mustMarshalString: whether s has a byte below
// 0x20, a byte above 0x7f, a quote or a backslash. A screenshot is hundreds of
// kilobytes of base64, which sjson checks a byte at a time; this checks eight
// bytes at a time, then the rest one by one.
func sjsonMustMarshal(s string) bool {
	const (
		ones = 0x0101010101010101
		high = 0x8080808080808080
	)
	i := 0
	for ; len(s)-i >= 8; i += 8 {
		b := s[i : i+8]
		w := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
			uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
		quote := w ^ (ones * '"')
		backslash := w ^ (ones * '\\')
		// A byte's high bit ends up set when the byte is above 0x7f (w
		// itself), below 0x20, or zero after an XOR, that is a quote or a
		// backslash. A borrow out of a byte that matches can set it in the
		// bytes above that one too, but no bit is set in a word where no byte
		// matches.
		if (w|(w-ones*0x20)&^w|(quote-ones)&^quote|(backslash-ones)&^backslash)&high != 0 {
			return true
		}
	}
	for ; i < len(s); i++ {
		if c := s[i]; c < ' ' || c > 0x7f || c == '"' || c == '\\' {
			return true
		}
	}
	return false
}
