package handlers

import (
	"context"
	"net/http"
)

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
