package executor

import (
	"context"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	log "github.com/sirupsen/logrus"
)

// guardOutcome folds loop protection into the outcome of the request whose usage
// is about to be published.
//
// A reply the stream handler cut is ended by cancelling the upstream request, to
// stop paying for it, and the executor sees a cancellation: which it files as
// the client having left, or not at all. The handler marks the context before it
// cancels, so the reporter finds the real reason here and records the request as
// what it was, a failure of its own kind. Whatever the upstream had reported by
// then is kept.
func (r *usageReporter) guardOutcome(ctx context.Context, failed bool) (bool, string) {
	kind := r.guardKind
	if kind == "" {
		if cut, ok := loopdetect.MarkFrom(ctx).Cut(); ok {
			kind = string(cut.Kind)
		}
	}
	return failed || kind != "", kind
}

// LoopGuardObserver records what loop protection did to a request. The stream
// handlers call it; it sits here because it files the request the way every other
// request is filed, through a usage reporter.
type LoopGuardObserver struct {
	// ReplySource and ToolSource say which level of settings (tenant or API
	// key) switched each check on, for the diagnostics.
	ReplySource string
	ToolSource  string
}

var _ loopdetect.Observer = LoopGuardObserver{}

// ReplyCut writes the cut into the request's diagnostics. The request's usage
// record is the executor's own, tagged through the context's mark.
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
	log.WithFields(log.Fields{"model": cut.Model, "limits_source": o.ReplySource, "cut_at_chunk": trip.Chunks}).
		Info("loop guard: ended a reply that had become a loop")
}

// ToolLoopAnswered writes the finding into the diagnostics and files the request:
// it is answered by the gateway and goes upstream nowhere, so it has no account,
// no channel and no tokens, and costs nothing. It is a failure of its own kind so
// the log, the monitor and the filters can tell it from an upstream failing.
func (o LoopGuardObserver) ToolLoopAnswered(ctx context.Context, answered loopdetect.ToolAnswered) {
	loop := answered.Loop
	calls := make([]diagnostics.LoopGuardCall, len(loop.Calls))
	for i, call := range loop.Calls {
		calls[i] = diagnostics.LoopGuardCall{Name: call.Name, Arguments: call.Arguments}
	}
	notice := loop.Notice()
	diagnostics.SetLoopGuard(ctx, diagnostics.LoopGuardSnapshot{
		Kind:              diagnostics.LoopGuardToolLoop,
		Source:            o.ToolSource,
		Repeats:           answered.Limits.Repeats,
		Turns:             loop.Turns,
		Calls:             calls,
		ConversationItems: loop.Items,
		ItemsRead:         loop.Read,
		Message:           notice,
	})

	reporter := newUsageReporter(ctx, "gateway", answered.Model, "", nil, answered.Stream)
	// With no account the reporter would name the caller's own API key as the
	// source of the request; nothing served this one.
	reporter.source = ""
	reporter.guardKind = string(loopdetect.KindToolLoop)
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": notice,
		"type":    "loop_guard",
		"code":    "tool_loop_answered",
	}})
	if err != nil {
		body = []byte(notice)
	}
	reporter.publishFailureWithContent(ctx, "", string(body))
	log.WithFields(log.Fields{"model": answered.Model, "limits_source": o.ToolSource, "turns": loop.Turns}).
		Info("loop guard: answered a tool-call loop without sending it upstream")
}
