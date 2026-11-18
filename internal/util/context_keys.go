package util

import sdkrequestctx "github.com/router-for-me/CLIProxyAPI/v8/sdk/requestctx"

// ContextKey preserves the historical internal alias while delegating the
// public request-context contract to the SDK layer.
type ContextKey = sdkrequestctx.ContextKey

const (
	ContextKeyAlt                      = sdkrequestctx.ContextKeyAlt
	ContextKeyGin                      = sdkrequestctx.ContextKeyGin
	ContextKeyRoundTripper             = sdkrequestctx.ContextKeyRoundTripper
	ContextKeyAPIKey                   = sdkrequestctx.ContextKeyAPIKey
	ContextKeyTrustedTenantID          = sdkrequestctx.ContextKeyTrustedTenantID
	ContextKeyImageGenerationPhaseHook = sdkrequestctx.ContextKeyImageGenerationPhaseHook
)

const GinKeyFirstResponseAt = sdkrequestctx.GinKeyFirstResponseAt
