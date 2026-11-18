package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Incremental Codex requests: the matcher.
//
// A turn may go upstream as an increment (previous_response_id plus the new
// input) only when sending it that way gives the model the context the full
// request would have. On one websocket the backend keeps the last response it
// produced, so the context of an increment is
//
//	previous input + previous output + new input
//
// while the full request is the client's own history. The two agree when the
// client's history starts with the previous input byte for byte, continues with
// what the client was given of the previous output, and then adds something.
// This follows codex-rs get_incremental_items (rust-v0.170.0), which requires
// the current input to start with the previous input plus the items the last
// response added, and the other request properties to be unchanged.
//
// What a client hands back of the previous output depends on its protocol. The
// official client returns the output items themselves; a Chat Completions
// client returns one assistant message, whose text is every output_text the
// relay streamed, joined, and whose tool_calls are the function calls; it never
// sees reasoning or hosted tool calls. So the echoed items are compared with the
// previous output through a projection both sides share: the assistant text,
// joined, and the client-executed tool calls in order. Reasoning and hosted
// tool calls are left out on both sides, which is also what makes the increment
// keep the reasoning a Chat client could not send back.

// codexIncrementalDigest identifies bytes the matcher compares. 128 bits of
// SHA-256 are far beyond what adjacent turns of one conversation can collide on.
type codexIncrementalDigest [16]byte

func codexIncrementalDigestOf(raw string) codexIncrementalDigest {
	sum := sha256.Sum256([]byte(raw))
	var digest codexIncrementalDigest
	copy(digest[:], sum[:16])
	return digest
}

func codexIncrementalDigestSum(h hash.Hash) codexIncrementalDigest {
	var digest codexIncrementalDigest
	copy(digest[:], h.Sum(nil))
	return digest
}

// codexIncrementalIgnoredFields are left out of the request properties two turns
// must share. codex-rs skips input, stream_options, client_metadata and
// access_programs; client_metadata carries a turn id that changes every turn.
// The rest are set or removed by the transport itself.
var codexIncrementalIgnoredFields = map[string]struct{}{
	"input":                {},
	"client_metadata":      {},
	"stream_options":       {},
	"access_programs":      {},
	"previous_response_id": {},
	"stream":               {},
	"type":                 {},
	"generate":             {},
}

// codexIncrementalRequest is one outbound Codex body as the matcher sees it.
type codexIncrementalRequest struct {
	body []byte
	// inputStart and inputEnd delimit the input array inside body.
	inputStart, inputEnd int
	items                []codexIncrementalInputItem
	props                codexIncrementalDigest
	promptCacheKey       string
	hasType              bool
}

type codexIncrementalInputItem struct {
	raw    string
	digest codexIncrementalDigest
}

// parseCodexIncrementalRequest reads the input items and request properties of
// an outbound body. The input is read once: on a multi-megabyte history every
// extra pass costs a full scan. A non-empty reason means the body cannot take
// part (no input array, malformed JSON).
func parseCodexIncrementalRequest(body []byte) (*codexIncrementalRequest, string) {
	if len(body) == 0 || body[0] != '{' {
		return nil, "body_not_object"
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil, "input_not_array"
	}
	start := input.Index
	end := start + len(input.Raw)
	if start <= 0 || end > len(body) || body[start] != '[' || body[end-1] != ']' {
		return nil, "input_offset_unknown"
	}
	req := &codexIncrementalRequest{body: body, inputStart: start, inputEnd: end}

	// The properties are read from the body with the input replaced by null, so
	// the history is not copied or scanned again.
	rest := make([]byte, 0, len(body)-len(input.Raw)+4)
	rest = append(rest, body[:start]...)
	rest = append(rest, "null"...)
	rest = append(rest, body[end:]...)
	root := gjson.ParseBytes(rest)
	if !root.IsObject() {
		return nil, "body_not_object"
	}
	type field struct{ key, raw string }
	fields := make([]field, 0, 16)
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if name == "type" {
			req.hasType = true
		}
		if name == "prompt_cache_key" {
			req.promptCacheKey = strings.TrimSpace(value.String())
		}
		if _, ignored := codexIncrementalIgnoredFields[name]; ignored {
			return true
		}
		fields = append(fields, field{key: name, raw: value.Raw})
		return true
	})
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].key < fields[j].key })
	h := sha256.New()
	for _, f := range fields {
		writeCodexIncrementalHashString(h, f.key)
		writeCodexIncrementalHashString(h, f.raw)
	}
	req.props = codexIncrementalDigestSum(h)

	input.ForEach(func(_, item gjson.Result) bool {
		req.items = append(req.items, codexIncrementalInputItem{raw: item.Raw, digest: codexIncrementalDigestOf(item.Raw)})
		return true
	})
	return req, ""
}

// writeCodexIncrementalHashString writes s length-prefixed, so two fields can
// never run together into the same bytes.
func writeCodexIncrementalHashString(h hash.Hash, s string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(s)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(s))
}

