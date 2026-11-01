package translator

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// Anthropic's Messages API takes a tool_use id, and the tool_use_id of the
// tool_result that answers it, only when it matches ^[a-zA-Z0-9_-]+$, and
// Claude Code drops a call whose id does not ("could not be parsed"). Other
// upstreams are less strict: Kimi names its calls functions.<name>:<n>, and
// the ids the Gemini translators build start with the function name, which
// may contain dots and colons.
//
// So an id outside that alphabet reaches a Claude client as claudeToolIDPrefix
// followed by its unpadded base64url encoding, and goes back to the upstream as
// the upstream wrote it when the client returns it in a request translated for
// another protocol. A request that stays in Claude format keeps the encoded id:
// a Claude upstream checks the same pattern, and a conversation can move from
// one upstream to another (a model switch, a failover).
//
// The encoding needs no state, so ids handed out before a restart, or by
// another instance, decode the same way. Ids already in the alphabet are left
// alone, except ones that happen to start with the prefix: those are encoded
// too, so that decoding a prefixed id is never ambiguous.
const claudeToolIDPrefix = "toolu_enc_"

var (
	claudeToolIDPrefixBytes = []byte(claudeToolIDPrefix)
	claudeToolUseMarker     = []byte(`"tool_use"`)
	sseDataPrefix           = []byte("data:")
)

func isClaudeToolIDChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// claudeToolIDNeedsEncoding reports whether id has to be encoded before a
// Claude client sees it. An empty id is left alone: there is nothing to keep.
func claudeToolIDNeedsEncoding(id string) bool {
	if id == "" {
		return false
	}
	if strings.HasPrefix(id, claudeToolIDPrefix) {
		return true
	}
	for i := 0; i < len(id); i++ {
		if !isClaudeToolIDChar(id[i]) {
			return true
		}
	}
	return false
}

// encodeClaudeToolID returns id as a Claude client may receive it.
func encodeClaudeToolID(id string) string {
	if !claudeToolIDNeedsEncoding(id) {
		return id
	}
	return claudeToolIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// decodeClaudeToolID inverts encodeClaudeToolID. It only accepts what
// encodeClaudeToolID produces, checked by encoding the result again: anything
// else that carries the prefix (a truncated, forged or hand-written id, base64
// in another form) is reported as not encoded and stays as it is.
func decodeClaudeToolID(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, claudeToolIDPrefix)
	if !ok || rest == "" {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return "", false
	}
	decoded := string(raw)
	if !utf8.ValidString(decoded) || encodeClaudeToolID(decoded) != id {
		return "", false
	}
	return decoded, true
}

// EncodeClaudeResponseToolIDs encodes the tool_use ids in a non-streaming
// Claude Messages response for a Claude client. Server tool blocks are left
// alone: their ids come from Anthropic and follow a stricter pattern
// (srvtoolu_ and no hyphens) that the encoding does not.
func EncodeClaudeResponseToolIDs(body []byte) []byte {
	if !bytes.Contains(body, claudeToolUseMarker) || !gjson.ValidBytes(body) {
		return body
	}
	var edits []stringEdit
	gjson.GetBytes(body, "content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_use" {
			edits = appendToolIDEdit(edits, body, block.Get("id"), 0)
		}
		return true
	})
	return applyStringEdits(body, edits)
}

// EncodeClaudeStreamToolIDs encodes the tool_use ids in a chunk of a Claude
// Messages stream for a Claude client. The id is only in content_block_start.
//
// A chunk is one or more whole SSE lines: the executors forward a Claude
// upstream's stream line by line, each with its newline, and the translators
// return whole events. Only lines that end in the chunk are rewritten; a line
// split across chunks would go out unchanged rather than be cut into.
func EncodeClaudeStreamToolIDs(chunk []byte) []byte {
	if !bytes.Contains(chunk, claudeToolUseMarker) {
		return chunk
	}
	var edits []stringEdit
	for start := 0; start < len(chunk); {
		newline := bytes.IndexByte(chunk[start:], '\n')
		if newline < 0 {
			break
		}
		end := start + newline
		if payload, ok := bytes.CutPrefix(chunk[start:end], sseDataPrefix); ok && gjson.ValidBytes(payload) {
			if gjson.GetBytes(payload, "type").String() == "content_block_start" {
				block := gjson.GetBytes(payload, "content_block")
				if block.Get("type").String() == "tool_use" {
					edits = appendToolIDEdit(edits, payload, block.Get("id"), end-len(payload))
				}
			}
		}
		start = end + 1
	}
	return applyStringEdits(chunk, edits)
}

