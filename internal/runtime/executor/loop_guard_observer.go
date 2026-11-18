package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/loopdetect"
	log "github.com/sirupsen/logrus"
)

// LoopGuardObserver records loop protection outcomes in request diagnostics.
// Usage accounting is handled by the v8 execution pipeline through the mark
// carried on the request context.
type LoopGuardObserver struct {
	ReplySource string
	ToolSource  string
}

var _ loopdetect.Observer = LoopGuardObserver{}

func (o LoopGuardObserver) ReplyCut(ctx context.Context, cut loopdetect.ReplyCut) {
	trip := cut.Trip
	diagnostics.SetLoopGuard(ctx, diagnostics.LoopGuardSnapshot{
		Kind:        diagnostics.LoopGuardReplyLoop,
		Source:      o.ReplySource,
		Window:      cut.Limits.Window,
		MaxDistinct: cut.Limits.Distinct,
		Distinct:    trip.Distinct,
		Units:       trip.Units,
		Reasoning:   trip.Reasoning,
		RunOn:       trip.RunOn,
		UnitsRead:   trip.UnitsRead,
		CutAtChunk:  trip.Chunks,
		CutAtBytes:  trip.Bytes,
		Message:     trip.Message(),
	})
	log.WithFields(log.Fields{
		"model": cut.Model, "limits_source": o.ReplySource, "cut_at_chunk": trip.Chunks,
	}).Info("loop guard: ended a reply that had become a loop")
}

func (o LoopGuardObserver) ToolLoopAnswered(ctx context.Context, answered loopdetect.ToolAnswered) {
	loop := answered.Loop
	calls := make([]diagnostics.LoopGuardCall, len(loop.Calls))
	for i, call := range loop.Calls {
		calls[i] = diagnostics.LoopGuardCall{Name: call.Name, Arguments: call.Arguments}
	}
	diagnostics.SetLoopGuard(ctx, diagnostics.LoopGuardSnapshot{
		Kind:              diagnostics.LoopGuardToolLoop,
		Source:            o.ToolSource,
		Repeats:           answered.Limits.Repeats,
		Turns:             loop.Turns,
		Calls:             calls,
		ConversationItems: loop.Items,
		ItemsRead:         loop.Read,
		Message:           loop.Notice(),
	})
	log.WithFields(log.Fields{
		"model": answered.Model, "limits_source": o.ToolSource, "turns": loop.Turns,
	}).Info("loop guard: answered a tool-call loop without sending it upstream")
}
