package vision

import (
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResolveSessionKey extracts a session-level identifier from executor options.
// Returns false when no real session key is available — in that case the caller
// MUST NOT perform cross-turn image memory (single-request only).
//
// Priority:
//  1. opts.Metadata[ExecutionSessionMetadataKey]
//  2. Session-Id header
//  3. opts.Metadata[SessionStickyMetadataKey]
//  4. Return (empty, false) — never fallback to auth.ID
func ResolveSessionKey(opts cliproxyexecutor.Options, auth *cliproxyauth.Auth) (SessionKey, bool) {
	// 1. Metadata first — most reliable when available
	if raw, ok := opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey]; ok {
		if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
			return SessionKey(strings.TrimSpace(s)), true
		}
	}

	// 2. Session-Id header
	if opts.Headers != nil {
		if sessionID := strings.TrimSpace(opts.Headers.Get("Session-Id")); sessionID != "" {
			return SessionKey(sessionID), true
		}
	}

	// 3. Sticky key — set only when the request carried a client-specific session
	// header (DeepSeek Harness, Grok, Codex, …). ExecutionSessionMetadataKey is
	// websocket-only, so without this those clients get no cross-turn image
	// memory at all. Safe by construction: the sticky key is per conversation,
	// not per API key, so it cannot leak images between sessions the way an
	// auth.ID fallback would.
	if raw, ok := opts.Metadata[cliproxyexecutor.SessionStickyMetadataKey]; ok {
		if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
			return SessionKey(strings.TrimSpace(s)), true
		}
	}

	// 4. Never fallback to auth.ID for image memory — would leak across sessions
	return "", false
}
