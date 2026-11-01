package contentmoderation

import (
	"bytes"
	"strconv"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// secretReply puts the secrets of one request back into its reply, wherever
// the model wrote their placeholders: in its answer and in the arguments of
// the tools it calls, which the client runs with the real values. Reasoning
// text keeps the placeholders, as the word guard leaves it alone: some of it is
// signed and goes back to the upstream as it came.
type secretReply struct {
	table  *secretTable
	format sdktranslator.Format
}

func newSecretReply(table *secretTable, format sdktranslator.Format) *secretReply {
	return &secretReply{table: table, format: format}
}

// FilterResponse implements coreauth.ReplyFilter.
func (r *secretReply) FilterResponse(resp cliproxyexecutor.Response) cliproxyexecutor.Response {
	return filterWholeReply(r, r.format, resp)
}

// NewStream implements coreauth.ReplyFilter.
func (r *secretReply) NewStream() coreauth.StreamReplyFilter {
	return newWordReplyStream(r, r.format)
}

func (r *secretReply) pieces(format sdktranslator.Format, doc []byte, stream bool) ([]replyPiece, int) {
	if !mayHoldReplyText(doc) && !mayHoldToolInput(doc) || !gjson.ValidBytes(doc) {
		return nil, 0
	}
	reader := &replyReader{doc: doc, root: gjson.ParseBytes(doc)}
	reader.text(format, stream)
	reader.toolInput(format, stream)
	return reader.sorted()
}

func (r *secretReply) filterText(text string, more bool) replyText {
	if !more {
		return replyText{out: r.table.restore(text)}
	}
	// The start of a placeholder at the end waits for the rest of it. The cut
	// cannot fall inside a whole placeholder: every one starts with "{{", and the
	// second brace of one is followed by its kind.
	cut := r.table.partialTail(text)
	return replyText{out: r.table.restore(text[:cut]), held: text[cut:]}
}

// skipReply has nothing to note: the request log says what was masked.
func (r *secretReply) skipReply(string) {}

// mayHoldToolInput is mayHoldReplyText for the arguments of tool calls.
func mayHoldToolInput(doc []byte) bool {
	return bytes.Contains(doc, []byte(`"arguments"`)) || bytes.Contains(doc, []byte(`"partial_json"`)) ||
		bytes.Contains(doc, []byte(`"input"`)) || bytes.Contains(doc, []byte(`"args"`))
}

// toolInput reads the arguments of the tool calls in the document. A stream
// sends them in pieces of JSON text; a whole reply has them as a JSON string
// (OpenAI) or as an object, whose strings are then read one by one.
func (r *replyReader) toolInput(format sdktranslator.Format, stream bool) {
	switch format {
	case sdktranslator.FormatClaude:
		r.claudeToolInput(stream)
	case sdktranslator.FormatOpenAI:
		r.chatToolInput(stream)
	case sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex:
		r.responsesToolInput(stream)
	case sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity:
		r.geminiToolInput()
	}
}

func (r *replyReader) claudeToolInput(stream bool) {
	if !stream {
		for _, block := range arrayItems(r.root.Get("content")) {
			if block.Get("type").String() == "tool_use" {
				r.addStrings(block.Get("input"))
			}
		}
		return
	}
	switch r.root.Get("type").String() {
	case "content_block_delta":
		if delta := r.root.Get("delta"); delta.Get("type").String() == "input_json_delta" {
			r.add("claude:"+r.root.Get("index").Raw, delta.Get("partial_json"), false)
		}
	case "content_block_start":
		if block := r.root.Get("content_block"); block.Get("type").String() == "tool_use" {
			r.addStrings(block.Get("input"))
		}
	}
}

func (r *replyReader) chatToolInput(stream bool) {
	for i, choice := range arrayItems(r.root.Get("choices")) {
		key := "chat:" + strconv.Itoa(i)
		if index := choice.Get("index"); index.Type == gjson.Number {
			key = "chat:" + index.Raw
		}
		if !stream {
			for _, call := range arrayItems(choice.Get("message.tool_calls")) {
				r.add("", call.Get("function.arguments"), true)
			}
			r.add("", choice.Get("message.function_call.arguments"), true)
			continue
		}
		for j, call := range arrayItems(choice.Get("delta.tool_calls")) {
			callKey := key + ":tool:" + strconv.Itoa(j)
			if index := call.Get("index"); index.Type == gjson.Number {
				callKey = key + ":tool:" + index.Raw
			}
			r.add(callKey, call.Get("function.arguments"), false)
		}
		r.add(key+":function", choice.Get("delta.function_call.arguments"), false)
	}
}

func (r *replyReader) responsesToolInput(stream bool) {
	if !stream {
		r.responseToolItems(r.root.Get("output"))
		return
	}
	key := "resptool:" + r.root.Get("item_id").String() + ":" + r.root.Get("output_index").Raw
	switch r.root.Get("type").String() {
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		r.add(key, r.root.Get("delta"), false)
	case "response.function_call_arguments.done":
		r.add("", r.root.Get("arguments"), true)
	case "response.custom_tool_call_input.done":
		r.add("", r.root.Get("input"), true)
	case "response.output_item.added", "response.output_item.done":
		r.responseToolItem(r.root.Get("item"))
	case "response.created", "response.in_progress", "response.queued", "response.completed", "response.incomplete", "response.failed":
		r.responseToolItems(r.root.Get("response.output"))
	}
}

func (r *replyReader) responseToolItems(output gjson.Result) {
	for _, item := range arrayItems(output) {
		r.responseToolItem(item)
	}
}

func (r *replyReader) responseToolItem(item gjson.Result) {
	switch item.Get("type").String() {
	case "function_call":
		r.add("", item.Get("arguments"), true)
	case "custom_tool_call":
		r.add("", item.Get("input"), true)
	}
}

func (r *replyReader) geminiToolInput() {
	root := r.root
	if wrapped := root.Get("response"); wrapped.IsObject() {
		root = wrapped
	}
	for _, candidate := range arrayItems(root.Get("candidates")) {
		for _, part := range arrayItems(candidate.Get("content.parts")) {
			r.addStrings(part.Get("functionCall.args"))
		}
	}
}

// addStrings adds every string value inside value, an object or an array, as
// a whole piece.
func (r *replyReader) addStrings(value gjson.Result) {
	switch {
	case value.Type == gjson.String:
		r.add("", value, true)
	case value.IsObject() || value.IsArray():
		value.ForEach(func(_, item gjson.Result) bool {
			r.addStrings(item)
			return true
		})
	}
}

// chainReplies applies first and then second to a reply. Either may be nil.
func chainReplies(first, second coreauth.ReplyFilter) coreauth.ReplyFilter {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	}
	return &chainedReply{first: first, second: second}
}

