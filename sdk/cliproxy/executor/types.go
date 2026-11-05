package executor

import (
	"net/http"
	"net/url"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// RequestedModelMetadataKey stores the client-requested model name in Options.Metadata.
const RequestedModelMetadataKey = "requested_model"

const (
	// TenantMetadataKey carries the trusted tenant selected during API key authentication.
	TenantMetadataKey = "tenant_id"
	// PinnedAuthMetadataKey locks execution to a specific auth ID.
	PinnedAuthMetadataKey = "pinned_auth_id"
	// AllowedAuthIDsMetadataKey ([]string) restricts selection to an exact set
	// of auth IDs. Unlike allowed-channels, which treats an empty list as "no
	// restriction", this key fails closed: when it is present only the listed
	// auth IDs are eligible, and a present but empty (or unparseable) value
	// leaves nothing eligible. It narrows every other scope (tenant, channels,
	// channel groups, route group, pinned auth) and never widens one.
	AllowedAuthIDsMetadataKey = "allowed_auth_ids"
	// SinglePickMetadataKey forces the auth manager to use exactly one picked auth
	// for the request and return its error directly without internal retry/fallback.
	SinglePickMetadataKey = "single_pick"
	// RouteGroupMetadataKey scopes execution to a channel group resolved from the URL path.
	RouteGroupMetadataKey = "route_group"
	// RouteFallbackMetadataKey controls whether groupURL requests may fall back to the default scope.
	RouteFallbackMetadataKey = "route_fallback"
	// TenantWideSelectionMetadataKey (bool) lets selection consider every
	// credential of the tenant. Without it a request that names no channel group
	// is placed in the implicit "default" group, and that group's membership and
	// model lists apply. That is the right policy for API traffic, but it made
	// the panel's model and image probes fail for models an operator kept out of
	// the default group. A route group, allowed channels or a pinned credential
	// set on the same request still apply.
	TenantWideSelectionMetadataKey = "tenant_wide_selection"
	// SelectedAuthMetadataKey stores the auth ID selected by the scheduler.
	SelectedAuthMetadataKey = "selected_auth_id"
	// SelectedAuthCallbackMetadataKey carries an optional callback invoked with the selected auth ID.
	SelectedAuthCallbackMetadataKey = "selected_auth_callback"
	// ExecutionSessionMetadataKey identifies a long-lived downstream execution session.
	ExecutionSessionMetadataKey = "execution_session_id"
	// SessionStickyMetadataKey carries a stable client session key for sticky routing.
	SessionStickyMetadataKey = "session_sticky_key"
)

// Request encapsulates the translated payload that will be sent to a provider executor.
type Request struct {
	// Model is the upstream model identifier after translation.
	Model string
	// Payload is the provider specific JSON payload.
	Payload []byte
	// Format represents the provider payload schema.
	Format sdktranslator.Format
	// Metadata carries optional provider specific execution hints.
	Metadata map[string]any
}

// Options controls execution behavior for both streaming and non-streaming calls.
type Options struct {
	// Stream toggles streaming mode.
	Stream bool
	// Alt carries optional alternate format hint (e.g. SSE JSON key).
	Alt string
	// Headers are forwarded to the provider request builder.
	Headers http.Header
	// Query contains optional query string parameters.
	Query url.Values
	// OriginalRequest preserves the inbound request bytes prior to translation.
	OriginalRequest []byte
	// SourceFormat identifies the inbound schema.
	SourceFormat sdktranslator.Format
	// Metadata carries extra execution hints shared across selection and executors.
	Metadata map[string]any
}

// Response wraps either a full provider response or metadata for streaming flows.
type Response struct {
	// Payload is the provider response in the executor format.
	Payload []byte
	// Metadata exposes optional structured data for translators.
	Metadata map[string]any
	// Headers carries upstream HTTP response headers for passthrough to clients.
	Headers http.Header
}

// StreamChunk represents a single streaming payload unit emitted by provider executors.
type StreamChunk struct {
	// Payload is the raw provider chunk payload.
	Payload []byte
	// Err reports any terminal error encountered while producing chunks.
	Err error
}

// StreamResult wraps the streaming response, providing both the chunk channel
// and the upstream HTTP response headers captured before streaming begins.
type StreamResult struct {
	// Headers carries upstream HTTP response headers from the initial connection.
	Headers http.Header
	// Chunks is the channel of streaming payload units.
	Chunks <-chan StreamChunk
	// AuthID names the credential serving this stream. The auth manager fills it
	// in; executors leave it empty. A caller that abandons the stream before the
	// first byte uses it to keep the retry away from the same account.
	AuthID string
	// FailedAuthIDs names the credentials the same scheduling call tried and
	// gave up on before AuthID took the stream. The auth manager fills it in;
	// executors leave it empty. A caller that abandons the stream before the
	// first byte keeps its retry away from these as well: a failure that left no
	// cooldown behind (cooling switched off, a 400-class answer, a back-off that
	// has already run out) would otherwise let the retry walk back to an account
	// that failed this very request a moment earlier.
	FailedAuthIDs []string
}

// StatusError represents an error that carries an HTTP-like status code.
// Provider executors should implement this when possible to enable
// better auth state updates on failures (e.g., 401/402/429).
type StatusError interface {
	error
	StatusCode() int
}
