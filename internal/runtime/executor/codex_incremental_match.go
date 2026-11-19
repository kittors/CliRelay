package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sort"
	"strings"
	"unsafe"

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
	return codexIncrementalDigestOfBytes(codexIncrementalStringBytes(raw))
}

func codexIncrementalDigestOfBytes(raw []byte) codexIncrementalDigest {
	sum := sha256.Sum256(raw)
	var digest codexIncrementalDigest
	copy(digest[:], sum[:16])
	return digest
}

func codexIncrementalDigestSum(h hash.Hash) codexIncrementalDigest {
	var digest codexIncrementalDigest
	copy(digest[:], h.Sum(nil))
	return digest
}

// codexIncrementalStringBytes is s as a byte slice, without the copy []byte(s)
// makes. It is only ever handed to hashes, which read what they are given and
// keep none of it (the io.Writer contract); converting instead copied every
// input item once more on every turn, as many bytes again as the history.
func codexIncrementalStringBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
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

// codexIncrementalInputItem is one input item: where it lies in the body and
// its digest. Its bytes stay in the body. A copy of them, which the matcher kept
// until it was done with the body, doubled what every turn held: the body is
// the request in flight, several megabytes of resent screenshots more often
// than not and tens of megabytes at times.
type codexIncrementalInputItem struct {
	start, end int
	digest     codexIncrementalDigest
}

func (item codexIncrementalInputItem) size() int { return item.end - item.start }

// itemView returns an item's bytes as a string that shares the body's memory.
// The body is final once a turn is compared (it is what goes upstream) and is
// never written afterwards, so the view stays what it was; it is empty once the
// content is dropped. Nothing read through it may be kept past the turn: a
// substring would hold the whole body. Values a turn keeps are cloned.
func (r *codexIncrementalRequest) itemView(item codexIncrementalInputItem) string {
	if r == nil || item.start < 0 || item.end > len(r.body) || item.start >= item.end {
		return ""
	}
	return unsafe.String(&r.body[item.start], item.size())
}

// parseCodexIncrementalRequest reads the input items and request properties of
// an outbound body. The input is read once: on a multi-megabyte history every
// extra pass costs a full scan. A non-empty reason means the body cannot take
// part (no input array, malformed JSON).
//
// The body is read through a string view rather than copied: gjson.GetBytes
// copies every result it returns, which for the input is nearly the whole
// request, and the request properties were read from another copy of the rest.
// Only digests, offsets and cloned strings are kept (see itemView).
func parseCodexIncrementalRequest(body []byte) (*codexIncrementalRequest, string) {
	if len(body) == 0 || body[0] != '{' {
		return nil, "body_not_object"
	}
	view := unsafe.String(unsafe.SliceData(body), len(body))
	root := gjson.Parse(view)
	if !root.IsObject() {
		return nil, "body_not_object"
	}
	req := &codexIncrementalRequest{body: body}
	var input gjson.Result
	type field struct{ key, raw string }
	fields := make([]field, 0, 16)
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		switch name {
		case "type":
			req.hasType = true
		case "prompt_cache_key":
			// Kept past the turn, in the connection's identity.
			req.promptCacheKey = strings.Clone(strings.TrimSpace(value.String()))
		case "input":
			// The first one, as gjson's path lookup reads it; inference entries
			// refuse bodies with duplicate keys before they get here.
			if !input.Exists() {
				input = value
			}
		}
		if _, ignored := codexIncrementalIgnoredFields[name]; ignored {
			return true
		}
		fields = append(fields, field{key: name, raw: value.Raw})
		return true
	})
	if !input.IsArray() {
		return nil, "input_not_array"
	}
	start := input.Index
	end := start + len(input.Raw)
	if start <= 0 || end > len(body) || body[start] != '[' || body[end-1] != ']' || !codexIncrementalViewAt(body, start, input.Raw) {
		return nil, "input_offset_unknown"
	}
	req.inputStart, req.inputEnd = start, end

	sort.SliceStable(fields, func(i, j int) bool { return fields[i].key < fields[j].key })
	h := sha256.New()
	for _, f := range fields {
		writeCodexIncrementalHashString(h, f.key)
		writeCodexIncrementalHashString(h, f.raw)
	}
	req.props = codexIncrementalDigestSum(h)

	offsetsKnown := true
	input.ForEach(func(_, item gjson.Result) bool {
		itemStart := item.Index
		itemEnd := itemStart + len(item.Raw)
		if itemStart <= start || itemEnd >= end || !codexIncrementalViewAt(body, itemStart, item.Raw) {
			offsetsKnown = false
			return false
		}
		req.items = append(req.items, codexIncrementalInputItem{start: itemStart, end: itemEnd, digest: codexIncrementalDigestOfBytes(body[itemStart:itemEnd])})
		return true
	})
	if !offsetsKnown {
		return nil, "input_offset_unknown"
	}
	return req, ""
}

