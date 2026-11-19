package handlers

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	sdkrequestctx "github.com/router-for-me/CLIProxyAPI/v6/sdk/requestctx"
)

// PinnedAuthServesModel reports whether a credential pinned for a long-lived
// session (for example a Responses WebSocket) can serve the model the next
// request asks for. ctx must carry the gin context, so the model resolves to
// providers exactly as it would for the request itself.
//
// When the model cannot be resolved at all, it answers true: keeping the pin
// lets the normal execution path report the real error instead of masking it
// behind a different credential.
func (h *BaseAPIHandler) PinnedAuthServesModel(ctx context.Context, authID, modelName string) bool {
	authID = strings.TrimSpace(authID)
	modelName = strings.TrimSpace(modelName)
	if h == nil || h.AuthManager == nil || authID == "" || modelName == "" {
		return true
	}
	providers, normalizedModel, errMsg := h.getRequestDetails(ctx, modelName)
	if errMsg != nil {
		return true
	}
	return h.AuthManager.AuthServesModel(tenantIDFromExecutionContext(ctx), authID, providers, normalizedModel)
}

// PinnedAuthReleaseReason reports why a long-lived session should give up the
// credential it pinned before serving modelName, or "" to keep it. ctx must
// carry the gin context, as for PinnedAuthServesModel.
//
// The pin goes when the credential cannot serve the model at all
// (PinnedAuthServesModel) and when it is out of service for longer than a
// moment (coreauth.Manager.PinnedAuthHeldBack): an exhausted quota, a rejected
// or disabled credential. Selection then places the session afresh on a
// credential that can serve it, as it places an HTTP request.
func (h *BaseAPIHandler) PinnedAuthReleaseReason(ctx context.Context, authID, modelName string) string {
	authID = strings.TrimSpace(authID)
	modelName = strings.TrimSpace(modelName)
	if h == nil || h.AuthManager == nil || authID == "" || modelName == "" {
		return ""
	}
	providers, normalizedModel, errMsg := h.getRequestDetails(ctx, modelName)
	if errMsg != nil {
		// As in PinnedAuthServesModel: keep the pin and let the request report
		// the real error.
		return ""
	}
	if !h.AuthManager.AuthServesModel(tenantIDFromExecutionContext(ctx), authID, providers, normalizedModel) {
		return "credential cannot serve the requested model"
	}
	if held, reason := h.AuthManager.PinnedAuthHeldBack(authID, normalizedModel); held {
		return reason
	}
	return ""
}

func tenantIDFromExecutionContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value(sdkrequestctx.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil {
		return ""
	}
	tenantValue, exists := ginCtx.Get("tenantID")
	if !exists {
		return ""
	}
	tenantID, _ := tenantValue.(string)
	return strings.TrimSpace(tenantID)
}
