package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// The identifiers below mirror a real Codex Desktop turn captured from the
// production request log: a root turn, so session, thread and x-client-request-id
// all carry one value, the window id appends a compaction counter, and the turn
// metadata repeats the same graph inside a JSON blob.
const (
	clientRootID    = "01a02380-e7de-79a1-a6ae-bcb146d1c0c9"
	clientWindowID  = clientRootID + ":2"
	clientTurnID    = "01a031cb-1b7f-72b1-b7de-6ffa906b0505"
	clientInstallID = "7e296383-a82a-4b29-bb1a-b561986714ef"
)

func codexDeviceModeRequest(t *testing.T) *http.Request {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for key, value := range map[string]string{
		"Session-Id":          clientRootID,
		"Thread-Id":           clientRootID,
		"X-Client-Request-Id": clientRootID,
		"X-Codex-Window-Id":   clientWindowID,
		"X-Codex-Turn-Metadata": `{"installation_id":"` + clientInstallID + `","session_id":"` + clientRootID +
			`","thread_id":"` + clientRootID + `","turn_id":"` + clientTurnID + `","window_id":"` + clientWindowID +
			`","request_kind":"turn","sandbox":"none"}`,
	} {
		ginCtx.Request.Header.Set(key, value)
	}

	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	return req.WithContext(context.WithValue(req.Context(), util.ContextKeyGin, ginCtx))
}

// TestDeviceModeProjectsOneCoherentIdentityGraph pins the invariants that
// production was violating: every outbound identifier is a well-formed UUID,
// the header copy and the turn-metadata copy agree, and the equalities the
// client established survive the account scoping.
func TestDeviceModeProjectsOneCoherentIdentityGraph(t *testing.T) {
	req := codexDeviceModeRequest(t)
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceDevice)
	applyCodexHeaders(req, cfg, codexConvergenceTestAuth("codex-projection"), "token", true)

	session := req.Header.Get("Session_id")
	thread := req.Header.Get("Thread-Id")
	window := req.Header.Get("X-Codex-Window-Id")
	requestID := req.Header.Get("X-Client-Request-Id")

	if session == "" || thread == "" || window == "" || requestID == "" {
		t.Fatalf("client identifiers were dropped: session=%q thread=%q window=%q request=%q",
			session, thread, window, requestID)
	}
	// A suffixed id like "<uuid>-79e1abc32c2ef4d0" is not a shape any Codex
	// client emits; that malformed value was the strongest tell in production.
	for name, value := range map[string]string{"Session_id": session, "Thread-Id": thread, "X-Client-Request-Id": requestID} {
		if _, err := uuid.Parse(value); err != nil {
			t.Fatalf("%s = %q is not a well-formed UUID", name, value)
		}
	}
	if _, err := uuid.Parse(strings.TrimSuffix(window, ":2")); err != nil {
		t.Fatalf("window id = %q, want <uuid>:<counter>", window)
	}
	if !strings.HasSuffix(window, ":2") {
		t.Fatalf("window id = %q, want the client's compaction counter preserved", window)
	}

	// The client sent one root turn, so upstream must still see one.
	if session != thread || session != requestID {
		t.Fatalf("root-turn equality broken: session=%q thread=%q request=%q", session, thread, requestID)
	}
	if strings.HasPrefix(window, session+":") == false {
		t.Fatalf("window id = %q no longer points at session %q", window, session)
	}
	if session == clientRootID {
		t.Fatal("session id reached upstream unscoped, so accounts are not isolated")
	}

	metadata := req.Header.Get("X-Codex-Turn-Metadata")
	for field, want := range map[string]string{"session_id": session, "thread_id": thread, "window_id": window} {
		if got := gjson.Get(metadata, field).String(); got != want {
			t.Fatalf("turn metadata %s = %q, want the header value %q", field, got, want)
		}
	}
	// A single turn carries no cross-request identity, so it is left alone.
	if got := gjson.Get(metadata, "turn_id").String(); got != clientTurnID {
		t.Fatalf("turn metadata turn_id = %q, want the client value untouched", got)
	}
	if got := gjson.Get(metadata, "installation_id").String(); got == clientInstallID || got == "" {
		t.Fatalf("turn metadata installation_id = %q, want a converged value", got)
	}
	if got := gjson.Get(metadata, "sandbox").String(); got != "none" {
		t.Fatalf("unrelated metadata field was dropped: sandbox = %q", got)
	}
}