// codexIncrementalViewAt reports whether raw is the part of body that starts at
// offset start, the same memory and not an equal copy, so an offset gjson
// reported can be trusted without comparing the bytes.
func codexIncrementalViewAt(body []byte, start int, raw string) bool {
	if raw == "" || start < 0 || start+len(raw) > len(body) {
		return false
	}
	return unsafe.StringData(raw) == &body[start]
}

// writeCodexIncrementalHashString writes s length-prefixed, so two fields can
// never run together into the same bytes.
func writeCodexIncrementalHashString(h hash.Hash, s string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(s)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(codexIncrementalStringBytes(s))
}

// dropContent lets the body go once nothing will be built from it; the digests
// and properties stay for the next turn's comparison.
func (r *codexIncrementalRequest) dropContent() {
	if r == nil {
		return
	}
	r.body = nil
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
	text codexIncrementalDigest
	// textBytes is the joined text's length: part of the identity, and what the
	// request log can say about text that differs without saying what it was.
	textBytes int
	calls     []codexIncrementalCall
}

func (p codexIncrementalProjection) equal(other codexIncrementalProjection) bool {
	if p.text != other.text || p.textBytes != other.textBytes || len(p.calls) != len(other.calls) {
		return false
	}
	for i := range p.calls {
		if p.calls[i] != other.calls[i] {
			return false
		}
	}
	return true
}

// empty reports a response a client has nothing to hand back of: no text and
// no tool call, only reasoning or hosted tools.
func (p codexIncrementalProjection) empty() bool {
	return p.textBytes == 0 && len(p.calls) == 0
}

// codexIncrementalEchoDiff says where echoed output parted from the previous
// response, for the request log: which parts differ and their sizes, never what
// they held.
type codexIncrementalEchoDiff struct {
	// parts are the differing parts, joined by "+": text, then the first call
	// that differs as call_kind, call_id, call_name or call_payload, or
	// call_count when the calls agree as far as the shorter list goes.
	parts string
	// at is the index of the call named in parts, -1 when no call differs.
	at                             int
	baseCalls, echoedCalls         int
	baseTextBytes, echoedTextBytes int
}

// diff compares the previous response's projection p with what a client sent
// back of it.
func (p codexIncrementalProjection) diff(echoed codexIncrementalProjection) codexIncrementalEchoDiff {
	d := codexIncrementalEchoDiff{
		at:        -1,
		baseCalls: len(p.calls), echoedCalls: len(echoed.calls),
		baseTextBytes: p.textBytes, echoedTextBytes: echoed.textBytes,
	}
	var parts []string
	if p.text != echoed.text || p.textBytes != echoed.textBytes {
		parts = append(parts, "text")
	}
	shorter := min(len(p.calls), len(echoed.calls))
	for i := 0; i < shorter && d.at < 0; i++ {
		a, b := p.calls[i], echoed.calls[i]
		part := ""
		switch {
		case a.kind != b.kind:
			part = "call_kind"
		case a.callID != b.callID:
			part = "call_id"
		case a.name != b.name:
			part = "call_name"
		case a.payload != b.payload:
			part = "call_payload"
		}
		if part != "" {
			parts = append(parts, part)
			d.at = i
		}
	}
	if d.at < 0 && len(p.calls) != len(echoed.calls) {
		parts = append(parts, "call_count")
		d.at = shorter
	}
	d.parts = strings.Join(parts, "+")
	return d
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
	textBytes := 0
	var calls []codexIncrementalCall
	for _, raw := range items {
		role, itemType := classifyCodexIncrementalItem(raw)
		switch role {
		case codexItemText:
			textBytes += writeCodexAssistantText(text, raw)
		case codexItemCall:
			calls = append(calls, codexIncrementalCallOf(raw, itemType))
		case codexItemSkipped:
		default:
			if itemType == "" {
				itemType = "unknown"
			}
			return codexIncrementalProjection{}, "output_item_unsupported:" + itemType
		}
	}
	return codexIncrementalProjection{text: codexIncrementalDigestSum(text), textBytes: textBytes, calls: calls}, ""
}