// decodeClaudeRequestToolIDs turns the encoded ids a Claude client sends back,
// in tool_use blocks and in the tool_result blocks that answer them, into the
// upstream's own ids. Ids in text and anywhere else are not touched.
func decodeClaudeRequestToolIDs(body []byte) []byte {
	if !bytes.Contains(body, claudeToolIDPrefixBytes) {
		return body
	}
	var edits []stringEdit
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			var value gjson.Result
			switch block.Get("type").String() {
			case "tool_use":
				value = block.Get("id")
			case "tool_result":
				value = block.Get("tool_use_id")
			default:
				return true
			}
			if decoded, ok := decodeClaudeToolID(value.Str); ok && value.Type == gjson.String {
				edits = appendStringEdit(edits, body, value, decoded, 0)
			}
			return true
		})
		return true
	})
	return applyStringEdits(body, edits)
}

// EncodeClaudeRequestToolIDs makes the tool ids of a Claude Messages request,
// about to go to a Claude upstream, ones the upstream accepts. A conversation
// held on another upstream and continued on Claude (a client switching models,
// a virtual model whose members come from different vendors) reaches the
// Claude executor with the ids the other upstream wrote, functions.<name>:<n>
// for Kimi, and Anthropic answers a 400 for them. Those ids are encoded as for
// a Claude client: deterministically, so a call and its result keep matching
// and a replayed history keeps its prompt cache prefix. Nothing has to be
// decoded afterwards: the upstream's reply carries ids of its own. Ids already
// in the alphabet are sent as they are, the encoded ones a Claude client
// returns included.
func EncodeClaudeRequestToolIDs(body []byte) []byte {
	if !bytes.Contains(body, claudeToolUseMarker) {
		return body
	}
	var edits []stringEdit
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			var value gjson.Result
			switch block.Get("type").String() {
			case "tool_use":
				value = block.Get("id")
			case "tool_result":
				value = block.Get("tool_use_id")
			default:
				return true
			}
			if value.Type == gjson.String && !isClaudeToolID(value.Str) {
				edits = appendStringEdit(edits, body, value, encodeClaudeToolID(value.Str), 0)
			}
			return true
		})
		return true
	})
	return applyStringEdits(body, edits)
}

// isClaudeToolID reports whether a Claude upstream accepts id as it is.
func isClaudeToolID(id string) bool {
	for i := 0; i < len(id); i++ {
		if !isClaudeToolIDChar(id[i]) {
			return false
		}
	}
	return id != ""
}

// stringEdit replaces the JSON string at src[start:end] with value.
type stringEdit struct {
	start, end int
	value      string
}

func appendToolIDEdit(edits []stringEdit, src []byte, id gjson.Result, offset int) []stringEdit {
	if id.Type != gjson.String || !claudeToolIDNeedsEncoding(id.Str) {
		return edits
	}
	return appendStringEdit(edits, src, id, encodeClaudeToolID(id.Str), offset)
}

// appendStringEdit records the replacement of value, found in src, which
// itself starts at offset in the buffer the edits apply to. gjson reports
// where a value sits in its input; the check keeps a value it could not place
// from being spliced in at the wrong spot.
func appendStringEdit(edits []stringEdit, src []byte, value gjson.Result, replacement string, offset int) []stringEdit {
	start, end := value.Index, value.Index+len(value.Raw)
	if start <= 0 || end > len(src) || string(src[start:end]) != value.Raw {
		return edits
	}
	return append(edits, stringEdit{start: offset + start, end: offset + end, value: replacement})
}

// applyStringEdits returns src with the edits, which are in order and do not
// overlap, applied. src itself is never modified.
func applyStringEdits(src []byte, edits []stringEdit) []byte {
	if len(edits) == 0 {
		return src
	}
	out := make([]byte, 0, len(src)+32*len(edits))
	last := 0
	for _, edit := range edits {
		out = append(out, src[last:edit.start]...)
		out = appendJSONString(out, edit.value)
		last = edit.end
	}
	return append(out, src[last:]...)
}

func appendJSONString(dst []byte, s string) []byte {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// Keep <, > and & as they are rather than as \u escapes: the upstream
	// gets back the id exactly as it wrote it.
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(s)
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}
