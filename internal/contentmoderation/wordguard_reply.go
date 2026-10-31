package contentmoderation

import (
	"bytes"
	"compress/gzip"
	"io"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// maxReplyBody bounds a compressed reply the filter will unpack. A larger one
// goes to the client unfiltered, and the request log says so.
const maxReplyBody = 64 << 20

// wordReply is the word guard's filter for the reply to one request. It
// implements the SDK's ReplyFilter.
type wordReply struct {
	engine *replyEngine
	format sdktranslator.Format
}

// newWordReply returns the reply filter for the contexts that ask for one, or
// nil when none does.
func newWordReply(contexts []wordContext, format sdktranslator.Format, snapshot *WordGuardSnapshot) coreauth.ReplyFilter {
	engine := newReplyEngine(contexts, snapshot)
	if engine == nil {
		return nil
	}
	return &wordReply{engine: engine, format: format}
}

// NewStream implements coreauth.ReplyFilter.
func (w *wordReply) NewStream() coreauth.StreamReplyFilter {
	return newWordReplyStream(w.engine, w.format)
}

// FilterResponse implements coreauth.ReplyFilter for a reply that arrives in
// one piece.
//
// Executors decode a compressed upstream body before they hand it on, so the
// payload is normally plain JSON whatever the Content-Encoding header says. A
// payload that is still gzip is unpacked, filtered and returned plain, with the
// encoding header gone; any other encoding cannot be read and is left alone.
func (w *wordReply) FilterResponse(resp cliproxyexecutor.Response) cliproxyexecutor.Response {
	doc := resp.Payload
	if len(doc) == 0 {
		return resp
	}
	unpacked := false
	if bytes.HasPrefix(doc, []byte{0x1f, 0x8b}) {
		plain, err := gunzipLimited(doc, maxReplyBody)
		if err != nil {
			w.engine.snapshot.skipReply("compressed_body")
			return resp
		}
		doc, unpacked = plain, true
	}
	pieces, untraced := replyPieces(w.format, doc, false)
	if untraced > 0 {
		w.engine.snapshot.skipReply("untraceable_text")
	}
	if len(pieces) == 0 {
		if enc := resp.Headers.Get("Content-Encoding"); enc != "" && enc != "identity" && !unpacked && !looksLikeJSON(doc) {
			w.engine.snapshot.skipReply("unsupported_encoding")
		}
		return resp
	}
	h := &heldReply{
		frame:  replyFrame{raw: doc, data: doc, kind: frameData},
		pieces: pieces,
		out:    make([]string, len(pieces)),
		tail:   make([]string, len(pieces)),
	}
	for i, piece := range pieces {
		h.out[i] = w.engine.filterText(piece.text, false).out
	}
	if !h.changed() {
		return resp
	}
	resp.Payload = h.build()
	// The body is no longer the one the upstream described.
	resp.Headers = resp.Headers.Clone()
	resp.Headers.Del("Content-Length")
	resp.Headers.Del("Content-Encoding")
	return resp
}

// changed reports whether any piece's text differs from what was received.
func (h *heldReply) changed() bool {
	for i, piece := range h.pieces {
		if h.out[i] != piece.text {
			return true
		}
	}
	return false
}

func looksLikeJSON(doc []byte) bool {
	doc = bytes.TrimSpace(doc)
	return len(doc) > 0 && (doc[0] == '{' || doc[0] == '[')
}

// gunzipLimited unpacks a gzip body, refusing one that grows beyond limit.
func gunzipLimited(body []byte, limit int64) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	plain, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) > limit {
		return nil, io.ErrShortBuffer
	}
	return plain, nil
}
