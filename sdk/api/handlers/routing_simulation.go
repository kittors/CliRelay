package handlers

import (
	"context"
	"fmt"
	"net/http"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// SimulateRouting reports where ExecuteWithAuthManager would send a request for
// modelName, without sending it: the providers and model it resolves, then the
// auth manager's simulated selection. ctx must carry the request's gin context,
// as the one ExecuteWithAuthManager receives does. It changes no state.
//
// The error is the one ExecuteWithAuthManager would return before selection,
// so WriteErrorResponse renders it the way a real request sees it.
func (h *BaseAPIHandler) SimulateRouting(ctx context.Context, modelName string) (*coreauth.SelectionSimulation, *StreamErrorMessage) {
	providers, normalizedModel, errMsg := h.getRequestDetails(ctx, modelName)
	if errMsg != nil {
		return nil, errMsg
	}
	if h.AuthManager == nil {
		return nil, &StreamErrorMessage{StatusCode: http.StatusInternalServerError, Error: fmt.Errorf("auth manager unavailable")}
	}
	reqMeta := requestExecutionMetadata(ctx)
	reqMeta[coreexecutor.RequestedModelMetadataKey] = normalizedModel
	return h.AuthManager.SimulateSelection(ctx, providers, normalizedModel, reqMeta), nil
}
