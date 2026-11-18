package handlers

import (
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/net/context"
)

// SimulateRouting resolves providers and asks the conductor for its dry-run
// credential selection. It performs no upstream request.
func (h *BaseAPIHandler) SimulateRouting(ctx context.Context, model string) (*coreauth.SelectionSimulation, *interfaces.ErrorMessage) {
	if h == nil || h.AuthManager == nil {
		return nil, &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: fmt.Errorf("auth manager unavailable")}
	}
	providers, normalized, errMsg := h.getRequestDetails(model)
	if errMsg != nil {
		return nil, errMsg
	}
	simulation := h.AuthManager.SimulateSelection(ctx, providers, normalized, requestExecutionMetadata(ctx))
	return simulation, nil
}