// TestDeviceModeBodyMatchesHeaders guards the other half of the split that
// production showed: client_metadata described a different session than the
// headers did.
func TestDeviceModeBodyMatchesHeaders(t *testing.T) {
	req := codexDeviceModeRequest(t)
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceDevice)
	auth := codexConvergenceTestAuth("codex-projection")

	ids := resolveCodexConvergedIDs(cfg, auth, req.Context().Value(util.ContextKeyGin).(*gin.Context).Request.Header)
	if ids == nil {
		t.Fatal("device mode returned no snapshot")
	}
	body := []byte(`{"client_metadata":{"x-codex-installation-id":"` + clientInstallID + `","session_id":"` + clientRootID +
		`","thread_id":"` + clientRootID + `","x-codex-window-id":"` + clientWindowID + `"}}`)
	body = applyCodexConvergenceClientMetadata(body, ids)

	for field, want := range map[string]string{
		"client_metadata.session_id":        ids.sessionID,
		"client_metadata.thread_id":         ids.threadID,
		"client_metadata.x-codex-window-id": ids.windowID,
	} {
		if got := gjson.GetBytes(body, field).String(); got != want {
			t.Fatalf("%s = %q, want %q", field, got, want)
		}
	}
}

// TestCacheKeyAndSessionHeaderAgreeEndToEnd walks the real order of operations:
// cacheHelper derives prompt_cache_key and stamps the session headers, then
// applyCodexHeaders projects the convergence snapshot over them. Production
// showed those two steps disagreeing, so the body cache key and the wire
// session have to be asserted together rather than in isolation.
func TestCacheKeyAndSessionHeaderAgreeEndToEnd(t *testing.T) {
	auth := codexConvergenceTestAuth("codex-endtoend")
	// A default Codex client sets prompt_cache_key to its session id.
	payload := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"` + clientRootID + `"}`)
	httpReq := codexCacheHelperRequest(t, auth, sdktranslator.FromString("openai-response"),
		cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: payload})

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = codexDeviceModeRequest(t).Context().Value(util.ContextKeyGin).(*gin.Context).Request
	httpReq = httpReq.WithContext(context.WithValue(httpReq.Context(), util.ContextKeyGin, ginCtx))

	cacheKey := gjson.GetBytes(readRequestBody(t, httpReq), "prompt_cache_key").String()
	applyCodexHeaders(httpReq, codexConvergenceTestConfig(config.CodexFingerprintConvergenceDevice), auth, "token", true)

	session := httpReq.Header.Get("Session_id")
	if cacheKey == "" || session == "" {
		t.Fatalf("cache key = %q, session = %q", cacheKey, session)
	}
	if cacheKey != session {
		t.Fatalf("body cache key %q and header session %q describe different sessions", cacheKey, session)
	}
	if got := httpReq.Header.Get("Conversation_id"); got != session {
		t.Fatalf("Conversation_id = %q, want the session %q", got, session)
	}
	if _, err := uuid.Parse(session); err != nil {
		t.Fatalf("session %q is not a well-formed UUID", session)
	}
}

// TestScopedIdentifierIsInjectiveAndShapePreserving covers the mapping itself:
// it must keep UUID version and variant, keep a v7 timestamp so ids stay
// time-ordered, separate accounts, and never collapse two distinct inputs.
func TestScopedIdentifierIsInjectiveAndShapePreserving(t *testing.T) {
	const scopeA = "account:a"
	const scopeB = "account:b"

	v7 := uuid.Must(uuid.NewV7()).String()
	mapped := codexScopedIdentifier(scopeA, v7)
	parsed, err := uuid.Parse(mapped)
	if err != nil {
		t.Fatalf("mapped v7 id = %q, not a UUID", mapped)
	}
	if parsed.Version() != 7 {
		t.Fatalf("mapped id version = %d, want 7", parsed.Version())
	}
	if mapped[:8] != v7[:8] {
		t.Fatalf("v7 timestamp prefix changed: %q vs %q", mapped[:8], v7[:8])
	}
	if mapped == v7 {
		t.Fatal("identifier was not scoped at all")
	}

	v4 := uuid.NewString()
	if got := uuid.MustParse(codexScopedIdentifier(scopeA, v4)).Version(); got != 4 {
		t.Fatalf("mapped v4 id version = %d, want the original 4", got)
	}

	if again := codexScopedIdentifier(scopeA, v7); again != mapped {
		t.Fatalf("mapping is not deterministic: %q then %q", mapped, again)
	}
	if codexScopedIdentifier(scopeB, v7) == mapped {
		t.Fatal("two accounts mapped one client id onto the same value")
	}
	other := uuid.Must(uuid.NewV7()).String()
	if codexScopedIdentifier(scopeA, other) == mapped {
		t.Fatal("two distinct client ids collided within one account")
	}

	// Non-UUID ids cannot keep a shape they never had, but must stay isolated.
	plain := codexScopedIdentifier(scopeA, "shared-session")
	if plain == "shared-session" || !strings.HasPrefix(plain, "shared-session-") {
		t.Fatalf("non-uuid identifier = %q, want a scope-derived suffix", plain)
	}
	if plain == codexScopedIdentifier(scopeB, "shared-session") {
		t.Fatal("non-uuid identifier is not account-isolated")
	}
}
