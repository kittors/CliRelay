package executor

import "context"

// codexBodyEdits tallies what this gateway changed in a request body on its way
// to the Codex upstream: the rewrites that make the body differ from the one the
// client sent. Each is one boolean or integer written as the rewrite happens,
// because the only reader is the shape recorded after the upstream refused a
// request without saying why (codex_request_shape.go), and by then the unedited
// body is gone.
//
// One value belongs to one upstream attempt and the edit sites run in sequence
// on that attempt's goroutine, so it takes no lock. Every method accepts a nil
// receiver: callers with no attempt to attribute the edits to (the websocket
// transport, the xAI executor, tests) pass nothing and the tally is dropped.
type codexBodyEdits struct {
	// The image bridge (codex_image_generation_bridge.go): it attached the hosted
	// image_generation tool, pinned tool_choice to it, and removed
	// reasoning.effort from a request that is going to call it.
	ImageBridgeTool  bool `json:"image_bridge_tool,omitempty"`
	ToolChoiceForced bool `json:"tool_choice_forced,omitempty"`
	EffortDropped    bool `json:"effort_dropped,omitempty"`

	// History hygiene (codex_image_history.go). IDsStripped counts the items that
	// stayed and lost their server-issued id, ShellsDropped the items that went
	// whole: reference-only shells, and image_generation_call items left with no
	// result (which includes those whose pixels ImageResultsDropped counts).
	// DataURLsReplaced counts data:image URLs in history text swapped for a
	// placeholder, and ImageReattached says the last such image was put back as
	// an input_image on the latest user turn.
	IDsStripped         int  `json:"ids_stripped,omitempty"`
	ShellsDropped       int  `json:"shells_dropped,omitempty"`
	DataURLsReplaced    int  `json:"data_urls_replaced,omitempty"`
	ImageResultsDropped int  `json:"image_results_dropped,omitempty"`
	ImageReattached     bool `json:"image_reattached,omitempty"`

	// ForeignReasoningDropped counts reasoning items removed before sending for
	// carrying another vendor's ciphertext (prepareCodexResponsesBody).
	ForeignReasoningDropped int `json:"foreign_reasoning_dropped,omitempty"`

	// ReasoningRetry marks a body sent a second time without any reasoning
	// ciphertext after the upstream refused the first (codex_responses_send.go).
	// The sender sets it on the copy it reports, not here.
	ReasoningRetry bool `json:"reasoning_retry,omitempty"`
}

func (e *codexBodyEdits) noteImageBridgeTool() {
	if e != nil {
		e.ImageBridgeTool = true
	}
}

func (e *codexBodyEdits) noteToolChoiceForced() {
	if e != nil {
		e.ToolChoiceForced = true
	}
}

func (e *codexBodyEdits) noteEffortDropped() {
	if e != nil {
		e.EffortDropped = true
	}
}

// noteHistory adds the outcome of one history hygiene pass. The pass runs twice
// per request, on the client payload and on the translated body, so the counts
// add up.
func (e *codexBodyEdits) noteHistory(idsStripped, shellsDropped, dataURLsReplaced, imageResultsDropped int, reattached bool) {
	if e == nil {
		return
	}
	e.IDsStripped += idsStripped
	e.ShellsDropped += shellsDropped
	e.DataURLsReplaced += dataURLsReplaced
	e.ImageResultsDropped += imageResultsDropped
	e.ImageReattached = e.ImageReattached || reattached
}

func (e *codexBodyEdits) noteForeignReasoning(dropped int) {
	if e != nil {
		e.ForeignReasoningDropped += dropped
	}
}

// firstCodexEdits picks the tally out of an optional trailing argument, so the
// body rewriters keep their signatures for the callers that have none to give.
func firstCodexEdits(edits []*codexBodyEdits) *codexBodyEdits {
	if len(edits) == 0 {
		return nil
	}
	return edits[0]
}

type codexBodyEditsKey struct{}

// withCodexBodyEdits starts the tally of one upstream attempt and returns a
// context carrying it, so the sender, which sees only the context, can report it
// next to the body it built.
func withCodexBodyEdits(ctx context.Context) (context.Context, *codexBodyEdits) {
	if ctx == nil {
		ctx = context.Background()
	}
	edits := &codexBodyEdits{}
	return context.WithValue(ctx, codexBodyEditsKey{}, edits), edits
}

// codexBodyEditsFromContext returns the tally ctx carries, or nil.
func codexBodyEditsFromContext(ctx context.Context) *codexBodyEdits {
	if ctx == nil {
		return nil
	}
	edits, _ := ctx.Value(codexBodyEditsKey{}).(*codexBodyEdits)
	return edits
}
