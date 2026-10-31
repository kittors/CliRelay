package contentmoderation

import (
	"bytes"
	"sort"
	"strconv"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// What the reply filter may change is the text the model wrote for the user.
// Everything else in a reply is protocol: ids, signatures, encrypted reasoning,
// tool calls and their arguments, usage, finish reasons. Reasoning text is left
// alone as well, because some of it is signed and must come back to the
// upstream exactly as it left it. The codecs below find the places that are
// model-written answer text, per protocol, and nothing else.

// replyPiece is one string of model-written text in a JSON document of a reply.
type replyPiece struct {
	// key names the place in the reply the text belongs to (a content block, a
	// choice, an output item). A piece that continues an earlier one has the
	// same key; the text held back from one event moves to the next piece with
	// its key. Keys are only comparable within one stream.
	key string
	// text is the string as decoded from the document.
	text string
	// lit0 and lit1 bound the string literal, quotes included, in the document.
	lit0, lit1 int
	// full says the piece is a whole text: a non-streaming reply, or one of the
	// copies of the answer that streaming protocols repeat at the end. Nothing
	// is held back for it.
	full bool
}

// replyReader collects the pieces of one document.
type replyReader struct {
	doc    []byte
	root   gjson.Result
	pieces []replyPiece
	// untraced counts strings the reader could not locate in the document. Their
	// text is not filtered; the engine reports it.
	untraced int
}

// replyPieces returns the model-written text in a document of a reply in the
// protocol of format, in document order. stream says the document is one event
// of a stream rather than a whole reply.
func replyPieces(format sdktranslator.Format, doc []byte, stream bool) (pieces []replyPiece, untraced int) {
	if !mayHoldReplyText(doc) || !gjson.ValidBytes(doc) {
		return nil, 0
	}
	r := &replyReader{doc: doc, root: gjson.ParseBytes(doc)}
	switch format {
	case sdktranslator.FormatClaude:
		r.claude(stream)
	case sdktranslator.FormatOpenAI:
		r.chat(stream)
	case sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex:
		r.responses(stream)
	case sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI, sdktranslator.FormatAntigravity:
		r.gemini(stream)
	}
	// Splicing walks the document once, front to back.
	sort.SliceStable(r.pieces, func(i, j int) bool { return r.pieces[i].lit0 < r.pieces[j].lit0 })
	kept := r.pieces[:0]
	for _, piece := range r.pieces {
		if len(kept) > 0 && piece.lit0 < kept[len(kept)-1].lit1 {
			continue // the same string reached twice
		}
		kept = append(kept, piece)
	}
	return kept, r.untraced
}

// mayHoldReplyText is a cheap test run on every event of a stream: an event
// with none of the field names that carry text cannot contain any.
func mayHoldReplyText(doc []byte) bool {
	return bytes.Contains(doc, []byte(`"text"`)) || bytes.Contains(doc, []byte(`"content"`)) || bytes.Contains(doc, []byte(`"delta"`))
}

// add records the string at as a piece. Its place in the document is taken from
// gjson and checked against the bytes there, because a piece at a wrong offset
// would corrupt the reply when it is rewritten.
func (r *replyReader) add(key string, at gjson.Result, full bool) {
	if at.Type != gjson.String || at.Index <= 0 {
		if at.Type == gjson.String {
			r.untraced++
		}
		return
	}
	lit0, lit1 := at.Index, at.Index+len(at.Raw)
	if lit1 > len(r.doc) || string(r.doc[lit0:lit1]) != at.Raw {
		r.untraced++
		return
	}
	if decoded, ok := decodeJSONLiteral(r.doc[lit0:lit1]); !ok || decoded != at.String() {
		r.untraced++
		return
	}
	r.pieces = append(r.pieces, replyPiece{key: key, text: at.String(), lit0: lit0, lit1: lit1, full: full || key == ""})
}

func (r *replyReader) claude(stream bool) {
	if !stream {
		for _, block := range arrayItems(r.root.Get("content")) {
			if kind := block.Get("type").String(); kind == "text" || kind == "" {
				r.add("", block.Get("text"), true)
			}
		}
		return
	}
	switch r.root.Get("type").String() {
	case "content_block_delta":
		if delta := r.root.Get("delta"); delta.Get("type").String() == "text_delta" {
			r.add("claude:"+r.root.Get("index").Raw, delta.Get("text"), false)
		}
	case "content_block_start":
		if block := r.root.Get("content_block"); block.Get("type").String() == "text" {
			r.add("", block.Get("text"), true)
		}
	}
}

func (r *replyReader) chat(stream bool) {
	for i, choice := range arrayItems(r.root.Get("choices")) {
		key := "chat:" + strconv.Itoa(i)
		if index := choice.Get("index"); index.Type == gjson.Number {
			key = "chat:" + index.Raw
		}
		if stream {
			r.add(key, choice.Get("delta.content"), false)
			r.add(key+":completion", choice.Get("text"), false)
			continue
		}
		content := choice.Get("message.content")
		if content.Type == gjson.String {
			r.add("", content, true)
		}
		for _, part := range arrayItems(content) {
			if part.Get("type").String() == "text" {
				r.add("", part.Get("text"), true)
			}
		}
		r.add("", choice.Get("text"), true)
	}
}

func (r *replyReader) responses(stream bool) {
	if !stream {
		r.responseOutput(r.root.Get("output"))
		return
	}
	switch r.root.Get("type").String() {
	case "response.output_text.delta":
		key := "resp:" + r.root.Get("item_id").String() + ":" + r.root.Get("output_index").Raw + ":" + r.root.Get("content_index").Raw
		r.add(key, r.root.Get("delta"), false)
	case "response.output_text.done":
		r.add("", r.root.Get("text"), true)
	case "response.content_part.added", "response.content_part.done":
		if part := r.root.Get("part"); part.Get("type").String() == "output_text" {
			r.add("", part.Get("text"), true)
		}
	case "response.output_item.added", "response.output_item.done":
		r.messageItem(r.root.Get("item"))
	case "response.created", "response.in_progress", "response.queued", "response.completed", "response.incomplete", "response.failed":
		// The final events carry the whole answer again.
		r.responseOutput(r.root.Get("response.output"))
	}
}

func (r *replyReader) responseOutput(output gjson.Result) {
	for _, item := range arrayItems(output) {
		r.messageItem(item)
	}
}

func (r *replyReader) messageItem(item gjson.Result) {
	if item.Get("type").String() != "message" {
		return
	}
	for _, part := range arrayItems(item.Get("content")) {
		if part.Get("type").String() == "output_text" {
			r.add("", part.Get("text"), true)
		}
	}
}

func (r *replyReader) gemini(stream bool) {
	root := r.root
	if wrapped := root.Get("response"); wrapped.IsObject() {
		// The CLI and Antigravity protocols wrap the reply in an envelope.
		root = wrapped
	}
	for i, candidate := range arrayItems(root.Get("candidates")) {
		var texts []gjson.Result
		for _, part := range arrayItems(candidate.Get("content.parts")) {
			// A thought summary is the model's reasoning, not its answer.
			if part.Get("text").Type == gjson.String && !part.Get("thought").Bool() {
				texts = append(texts, part.Get("text"))
			}
		}
		// Text carries over from one event to the next only when it is the one
		// text part of its candidate; with several parts there is no telling which
		// of them the held text belongs in front of.
		carries := stream && len(texts) == 1
		key := "gemini:" + strconv.Itoa(i)
		if index := candidate.Get("index"); index.Type == gjson.Number {
			key = "gemini:" + index.Raw
		}
		for _, text := range texts {
			if carries {
				r.add(key, text, false)
			} else {
				r.add("", text, true)
			}
		}
	}
}
