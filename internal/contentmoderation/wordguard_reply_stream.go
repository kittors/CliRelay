package contentmoderation

import (
	"bytes"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// wordReplyStream filters one stream of events.
//
// A word can arrive in two pieces, in two events. To still see it whole, the
// end of an event's text that could be the beginning of a match is not sent
// with the event: the event is held back, and when the next event for the same
// place in the reply arrives the held text moves to its front and is filtered
// together with it. The event that was held goes out first, without that text,
// and the next one carries it, so the text of the reply is the same, only
// shifted a little towards the end.
//
// Only an event whose text ends in something that could still become a match
// is held, and only until the next event: when that is not text for the same
// place (the block ends, the stream ends, another place speaks), or nothing
// comes for a while (see HeldSince), the event is sent with its text as it was,
// the end now settled. Comments and pings pass at once and change nothing.
type wordReplyStream struct {
	engine *replyEngine
	format sdktranslator.Format

	// rest is the start of an event whose end has not arrived yet.
	rest []byte
	held *heldReply
	// heldAt is when held was taken.
	heldAt time.Time
}

// heldReply is an event waiting for the text that follows it.
type heldReply struct {
	frame  replyFrame
	pieces []replyPiece
	// out is, per piece, the text to send, up to what is held back; tail is what
	// is held back, as written.
	out, tail []string
}

func newWordReplyStream(engine *replyEngine, format sdktranslator.Format) *wordReplyStream {
	return &wordReplyStream{engine: engine, format: format}
}

// Abandoned is called by the SDK when it stopped using this filter because it
// panicked: the rest of the reply goes out unfiltered, and the request log says
// so.
func (s *wordReplyStream) Abandoned(any) { s.engine.snapshot.skipReply("filter_error") }

// Push implements the SDK's StreamReplyFilter.
func (s *wordReplyStream) Push(chunk []byte) [][]byte {
	if len(s.rest) == 0 {
		if frame, ok := bareJSONFrame(chunk); ok {
			return s.frame(frame)
		}
	}
	buf := chunk
	if len(s.rest) > 0 {
		buf = append(append(make([]byte, 0, len(s.rest)+len(chunk)), s.rest...), chunk...)
	}
	frames, rest := splitFrames(buf)
	if len(rest) > 0 && !bytes.HasSuffix(chunk, []byte("\n")) {
		// A producer that ends a chunk in the middle of a line has not finished
		// an event; one that stops after its last character has: the HTTP handler
		// adds the line ends to those.
		frames = append(frames, rest)
		rest = nil
	}
	if len(rest) > maxReplyFrame {
		frames = append(frames, rest)
		rest = nil
	}
	s.rest = append(s.rest[:0], rest...)
	var out [][]byte
	for _, raw := range frames {
		out = append(out, s.frame(classifyFrame(raw))...)
	}
	return out
}

// Flush implements StreamReplyFilter: it sends everything held back, text and
// unfinished event alike.
func (s *wordReplyStream) Flush() [][]byte {
	out, _ := s.release(nil)
	if len(s.rest) > 0 {
		out = append(out, bytes.Clone(s.rest))
		s.rest = s.rest[:0]
	}
	return out
}

// HeldSince implements StreamReplyFilter.
func (s *wordReplyStream) HeldSince() (time.Time, bool) {
	return s.heldAt, s.held != nil
}

// frame handles one complete event and returns the chunks that may go out.
func (s *wordReplyStream) frame(f replyFrame) [][]byte {
	switch f.kind {
	case frameMeta:
		return [][]byte{f.raw}
	case frameOther:
		out, _ := s.release(nil)
		return append(out, f.raw)
	}
	pieces, untraced := replyPieces(s.format, f.data, true)
	if untraced > 0 {
		s.engine.snapshot.skipReply("untraceable_text")
	}
	if len(pieces) == 0 {
		if isPingEvent(f.data) {
			return [][]byte{f.raw} // a ping says nothing about the text around it
		}
		out, _ := s.release(nil)
		return append(out, f.raw)
	}
	out, carry := s.release(pieces)
	h := &heldReply{frame: f, pieces: pieces, out: make([]string, len(pieces)), tail: make([]string, len(pieces))}
	holding := false
	for i, piece := range pieces {
		var r replyText
		if piece.full {
			r = s.engine.filterText(piece.text, false)
		} else {
			r = s.engine.filterText(carry[i]+piece.text, true)
		}
		h.out[i], h.tail[i] = r.out, r.held
		holding = holding || r.held != ""
	}
	if !holding {
		return append(out, h.build())
	}
	// The event may be a slice of the caller's chunk, which the caller may reuse.
	raw := bytes.Clone(f.raw)
	h.frame.raw = raw
	h.frame.data = raw[f.dataAt : f.dataAt+len(f.data)]
	s.held, s.heldAt = h, time.Now()
	return out
}

// release sends the held event. Text it holds back moves to the piece of next
// that continues it, and is returned by the index of that piece; text with no
// such piece to move to is decided now, with its end final, and stays in the
// event.
func (s *wordReplyStream) release(next []replyPiece) ([][]byte, map[int]string) {
	h := s.held
	if h == nil {
		return nil, nil
	}
	s.held = nil
	var carry map[int]string
	for i := range h.pieces {
		if h.tail[i] == "" {
			continue
		}
		for j, candidate := range next {
			if _, taken := carry[j]; !taken && !candidate.full && candidate.key == h.pieces[i].key {
				if carry == nil {
					carry = make(map[int]string)
				}
				carry[j], h.tail[i] = h.tail[i], ""
				break
			}
		}
		if h.tail[i] != "" {
			h.out[i] += s.engine.filterText(h.tail[i], false).out
			h.tail[i] = ""
		}
	}
	return [][]byte{h.build()}, carry
}

// build writes the event with the text of its pieces replaced by out. An event
// whose text did not change is returned as it came, byte for byte; otherwise
// only the string literals of the pieces are rewritten.
func (h *heldReply) build() []byte {
	data := h.frame.data
	var edited []byte
	cursor := 0
	for i, piece := range h.pieces {
		if h.out[i] == piece.text {
			continue
		}
		edited = append(edited, data[cursor:piece.lit0]...)
		edited = append(edited, '"')
		edited = appendJSONEscaped(edited, h.out[i])
		edited = append(edited, '"')
		cursor = piece.lit1
	}
	if edited == nil {
		return h.frame.raw
	}
	edited = append(edited, data[cursor:]...)
	if !gjson.ValidBytes(edited) {
		// Unreachable for text that was escaped, kept as a guard: an event that is
		// not JSON would break the client's parser.
		return h.frame.raw
	}
	raw := h.frame.raw
	out := make([]byte, 0, len(raw)+len(edited)-len(data))
	out = append(out, raw[:h.frame.dataAt]...)
	out = append(out, edited...)
	return append(out, raw[h.frame.dataAt+len(data):]...)
}

// isPingEvent recognises the keep-alive events some protocols send as data.
func isPingEvent(doc []byte) bool {
	return bytes.Contains(doc, []byte(`"ping"`)) && gjson.GetBytes(doc, "type").String() == "ping"
}
