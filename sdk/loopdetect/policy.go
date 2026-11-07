package loopdetect

import (
	"context"
	"sync/atomic"
)

// Kind names the check that ended a request. The values are stored in the
// request log and read by the management panel: never rename one.
type Kind string

const (
	// KindReplyLoop: a streamed reply was ended because its reasoning or text
	// had become the same few lines over and over.
	KindReplyLoop Kind = "reply_loop"
	// KindToolLoop: a request was answered by the gateway itself, because its
	// conversation ended in one tool call answered with one result again and
	// again, and was not sent upstream.
	KindToolLoop Kind = "tool_loop"
)

// Valid says whether k is a kind this package knows.
func (k Kind) Valid() bool { return k == KindReplyLoop || k == KindToolLoop }

// Policy is what loop protection does to one request, resolved by the gateway
// from the settings of the tenant and of the API key before the handler runs.
// A nil check is off; a nil *Policy is both off.
type Policy struct {
	Reply *ReplyLimits
	Tool  *ToolLimits
	// Observer is told of every interception. It may be nil.
	Observer Observer
}

// ReplyOn says whether streamed replies are checked for loops.
func (p *Policy) ReplyOn() bool { return p != nil && p.Reply != nil }

// ToolOn says whether requests are checked for a tool-call loop.
func (p *Policy) ToolOn() bool { return p != nil && p.Tool != nil }

// ReplyCut describes a reply the handler ended.
type ReplyCut struct {
	Limits ReplyLimits
	Trip   ReplyTrip
	Model  string
}

// ToolAnswered describes a request the handler answered itself.
type ToolAnswered struct {
	Limits   ToolLimits
	Loop     ToolLoop
	Model    string
	Protocol Protocol
	Stream   bool
}

// Observer is told what the checks did, for the gateway to record it. The
// handlers call it from the goroutine that serves the request and do not wait
// on anything it does beyond its own return, so an implementation must not block.
type Observer interface {
	// ReplyCut is called once, before the upstream request is abandoned, when
	// a streamed reply was ended. ctx is the request's, carrying the gin
	// context.
	ReplyCut(ctx context.Context, cut ReplyCut)
	// ToolLoopAnswered is called once, before the answer is written, when a
	// request was answered here and sent upstream nowhere.
	ToolLoopAnswered(ctx context.Context, answered ToolAnswered)
}

// Mark records, on the context of one upstream request, that the gateway ended
// it on purpose. The usage reporter of the executor that served the request
// reads it when the request ends: the handler cancels the upstream request to
// stop paying for the reply, and a canceled request is otherwise recorded as
// the client having left, not as a reply that was cut.
type Mark struct {
	cut atomic.Pointer[MarkedCut]
}

// MarkedCut is what a Mark holds.
type MarkedCut struct {
	Kind Kind
	// Detail is the same text the client was told.
	Detail string
}

type markKey struct{}

// WithMark returns ctx carrying a new, unset Mark.
func WithMark(ctx context.Context) (context.Context, *Mark) {
	m := &Mark{}
	return context.WithValue(ctx, markKey{}, m), m
}

// MarkFrom returns the Mark in ctx, or nil.
func MarkFrom(ctx context.Context) *Mark {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(markKey{}).(*Mark)
	return m
}

// Set records the cut. The first one stands.
func (m *Mark) Set(kind Kind, detail string) {
	if m == nil {
		return
	}
	m.cut.CompareAndSwap(nil, &MarkedCut{Kind: kind, Detail: detail})
}

// Cut returns what was recorded, if anything.
func (m *Mark) Cut() (MarkedCut, bool) {
	if m == nil {
		return MarkedCut{}, false
	}
	if c := m.cut.Load(); c != nil {
		return *c, true
	}
	return MarkedCut{}, false
}