type chainedReply struct {
	first, second coreauth.ReplyFilter
}

func (c *chainedReply) FilterResponse(resp cliproxyexecutor.Response) cliproxyexecutor.Response {
	return c.second.FilterResponse(c.first.FilterResponse(resp))
}

func (c *chainedReply) NewStream() coreauth.StreamReplyFilter {
	return &chainedStream{first: c.first.NewStream(), second: c.second.NewStream()}
}

// Abandoned passes the SDK's notice on to the filters that take it.
func (c *chainedReply) Abandoned(recovered any) {
	abandon(c.first, recovered)
	abandon(c.second, recovered)
}

type chainedStream struct {
	first, second coreauth.StreamReplyFilter
}

func (s *chainedStream) Push(chunk []byte) [][]byte {
	var out [][]byte
	for _, passed := range s.first.Push(chunk) {
		out = append(out, s.second.Push(passed)...)
	}
	return out
}

func (s *chainedStream) Flush() [][]byte {
	var out [][]byte
	for _, passed := range s.first.Flush() {
		out = append(out, s.second.Push(passed)...)
	}
	return append(out, s.second.Flush()...)
}

func (s *chainedStream) HeldSince() (time.Time, bool) {
	firstAt, firstHeld := s.first.HeldSince()
	secondAt, secondHeld := s.second.HeldSince()
	switch {
	case firstHeld && secondHeld:
		if secondAt.Before(firstAt) {
			return secondAt, true
		}
		return firstAt, true
	case firstHeld:
		return firstAt, true
	default:
		return secondAt, secondHeld
	}
}

func (s *chainedStream) Abandoned(recovered any) {
	abandon(s.first, recovered)
	abandon(s.second, recovered)
}

func abandon(filter any, recovered any) {
	if a, ok := filter.(interface{ Abandoned(any) }); ok {
		a.Abandoned(recovered)
	}
}
