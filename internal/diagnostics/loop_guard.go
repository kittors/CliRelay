package diagnostics

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// Kinds of LoopGuardSnapshot. They are the values request_logs.guard_kind holds
// and the panel reads; never rename one.
const (
	LoopGuardReplyLoop = "reply_loop"
	LoopGuardToolLoop  = "tool_loop"
)

// LoopGuardSnapshot records what loop protection did to a request: which check
// ended it, the thresholds it ran with, which level of settings switched it on,
// and where the cut was made. Without it a cut reply or an answered request
// looks, in the log, like the model or the upstream failing.
type LoopGuardSnapshot struct {
	// Kind is LoopGuardReplyLoop or LoopGuardToolLoop.
	Kind string `json:"kind"`
	// Source is the level that switched the check on: "tenant" or "api_key".
	Source string `json:"source,omitempty"`

	// Window and MaxDistinct are the thresholds of the repeated-reply check, and
	// Repeats that of the tool-loop check.
	Window      int `json:"window,omitempty"`
	MaxDistinct int `json:"max_distinct,omitempty"`
	Repeats     int `json:"repeats,omitempty"`

	// Distinct is how many different units the window held when the reply was
	// cut, and Units the ones that repeated, most frequent first, each cut
	// short. Reasoning says the reasoning looped and not the answer, RunOn that
	// the loop had no line or sentence break.
	Distinct  int      `json:"distinct,omitempty"`
	Units     []string `json:"units,omitempty"`
	Reasoning bool     `json:"reasoning,omitempty"`
	RunOn     bool     `json:"run_on,omitempty"`
	// CutAtChunk and CutAtBytes say where in the reply the cut was made: the
	// chunk that tripped the check was the CutAtChunk-th the upstream sent,
	// after CutAtBytes bytes of it, and it was not forwarded to the client.
	// UnitsRead is how many units the looping channel had counted.
	UnitsRead  int   `json:"units_read,omitempty"`
	CutAtChunk int   `json:"cut_at_chunk,omitempty"`
	CutAtBytes int64 `json:"cut_at_bytes,omitempty"`

	// Turns is how many turns in a row ended the conversation with the same
	// calls and results, Calls the calls of the repeated turn, and
	// ConversationItems and ItemsRead how many items the conversation held and
	// how many of its newest the check looked at.
	Turns             int             `json:"turns,omitempty"`
	Calls             []LoopGuardCall `json:"calls,omitempty"`
	ConversationItems int             `json:"conversation_items,omitempty"`
	ItemsRead         int             `json:"items_read,omitempty"`

	// Message is what the client was told.
	Message string `json:"message,omitempty"`
}

// LoopGuardCall is one call of a repeated turn.
type LoopGuardCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// SetLoopGuard records what loop protection did to the request ctx belongs to.
// ctx is the request's own or the handler's, which carries the gin context.
func SetLoopGuard(ctx context.Context, snapshot LoopGuardSnapshot) {
	d := FromContext(ctx)
	if d == nil && ctx != nil {
		if ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context); ok {
			d = FromGin(ginCtx)
		}
	}
	if d == nil {
		return
	}
	snapshot.Units = append([]string(nil), snapshot.Units...)
	snapshot.Calls = append([]LoopGuardCall(nil), snapshot.Calls...)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.LoopGuard = &snapshot
}

func cloneLoopGuard(in *LoopGuardSnapshot) *LoopGuardSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.Units = append([]string(nil), in.Units...)
	out.Calls = append([]LoopGuardCall(nil), in.Calls...)
	return &out
}
