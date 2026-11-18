package vision

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestResolveSessionKey_FromStickyKey(t *testing.T) {
	// ExecutionSessionMetadataKey is websocket-only, so header-based clients
	// (DeepSeek Harness, Grok, …) reach image memory only through the sticky key.
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SessionStickyMetadataKey: "header:x-deepseek-harness-session-id:ses_abc",
		},
	}
	key, ok := ResolveSessionKey(opts, nil)
	if !ok {
		t.Fatal("expected a resolved session key")
	}
	if want := SessionKey("header:x-deepseek-harness-session-id:ses_abc"); key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
}

func TestResolveSessionKey_StickyKeysStayDistinct(t *testing.T) {
	// Distinct conversations on one API key must not share an image registry.
	auth := &cliproxyauth.Auth{ID: "auth-shared"}
	first, okFirst := ResolveSessionKey(cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SessionStickyMetadataKey: "header:x-deepseek-harness-session-id:ses_first",
		},
	}, auth)
	second, okSecond := ResolveSessionKey(cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SessionStickyMetadataKey: "header:x-deepseek-harness-session-id:ses_second",
		},
	}, auth)
	if !okFirst || !okSecond {
		t.Fatalf("expected both keys to resolve, got %v and %v", okFirst, okSecond)
	}
	if first == second {
		t.Fatalf("expected distinct session keys, both were %q", first)
	}
}

func TestResolveSessionKey_ExistingSourcesKeepPrecedence(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Session-Id", "sess_header")
	opts := cliproxyexecutor.Options{
		Headers: headers,
		Metadata: map[string]any{
			cliproxyexecutor.SessionStickyMetadataKey: "header:x-deepseek-harness-session-id:ses_abc",
		},
	}
	key, ok := ResolveSessionKey(opts, nil)
	if !ok || key != SessionKey("sess_header") {
		t.Fatalf("key = %q (ok=%v), want sess_header", key, ok)
	}

	opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey] = "exec_session"
	key, ok = ResolveSessionKey(opts, nil)
	if !ok || key != SessionKey("exec_session") {
		t.Fatalf("key = %q (ok=%v), want exec_session", key, ok)
	}
}

func TestResolveSessionKey_NeverFallsBackToAuthID(t *testing.T) {
	// The existing safety property: without a real session identity there is no
	// cross-turn image memory, because auth.ID would leak images between sessions.
	auth := &cliproxyauth.Auth{ID: "auth-1"}
	if key, ok := ResolveSessionKey(cliproxyexecutor.Options{}, auth); ok || key != "" {
		t.Fatalf("key = %q (ok=%v), want empty and false", key, ok)
	}
	blank := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SessionStickyMetadataKey: "   ",
		},
	}
	if key, ok := ResolveSessionKey(blank, auth); ok || key != "" {
		t.Fatalf("blank sticky key = %q (ok=%v), want empty and false", key, ok)
	}
}
