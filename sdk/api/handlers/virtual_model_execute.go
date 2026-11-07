package handlers

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
)

// ExecuteWithAuthManager executes a non-streaming request via the core auth
// manager. A model that names a virtual model runs on its members in turn.
func (h *BaseAPIHandler) ExecuteWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *StreamErrorMessage) {
	// A conversation stuck in a tool-call loop is answered here, not sent
	// upstream: for a virtual model once, whichever member would have run it.
	if reply, ok := toolLoopReply(ctx, handlerType, modelName, rawJSON, alt, false); ok {
		return reply.JSON(), loopGuardHeaders(loopdetect.KindToolLoop), nil
	}
	plan := virtualModelPlanFor(ctx, modelName)
	if plan == nil {
		return h.executeModel(ctx, handlerType, modelName, rawJSON, alt)
	}
	return h.executeVirtualModel(ctx, plan, func(ctx context.Context, model string) ([]byte, http.Header, *StreamErrorMessage) {
		return h.executeModel(ctx, handlerType, model, rawJSON, alt)
	})
}

// ExecuteCountWithAuthManager executes a token count via the core auth manager.
// A model that names a virtual model is counted by the first member that can.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *StreamErrorMessage) {
	plan := virtualModelPlanFor(ctx, modelName)
	if plan == nil {
		return h.executeCountModel(ctx, handlerType, modelName, rawJSON, alt)
	}
	return h.executeVirtualModel(ctx, plan, func(ctx context.Context, model string) ([]byte, http.Header, *StreamErrorMessage) {
		return h.executeCountModel(ctx, handlerType, model, rawJSON, alt)
	})
}

// executeVirtualModel runs a non-streaming request on the members of plan in
// turn, with execute running it on one member's model.
func (h *BaseAPIHandler) executeVirtualModel(
	ctx context.Context,
	plan *VirtualModelPlan,
	execute func(ctx context.Context, model string) ([]byte, http.Header, *StreamErrorMessage),
) ([]byte, http.Header, *StreamErrorMessage) {
	run := h.newVirtualModelRun(ctx, plan, false)
	for {
		try, ok := run.nextTry()
		if !ok {
			break
		}
		resp, headers, errMsg := execute(try.ctx, try.member.Model)
		if errMsg == nil {
			return resp, mergeVirtualModelHeaders(headers, run.served(try)), nil
		}
		if !run.failed(try, errMsg) {
			break
		}
	}
	return nil, nil, run.finish()
}

// startVirtualModelStream starts a stream on the members of plan in turn: the
// members tried until one hands over a stream, and then, from the forwarding
// goroutine, the ones after it while its stream fails before its first byte.
//
// The response headers name the member the stream started on. A member that
// takes over before the first byte cannot change them: the protocol handler
// sends them as soon as this returns. The route trace records the change.
func (h *BaseAPIHandler) startVirtualModelStream(ctx context.Context, waits *streamStartWaits, guard *replyGuard, plan *VirtualModelPlan, handlerType string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *StreamErrorMessage, *StreamErrorMessage) {
	run := h.newVirtualModelRun(ctx, plan, true)
	begin := func() (*startedStream, virtualModelTry, *StreamErrorMessage) {
		for {
			try, ok := run.nextTry()
			if !ok {
				return nil, try, run.finish()
			}
			stream, errMsg := h.beginStream(try.ctx, handlerType, try.member.Model, rawJSON, alt)
			if errMsg == nil {
				return stream, try, nil
			}
			if !run.failed(try, errMsg) {
				return nil, try, run.finish()
			}
		}
	}
	stream, current, errMsg := begin()
	if committed := waits.handOver(); errMsg != nil {
		if committed {
			return failedAfterCommit(errMsg)
		}
		return nil, nil, nil, errMsg
	}
	writeVirtualModelHeaders(ginContextFrom(ctx), run.served(current))
	next := func(cause error) (*startedStream, *StreamErrorMessage) {
		if !run.failed(current, streamErrorMessageFromErr(cause)) {
			return nil, run.finish()
		}
		stream, try, errMsg := begin()
		if errMsg != nil {
			return nil, errMsg
		}
		current = try
		run.served(try)
		return stream, nil
	}
	dataChan, upstreamHeaders, errChan := h.forwardStream(handlerType, stream, guard, next)
	return dataChan, upstreamHeaders, errChan, nil
}