// projectEchoed projects the items a client sent back; every one of them is
// model output by construction (see splitEchoed).
func (r *codexIncrementalRequest) projectEchoed(items []codexIncrementalInputItem) codexIncrementalProjection {
	text := sha256.New()
	textBytes := 0
	var calls []codexIncrementalCall
	for _, item := range items {
		raw := r.itemView(item)
		role, itemType := classifyCodexIncrementalItem(raw)
		switch role {
		case codexItemText:
			textBytes += writeCodexAssistantText(text, raw)
		case codexItemCall:
			calls = append(calls, codexIncrementalCallOf(raw, itemType))
		}
	}
	return codexIncrementalProjection{text: codexIncrementalDigestSum(text), textBytes: textBytes, calls: calls}
}

// writeCodexAssistantText appends an assistant message's text and returns how
// many bytes it added. Output carries output_text parts; a client may send the
// text back as a plain string or as input_text. Refusals are not streamed to
// Chat clients and are left out on both sides.
func writeCodexAssistantText(h hash.Hash, raw string) int {
	content := gjson.Get(raw, "content")
	if content.Type == gjson.String {
		text := content.String()
		_, _ = h.Write(codexIncrementalStringBytes(text))
		return len(text)
	}
	if !content.IsArray() {
		return 0
	}
	written := 0
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "output_text", "input_text", "text":
			text := part.Get("text").String()
			_, _ = h.Write(codexIncrementalStringBytes(text))
			written += len(text)
		}
		return true
	})
	return written
}

// codexIncrementalCall identifies a tool call by what the client answers it
// with. Item ids and status are left out: with store=false the relay strips
// item ids from history, and clients rewrite status. Its strings are cloned, so
// a base kept between turns holds a few short strings, not the event or the body
// they were read from.
type codexIncrementalCall struct {
	kind, callID, name string
	// payload identifies a function call's arguments by their JSON value (see
	// codexIncrementalArgumentsDigest) and a custom tool call's input as
	// written; local_shell_call has none, its call id alone names the one call
	// it was (clients re-serialise its action object).
	payload codexIncrementalDigest
}

func codexIncrementalCallOf(raw, itemType string) codexIncrementalCall {
	call := codexIncrementalCall{callID: strings.Clone(gjson.Get(raw, "call_id").String())}
	switch itemType {
	case "function_call":
		call.kind = "function_call"
		call.name = strings.Clone(gjson.Get(raw, "name").String())
		call.payload = codexIncrementalArgumentsDigest(gjson.Get(raw, "arguments").String())
	case "custom_tool_call":
		call.kind = "custom_tool_call"
		call.name = strings.Clone(gjson.Get(raw, "name").String())
		call.payload = codexIncrementalDigestOf(gjson.Get(raw, "input").String())
	default:
		call.kind = "local_shell_call"
	}
	return call
}

// splitEchoed returns how many leading items of the remainder are model output,
// that is, what the client sent back of the previous response.
func (r *codexIncrementalRequest) splitEchoed(rest []codexIncrementalInputItem) int {
	for i, item := range rest {
		if role, _ := classifyCodexIncrementalItem(r.itemView(item)); role == codexItemNotOutput {
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
	// diff locates an echoed_output_mismatch for the request log.
	diff *codexIncrementalEchoDiff
	// after is how many items followed the previous input, for the request
	// log of a turn that did not continue it.
	after int
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
	echoed := cur.splitEchoed(rest)
	if echoed == 0 && !base.output.empty() {
		// The previous input came back without the response: a retry of the
		// same turn, or one whose reply the client never got or threw away (an
		// interrupted turn). An increment would carry a reply the client does
		// not have. Told apart from an edited reply, which is a different fault.
		return codexIncrementalMatch{after: len(rest)}, "output_not_echoed"
	}
	if projection := cur.projectEchoed(rest[:echoed]); !projection.equal(base.output) {
		diff := base.output.diff(projection)
		return codexIncrementalMatch{echoed: echoed, diff: &diff, after: len(rest)}, "echoed_output_mismatch"
	}
	if echoed == len(rest) {
		return codexIncrementalMatch{}, "no_new_input"
	}
	return codexIncrementalMatch{newItems: rest[echoed:], echoed: echoed, after: len(rest)}, ""
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
		size += item.size() + 1
	}
	out := make([]byte, 0, size+len(previousResponseID)+64)
	out = append(out, cur.body[:cur.inputStart]...)
	out = append(out, '[')
	for i, item := range newItems {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, cur.body[item.start:item.end]...)
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
