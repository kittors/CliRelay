package contentmoderation

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"unicode"
)

// Class says where a fragment of moderated text came from, which decides the
// checks it goes through (see Evaluator.Evaluate).
type Class uint8

const (
	// ClassPrimary is prose the caller wrote: user turns and system or
	// developer prompts. Keyword checks and the moderation backend both see it.
	ClassPrimary Class = iota
	// ClassToolOutput is text the client attached rather than wrote: tool
	// results, documents, and context a client injects into user turns.
	// Keyword checks always see it. The backend sees it only when the profile
	// sets api_includes_tool_output: it is often most of an agent's request, and
	// the backend is paid for, or rate limited, per token.
	ClassToolOutput
	// ClassAssistantHistory is replayed assistant/model turns. The client writes
	// the history it sends, so a fabricated assistant turn can carry anything and
	// must not be a blind spot. It is still mostly genuine model output, which a
	// classifier would flag merely for discussing a sensitive topic, so only the
	// exact keyword check sees it.
	ClassAssistantHistory

	classCount = 3
)

func (c Class) String() string {
	switch c {
	case ClassToolOutput:
		return "tool_output"
	case ClassAssistantHistory:
		return "assistant_history"
	default:
		return "primary"
	}
}

// Segment is one whitespace-collapsed text fragment of a request.
type Segment struct {
	Class Class
	// User marks primary text from a user turn, as opposed to a system or
	// developer prompt. It identifies the newest user message, which the
	// budgeted backends must always scan.
	User bool
	// Turn numbers the message a fragment came from. Fragments of one message
	// share it, and it increases through the conversation.
	Turn int
	Text string
}

// Input is the classified text of one request in conversation order.
type Input struct {
	Segments []Segment
	// memo is shared by copies of the Input, so the runtime's keyword pre-check
	// and the evaluator normalize and scan a large request only once.
	memo *inputMemo
}

type inputMemo struct {
	mu sync.Mutex
	// texts holds the normalized keyword text, indexed by whether Traditional
	// Chinese was folded into Simplified.
	texts [2]*keywordText
	// lastSet and lastHit remember the most recent keyword scan. One request
	// is checked against one profile, so a single entry is enough.
	lastSet *keywordSet
	lastHit keywordHit
}

// NewInput wraps classified segments for evaluation.
func NewInput(segments []Segment) Input {
	return Input{Segments: segments, memo: &inputMemo{}}
}

// TextInput treats a single piece of text as a user turn. The management test
// endpoint uses it for the text an operator types in.
func TextInput(text string) Input {
	var x extraction
	x.add(ClassPrimary, true, text)
	return NewInput(x.segments)
}

// Empty reports whether the request carried no moderatable text at all.
func (in Input) Empty() bool { return len(in.Segments) == 0 }

// sentToBackend reports whether a segment of this class goes to the moderation
// backend: primary text, plus tool output when the profile opts in. Assistant
// history never does (see ClassAssistantHistory).
func sentToBackend(class Class, includeToolOutput bool) bool {
	return class == ClassPrimary || (class == ClassToolOutput && includeToolOutput)
}

// backendSegments returns the fragments the moderation backend may see, with
// default-ignorable characters removed: they carry no meaning, but a zero-width
// space inside a word can split it into tokens a classifier does not recognize.
func (in Input) backendSegments(includeToolOutput bool) []Segment {
	out := make([]Segment, 0, len(in.Segments))
	for _, segment := range in.Segments {
		if !sentToBackend(segment.Class, includeToolOutput) {
			continue
		}
		if stripped := stripIgnorable(segment.Text); stripped != segment.Text {
			if segment.Text = collapseSpaces(stripped); segment.Text == "" {
				continue
			}
		}
		out = append(out, segment)
	}
	return out
}

// hasBackendText reports whether backendSegments would return anything,
// without copying the text.
func (in Input) hasBackendText(includeToolOutput bool) bool {
	for _, segment := range in.Segments {
		if !sentToBackend(segment.Class, includeToolOutput) {
			continue
		}
		for _, r := range segment.Text {
			if !isIgnorable(r) && !unicode.IsSpace(r) {
				return true
			}
		}
	}
	return false
}

// digest identifies the input for the per-request decision cache. It covers
// every field, because the same text split into other classes or turns can be
// moderated differently.
func (in Input) digest() [sha256.Size]byte {
	hash := sha256.New()
	var header [1 + 1 + 2*binary.MaxVarintLen64]byte
	for _, segment := range in.Segments {
		header[0] = byte(segment.Class)
		header[1] = 0
		if segment.User {
			header[1] = 1
		}
		n := 2 + binary.PutVarint(header[2:], int64(segment.Turn))
		n += binary.PutUvarint(header[n:], uint64(len(segment.Text)))
		_, _ = hash.Write(header[:n])
		_, _ = hash.Write([]byte(segment.Text))
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}