// dropContent lets the body and the item bytes go once nothing will be built
// from them; the digests and properties stay for the next turn's comparison.
func (r *codexIncrementalRequest) dropContent() {
	if r == nil {
		return
	}
	r.body = nil
	for i := range r.items {
		r.items[i].raw = ""
	}
}

func (r *codexIncrementalRequest) itemDigests() []codexIncrementalDigest {
	out := make([]codexIncrementalDigest, len(r.items))
	for i := range r.items {
		out[i] = r.items[i].digest
	}
	return out
}

// codexIncrementalProjection is what a client can hand back of one response:
// the assistant text, joined, and the client-executed tool calls in order.
type codexIncrementalProjection struct {
	text  codexIncrementalDigest
	calls []string
}

func (p codexIncrementalProjection) equal(other codexIncrementalProjection) bool {
	if p.text != other.text || len(p.calls) != len(other.calls) {
		return false
	}
	for i := range p.calls {
		if p.calls[i] != other.calls[i] {
			return false
		}
	}
	return true
}

// codexIncrementalItemRole classifies an item for the projection.
type codexIncrementalItemRole uint8

const (
	// codexItemNotOutput is anything a client adds: user, developer and system
	// messages, tool outputs, references. It ends the echoed output.
	codexItemNotOutput codexIncrementalItemRole = iota
	// codexItemText is an assistant message.
	codexItemText
	// codexItemCall is a tool call the client executes and answers.
	codexItemCall
	// codexItemSkipped is model output a Chat client never receives: reasoning
	// and tool calls the backend runs itself.
	codexItemSkipped
)

func classifyCodexIncrementalItem(raw string) (codexIncrementalItemRole, string) {
	itemType := gjson.Get(raw, "type").String()
	switch itemType {
	case "message", "":
		// A message without type is still a message in the Responses input.
		if gjson.Get(raw, "role").String() == "assistant" {
			return codexItemText, "message"
		}
		return codexItemNotOutput, itemType
	case "function_call", "custom_tool_call", "local_shell_call":
		return codexItemCall, itemType
	case "reasoning", "web_search_call", "image_generation_call", "file_search_call",
		"code_interpreter_call", "mcp_call", "mcp_list_tools":
		return codexItemSkipped, itemType
	default:
		return codexItemNotOutput, itemType
	}
}

// projectCodexOutputItems projects the output of a response. Any output item
// the projection does not know makes the response unusable as a base, since
// there is no telling whether a client would hand it back.
func projectCodexOutputItems(items []string) (codexIncrementalProjection, string) {
	text := sha256.New()
	var calls []string
	for _, raw := range items {
		role, itemType := classifyCodexIncrementalItem(raw)
		switch role {
		case codexItemText:
			writeCodexAssistantText(text, raw)
		case codexItemCall:
			calls = append(calls, codexIncrementalCallKey(raw, itemType))
		case codexItemSkipped:
		default:
			if itemType == "" {
				itemType = "unknown"
			}
			return codexIncrementalProjection{}, "output_item_unsupported:" + itemType
		}
	}
	return codexIncrementalProjection{text: codexIncrementalDigestSum(text), calls: calls}, ""
}

// projectCodexEchoedItems projects the items a client sent back; every one of
// them is model output by construction (see splitCodexEchoedItems).
func projectCodexEchoedItems(items []codexIncrementalInputItem) codexIncrementalProjection {
	text := sha256.New()
	var calls []string
	for _, item := range items {
		role, itemType := classifyCodexIncrementalItem(item.raw)
		switch role {
		case codexItemText:
			writeCodexAssistantText(text, item.raw)
		case codexItemCall:
			calls = append(calls, codexIncrementalCallKey(item.raw, itemType))
		}
	}
	return codexIncrementalProjection{text: codexIncrementalDigestSum(text), calls: calls}
}

// writeCodexAssistantText appends an assistant message's text. Output carries
// output_text parts; a client may send the text back as a plain string or as
// input_text. Refusals are not streamed to Chat clients and are left out on both
// sides.
func writeCodexAssistantText(h hash.Hash, raw string) {
	content := gjson.Get(raw, "content")
	if content.Type == gjson.String {
		_, _ = h.Write([]byte(content.String()))
		return
	}
	if !content.IsArray() {
		return
	}
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "output_text", "input_text", "text":
			_, _ = h.Write([]byte(part.Get("text").String()))
		}
		return true
	})
}

// codexIncrementalCallKey identifies a tool call by what the client answers it
// with. Item ids and status are left out: with store=false the relay strips
// item ids from history, and clients rewrite status.
func codexIncrementalCallKey(raw, itemType string) string {
	callID := gjson.Get(raw, "call_id").String()
	switch itemType {
	case "function_call":
		args := codexIncrementalDigestOf(gjson.Get(raw, "arguments").String())
		return "function_call|" + callID + "|" + gjson.Get(raw, "name").String() + "|" + hex.EncodeToString(args[:])
	case "custom_tool_call":
		input := codexIncrementalDigestOf(gjson.Get(raw, "input").String())
		return "custom_tool_call|" + callID + "|" + gjson.Get(raw, "name").String() + "|" + hex.EncodeToString(input[:])
	default:
		// local_shell_call: the action object is re-serialised by clients, and
		// the call id alone already names the one call it was.
		return itemType + "|" + callID
	}
}

