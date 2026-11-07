package loopdetect

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// ReplyGuard reads a streamed reply as it is sent to the client and says when
// its reasoning or its text has become a loop. One guard serves one reply and
// is not safe for concurrent use: the goroutine that forwards the stream feeds
// it.
//
// The reply is cut into units, its lines and sentences. A loop is a window of
// the latest units of which no more than a few are different: the model said
// "Now." "Writing." "Go." "Let me write." "OK." over and over, a word or two
// changing between rounds, until its output limit. The cycle need not be exact.
// The reasoning and the text are counted apart, because a reply may reason in
// one voice and answer in another.
type ReplyGuard struct {
	limits    ReplyLimits
	reasoning channel
	text      channel
	chunks    int
	bytes     int64
}

// NewReplyGuard returns a guard with limits held to their bounds.
func NewReplyGuard(limits ReplyLimits) *ReplyGuard {
	return &ReplyGuard{limits: limits.Normalized()}
}

// Limits returns the thresholds the guard runs with.
func (g *ReplyGuard) Limits() ReplyLimits { return g.limits }

// Feed reads chunk, the next of the reply as one of the client protocols
// writes it (a whole server-sent event, or a whole JSON object), and returns the
// trip when the reply has become a loop with it. Nothing is read after a trip.
func (g *ReplyGuard) Feed(chunk []byte) (ReplyTrip, bool) {
	g.chunks++
	g.bytes += int64(len(chunk))
	reasoning, text := replyText(chunk)
	if reasoning != "" {
		if trip, ok := g.reasoning.add(reasoning, g.limits); ok {
			trip.Reasoning = true
			return g.located(trip), true
		}
	}
	if text != "" {
		if trip, ok := g.text.add(text, g.limits); ok {
			return g.located(trip), true
		}
	}
	return ReplyTrip{}, false
}

// located says where in the reply the cut was made.
func (g *ReplyGuard) located(trip ReplyTrip) ReplyTrip {
	trip.Chunks, trip.Bytes = g.chunks, g.bytes
	return trip
}

var (
	deltaMark      = []byte("delta")
	candidatesMark = []byte("candidates")
	dataMark       = []byte("data:")
)

// replyText returns the reasoning and the answer text one chunk carries, in any
// of the client protocols. A tool call's arguments, a start, a stop and usage
// say nothing here: a tool writing a file of repeated lines is not a loop.
func replyText(chunk []byte) (reasoning, text string) {
	// Most events that carry no text, without reading them.
	if !bytes.Contains(chunk, deltaMark) && !bytes.Contains(chunk, candidatesMark) {
		return "", ""
	}
	var r, t strings.Builder
	eachPayload(chunk, func(payload []byte) {
		payloadText(payload, &r, &t)
	})
	return r.String(), t.String()
}

// eachPayload calls fn with the JSON of every event in chunk: the data lines of
// a server-sent event, or the whole chunk when it is the JSON itself (Chat
// Completions and Gemini hand the handler bare objects).
func eachPayload(chunk []byte, fn func(payload []byte)) {
	chunk = bytes.TrimSpace(chunk)
	if len(chunk) == 0 {
		return
	}
	if chunk[0] == '{' {
		fn(chunk)
		return
	}
	for len(chunk) > 0 {
		var line []byte
		line, chunk, _ = bytes.Cut(chunk, []byte("\n"))
		if rest, ok := bytes.CutPrefix(bytes.TrimSpace(line), dataMark); ok {
			if rest = bytes.TrimSpace(rest); len(rest) > 0 && rest[0] == '{' {
				fn(rest)
			}
		}
	}
}

// payloadText adds the reasoning and the text one event carries to r and t.
func payloadText(payload []byte, r, t *strings.Builder) {
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	switch root.Get("type").String() {
	case "content_block_delta": // Claude
		delta := root.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			writeString(t, delta.Get("text"))
		case "thinking_delta":
			writeString(r, delta.Get("thinking"))
		}
		return
	case "response.output_text.delta": // Responses
		writeString(t, root.Get("delta"))
		return
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		writeString(r, root.Get("delta"))
		return
	case "":
		// Chat Completions and Gemini events carry no type.
	default:
		return
	}
	root.Get("choices").ForEach(func(_, choice gjson.Result) bool { // Chat Completions
		delta := choice.Get("delta")
		if v := delta.Get("reasoning_content"); v.Type == gjson.String {
			r.WriteString(v.Str)
		} else {
			writeString(r, delta.Get("reasoning"))
		}
		writeString(t, delta.Get("content"))
		return true
	})
	candidates := root.Get("candidates") // Gemini
	if !candidates.Exists() {
		candidates = root.Get("response.candidates") // Gemini CLI wraps it
	}
	candidates.ForEach(func(_, candidate gjson.Result) bool {
		candidate.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			if part.Get("functionCall").Exists() {
				return true
			}
			if part.Get("thought").Bool() {
				writeString(r, part.Get("text"))
			} else {
				writeString(t, part.Get("text"))
			}
			return true
		})
		return true
	})
}

// writeString writes v when it is a JSON string; null, numbers and objects in
// its place (a Chat delta's content is null while a tool is called) say nothing.
func writeString(b *strings.Builder, v gjson.Result) {
	if v.Type == gjson.String {
		b.WriteString(v.Str)
	}
}
