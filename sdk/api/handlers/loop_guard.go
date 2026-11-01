package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	log "github.com/sirupsen/logrus"
)

// LoopGuardGate resolves the loop protection of the request it was published
// for: which checks run and with what thresholds, from the settings of the
// tenant and of the API key. It is a function, not a value, so a long-lived
// request (a Responses WebSocket serves turns for hours) applies a setting
// changed while it is open from its next turn on. It returns nil when nothing is
// on.
type LoopGuardGate func() *loopdetect.Policy

// LoopGuardGateContextKey is the gin context key the loop-guard middleware
// stores a LoopGuardGate under.
const LoopGuardGateContextKey = "cliproxy.loopGuardGate"

// LoopGuardGateFromGin returns the gate published for this request, or nil when
// the middleware did not run (a route without an API key, such as the Gemini
// CLI bridge, is never protected).
func LoopGuardGateFromGin(c *gin.Context) LoopGuardGate {
	if c == nil {
		return nil
	}
	value, ok := c.Get(LoopGuardGateContextKey)
	if !ok {
		return nil
	}
	gate, _ := value.(LoopGuardGate)
	return gate
}

// loopGuardPolicy returns the policy in force for the request ctx belongs to.
func loopGuardPolicy(ctx context.Context) *loopdetect.Policy {
	gate := LoopGuardGateFromGin(ginContextFrom(ctx))
	if gate == nil {
		return nil
	}
	return gate()
}

// LoopGuardHeader tells a client, and whoever reads its logs, that the gateway
// answered the request itself because of a loop.
const LoopGuardHeader = "X-CliRelay-Loop-Guard"

func loopGuardHeaders(kind loopdetect.Kind) http.Header {
	return http.Header{LoopGuardHeader: {string(kind)}}
}

// toolLoopReply answers a request whose conversation ends in a tool-call loop:
// the same call answered with the same result turn after turn. It returns the
// reply the gateway writes in the API the client speaks, and true, when the
// request is not to be sent upstream. It reads the newest items of the
// conversation only.
//
// An alt with a path in it names another endpoint of the API (a Responses
// compaction, an image edit) whose reply is not a conversation turn; Gemini's
// own alt values (json, sse) carry no path.
func toolLoopReply(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, stream bool) (loopdetect.LocalReply, bool) {
	if strings.Contains(alt, "/") || len(rawJSON) == 0 {
		return loopdetect.LocalReply{}, false
	}
	policy := loopGuardPolicy(ctx)
	if !policy.ToolOn() {
		return loopdetect.LocalReply{}, false
	}
	protocol, ok := loopdetect.ProtocolOf(handlerType)
	if !ok {
		return loopdetect.LocalReply{}, false
	}
	loop, ok := detectToolLoop(protocol, rawJSON, *policy.Tool)
	if !ok {
		return loopdetect.LocalReply{}, false
	}
	reply := loopdetect.LocalReply{
		Protocol: protocol,
		Model:    modelName,
		Text:     loop.Notice(),
		ID:       strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
		Created:  time.Now().Unix(),
	}
	if policy.Observer != nil {
		policy.Observer.ToolLoopAnswered(ctx, loopdetect.ToolAnswered{
			Limits: policy.Tool.Normalized(), Loop: loop, Model: modelName, Protocol: protocol, Stream: stream,
		})
	}
	return reply, true
}

// toolLoopCheck is the tool-loop check. It is a variable only so a test can make
// it fail (see export_test.go); nothing else replaces it.
var toolLoopCheck = loopdetect.DetectToolLoop

// detectToolLoop runs the check, which reads client-supplied JSON: a defect in
// it must cost the request its protection, never the request.
func detectToolLoop(protocol loopdetect.Protocol, body []byte, limits loopdetect.ToolLimits) (loop loopdetect.ToolLoop, found bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("loop guard: tool-loop check panicked and was skipped: %v", r)
			loop, found = loopdetect.ToolLoop{}, false
		}
	}()
	return toolLoopCheck(protocol, body, limits)
}

// localStream hands out the chunks of a reply the gateway wrote itself as an
// upstream's stream would be: all of them waiting, then closed.
func localStream(chunks [][]byte) (<-chan []byte, <-chan *StreamErrorMessage) {
	data := make(chan []byte, len(chunks))
	for _, chunk := range chunks {
		data <- chunk
	}
	close(data)
	errs := make(chan *StreamErrorMessage)
	close(errs)
	return data, errs
}

// replyGuard watches the chunks a stream is about to send the client for a reply
// that has become a loop. One serves one upstream stream and only its forwarding
// goroutine calls it. A nil *replyGuard is a stream that is not watched.
//
// It sits at the end of the forwarding goroutine, past the point where a
// stream that failed before its first byte moves to another account, so only
// what the client is actually sent is counted.
type replyGuard struct {
	ctx    context.Context
	policy *loopdetect.Policy
	guard  *loopdetect.ReplyGuard
	mark   *loopdetect.Mark
	model  string
	// off: the check failed on this stream and is not run again.
	off bool
}

// newReplyGuard returns the guard for a stream about to start, and ctx carrying
// the Mark the executor's usage reporter reads. It returns ctx and nil when
// replies are not checked.
func newReplyGuard(ctx context.Context, model string) (context.Context, *replyGuard) {
	policy := loopGuardPolicy(ctx)
	if !policy.ReplyOn() {
		return ctx, nil
	}
	ctx, mark := loopdetect.WithMark(ctx)
	return ctx, &replyGuard{
		ctx: ctx, policy: policy, mark: mark, model: model,
		guard: loopdetect.NewReplyGuard(*policy.Reply),
	}
}

// inspect reads the next chunk of the stream. When the reply has become a loop
// it returns the error the stream ends with, after recording the cut where the
// executor's usage reporter and the request log will find it.
func (g *replyGuard) inspect(chunk []byte) *StreamErrorMessage {
	if g == nil || g.off {
		return nil
	}
	trip, ok := g.feed(chunk)
	if !ok {
		return nil
	}
	err := &loopdetect.ReplyLoopError{Trip: trip}
	// Before the stream's end reaches the entry handler, which cancels the
	// upstream request: the executor reports its usage when that cancellation
	// reaches it, and reads the mark then.
	g.mark.Set(loopdetect.KindReplyLoop, trip.Message())
	if g.policy.Observer != nil {
		g.policy.Observer.ReplyCut(g.ctx, loopdetect.ReplyCut{Limits: g.guard.Limits(), Trip: trip, Model: g.model})
	}
	return &StreamErrorMessage{StatusCode: loopdetect.ReplyLoopStatus, Error: err}
}

func (g *replyGuard) feed(chunk []byte) (trip loopdetect.ReplyTrip, found bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("loop guard: reply check panicked and was turned off for this stream: %v", r)
			g.off = true
			trip, found = loopdetect.ReplyTrip{}, false
		}
	}()
	return g.guard.Feed(chunk)
}