// splitCodexEchoedItems returns how many leading items of the remainder are
// model output, that is, what the client sent back of the previous response.
func splitCodexEchoedItems(rest []codexIncrementalInputItem) int {
	for i, item := range rest {
		if role, _ := classifyCodexIncrementalItem(item.raw); role == codexItemNotOutput {
			return i
		}
	}
	return len(rest)
}

// codexIncrementalBase is the previous turn of a chain: the full input it had,
// the properties it was sent with, and the response it got.
type codexIncrementalBase struct {
	items      []codexIncrementalDigest
	props      codexIncrementalDigest
	responseID string
	output     codexIncrementalProjection
	// unusable is set when the response cannot be a base; it is the reason.
	unusable string
	// connSerial names the upstream connection holding the response, 0 when it
	// came over HTTP. Only that connection can continue from it.
	connSerial uint64
}

type codexIncrementalMatch struct {
	newItems []codexIncrementalInputItem
	echoed   int
}

// matchCodexIncremental decides whether cur can be sent as an increment on top
// of base. A non-empty reason explains why not.
func matchCodexIncremental(base *codexIncrementalBase, cur *codexIncrementalRequest) (codexIncrementalMatch, string) {
	if base == nil || cur == nil {
		return codexIncrementalMatch{}, "no_previous_turn"
	}
	if base.unusable != "" {
		return codexIncrementalMatch{}, base.unusable
	}
	if base.responseID == "" {
		return codexIncrementalMatch{}, "no_previous_response"
	}
	if base.props != cur.props {
		return codexIncrementalMatch{}, "request_properties_changed"
	}
	if len(cur.items) < len(base.items) {
		return codexIncrementalMatch{}, "input_shortened"
	}
	for i := range base.items {
		if cur.items[i].digest != base.items[i] {
			return codexIncrementalMatch{}, "input_prefix_mismatch"
		}
	}
	rest := cur.items[len(base.items):]
	echoed := splitCodexEchoedItems(rest)
	if !projectCodexEchoedItems(rest[:echoed]).equal(base.output) {
		return codexIncrementalMatch{}, "echoed_output_mismatch"
	}
	if echoed == len(rest) {
		return codexIncrementalMatch{}, "no_new_input"
	}
	return codexIncrementalMatch{newItems: rest[echoed:], echoed: echoed}, ""
}

// codexIncrementalMismatchAt returns the index of the first item where cur stops
// following base's input, or -1 when cur starts with all of it. It is the cheap
// check that picks among the chains of one conversation, and the index tells a
// rewritten history (an early item) from an edited last turn (a late one).
func codexIncrementalMismatchAt(base *codexIncrementalBase, cur *codexIncrementalRequest) int {
	if base == nil || cur == nil || len(base.items) == 0 {
		return 0
	}
	for i := range base.items {
		if i >= len(cur.items) || cur.items[i].digest != base.items[i] {
			return i
		}
	}
	return -1
}

// buildCodexIncrementalCreate builds the websocket message that sends only the
// new items on top of previousResponseID.
func buildCodexIncrementalCreate(cur *codexIncrementalRequest, newItems []codexIncrementalInputItem, previousResponseID string) []byte {
	size := cur.inputStart + len(cur.body) - cur.inputEnd + 2
	for _, item := range newItems {
		size += len(item.raw) + 1
	}
	out := make([]byte, 0, size+len(previousResponseID)+64)
	out = append(out, cur.body[:cur.inputStart]...)
	out = append(out, '[')
	for i, item := range newItems {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item.raw...)
	}
	out = append(out, ']')
	out = append(out, cur.body[cur.inputEnd:]...)
	// The body is small now; sjson copying it is cheap.
	out, _ = sjson.SetBytes(out, "previous_response_id", previousResponseID)
	out, _ = sjson.SetBytes(out, "type", "response.create")
	return out
}

// buildCodexFullCreate builds the websocket message that sends the whole
// request, starting (or restarting) a chain. The history is copied once.
func buildCodexFullCreate(cur *codexIncrementalRequest) []byte {
	body := cur.body
	if cur.hasType || len(body) < 2 {
		out, _ := sjson.SetBytes(bytes.Clone(body), "type", "response.create")
		return out
	}
	const head = `{"type":"response.create"`
	rest := bytes.TrimLeft(body[1:], " \t\r\n")
	out := make([]byte, 0, len(head)+1+len(rest))
	out = append(out, head...)
	if len(rest) > 0 && rest[0] != '}' {
		out = append(out, ',')
	}
	return append(out, rest...)
}
