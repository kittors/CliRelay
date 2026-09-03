package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func codexConvergenceTestConfig(mode string) *config.Config {
	return &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{
				Enabled:         true,
				UserAgent:       "codex_cli_rs/0.144.1",
				Originator:      "codex_cli_rs",
				SessionMode:     "per-request",
				ConvergenceMode: mode,
			},
		},
	}
}

func codexConvergenceTestAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "codex-token",
			"account_id":   id + "-account",
		},
	}
}

func TestResolveCodexConvergedIDsIsStableAcrossRequests(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	auth := codexConvergenceTestAuth("codex-stable")

	headers := http.Header{}
	headers.Set("session-id", "client-session-a")

	first := resolveCodexConvergedIDs(cfg, auth, headers)
	second := resolveCodexConvergedIDs(cfg, auth, headers)
	if first == nil || second == nil {
		t.Fatal("convergence returned nil for an auth with a resolvable scope")
	}
	if first.installationID != second.installationID {
		t.Fatalf("installation id drifted between requests: %q vs %q", first.installationID, second.installationID)
	}
	if first.sessionID != second.sessionID {
		t.Fatalf("session id drifted between requests: %q vs %q", first.sessionID, second.sessionID)
	}
	if first.threadID != second.threadID {
		t.Fatalf("thread id drifted for the same client session: %q vs %q", first.threadID, second.threadID)
	}
	// Turn id is per-request by design; a constant turn id would be the anomaly.
	if first.turnID == second.turnID {
		t.Fatal("turn id was reused across requests, want a fresh value each turn")
	}
}

func TestResolveCodexConvergedIDsSeparatesAccounts(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	headers := http.Header{}
	headers.Set("session-id", "shared-client-session")

	a := resolveCodexConvergedIDs(cfg, codexConvergenceTestAuth("codex-a"), headers)
	b := resolveCodexConvergedIDs(cfg, codexConvergenceTestAuth("codex-b"), headers)
	if a == nil || b == nil {
		t.Fatal("convergence returned nil for accounts with resolvable scopes")
	}
	if a.installationID == b.installationID {
		t.Fatal("two accounts collapsed onto one installation id")
	}
	if a.sessionID == b.sessionID {
		t.Fatal("two accounts collapsed onto one session id")
	}
}

func TestResolveCodexConvergedIDsDerivesThreadPerClientSession(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	auth := codexConvergenceTestAuth("codex-threads")

	first := http.Header{}
	first.Set("session-id", "client-session-1")
	second := http.Header{}
	second.Set("session-id", "client-session-2")

	a := resolveCodexConvergedIDs(cfg, auth, first)
	b := resolveCodexConvergedIDs(cfg, auth, second)
	if a == nil || b == nil {
		t.Fatal("convergence returned nil for an auth with a resolvable scope")
	}
	if a.sessionID != b.sessionID {
		t.Fatal("session mode must converge both clients onto one session id")
	}
	if a.threadID == b.threadID {
		t.Fatal("distinct client sessions must map to distinct thread ids")
	}
}

func TestResolveCodexConvergedIDsFullCollapsesThread(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceFull)
	auth := codexConvergenceTestAuth("codex-full")

	first := http.Header{}
	first.Set("session-id", "client-session-1")
	second := http.Header{}
	second.Set("session-id", "client-session-2")

	a := resolveCodexConvergedIDs(cfg, auth, first)
	b := resolveCodexConvergedIDs(cfg, auth, second)
	if a == nil || b == nil {
		t.Fatal("convergence returned nil for an auth with a resolvable scope")
	}
	if a.threadID != b.threadID || a.threadID != a.sessionID {
		t.Fatalf("full mode must collapse every client onto the session thread: %q vs %q", a.threadID, b.threadID)
	}
}

func TestResolveCodexConvergedIDsOffAndAPIKey(t *testing.T) {
	auth := codexConvergenceTestAuth("codex-off")
	if ids := resolveCodexConvergedIDs(codexConvergenceTestConfig(config.CodexFingerprintConvergenceOff), auth, nil); ids != nil {
		t.Fatal("off mode must not converge anything")
	}

	disabled := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	disabled.IdentityFingerprint.Codex.Enabled = false
	if ids := resolveCodexConvergedIDs(disabled, auth, nil); ids != nil {
		t.Fatal("disabling the Codex identity fingerprint must also disable convergence")
	}

	apiKeyAuth := codexConvergenceTestAuth("codex-api-key")
	apiKeyAuth.Attributes = map[string]string{"api_key": "sk-test"}
	if ids := resolveCodexConvergedIDs(codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession), apiKeyAuth, nil); ids != nil {
		t.Fatal("API key credentials carry no device quota and must not be converged")
	}
}

func TestResolveCodexConvergedIDsHonoursPinnedInstallationID(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceDevice)
	cfg.IdentityFingerprint.Codex.InstallationID = "11111111-2222-3333-4444-555555555555"

	ids := resolveCodexConvergedIDs(cfg, codexConvergenceTestAuth("codex-pinned"), nil)
	if ids == nil {
		t.Fatal("convergence returned nil for an auth with a resolvable scope")
	}
	if ids.installationID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("installation id = %q, want the operator-pinned value", ids.installationID)
	}
	if ids.sessionID != "" || ids.threadID != "" {
		t.Fatal("device mode must leave session and thread identifiers untouched")
	}
}

func TestApplyCodexHeadersConvergesTurnMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("session-id", "client-session")
	ginCtx.Request.Header.Set("X-Codex-Turn-Metadata",
		`{"installation_id":"client-install","session_id":"client-session","thread_id":"client-thread","turn_id":"client-turn","window_id":"client-window","sandbox":"workspace-write"}`)
	ginCtx.Request.Header.Set("X-Client-Request-Id", "client-request")

	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), util.ContextKeyGin, ginCtx))

	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	auth := codexConvergenceTestAuth("codex-headers")
	applyCodexHeaders(req, cfg, auth, "token", true)

	metadata := req.Header.Get("X-Codex-Turn-Metadata")
	for _, field := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id"} {
		got := gjson.Get(metadata, field).String()
		if got == "" {
			t.Fatalf("turn metadata %s was dropped", field)
		}
		if got == "client-"+shortFieldName(field) {
			t.Fatalf("turn metadata %s still carries the client value %q", field, got)
		}
	}
	// Unrelated fields must survive: dropping them would itself change the fingerprint.
	if got := gjson.Get(metadata, "sandbox").String(); got != "workspace-write" {
		t.Fatalf("sandbox = %q, want the client value to be preserved", got)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got == "client-request" || got == "" {
		t.Fatalf("X-Client-Request-Id = %q, want a converged value", got)
	}

	ids := resolveCodexConvergedIDs(cfg, auth, ginCtx.Request.Header)
	if got := req.Header.Get("Session_id"); got != ids.sessionID {
		t.Fatalf("Session_id = %q, want converged %q", got, ids.sessionID)
	}
	if got := req.Header.Get("Session-Id"); got != ids.sessionID {
		t.Fatalf("Session-Id = %q, want converged %q", got, ids.sessionID)
	}
	if got := gjson.Get(metadata, "installation_id").String(); got != ids.installationID {
		t.Fatalf("turn metadata installation_id = %q, want converged %q", got, ids.installationID)
	}
}

// shortFieldName maps a turn metadata field to the suffix used by the client
// placeholder values in the test above.
func shortFieldName(field string) string {
	switch field {
	case "installation_id":
		return "install"
	case "session_id":
		return "session"
	case "thread_id":
		return "thread"
	case "turn_id":
		return "turn"
	case "window_id":
		return "window"
	default:
		return field
	}
}

func TestApplyCodexHeadersDoesNotIntroduceUnsentIdentifiers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("session-id", "client-session")

	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), util.ContextKeyGin, ginCtx))

	applyCodexHeaders(req, codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession),
		codexConvergenceTestAuth("codex-nosurface"), "token", true)

	// The client sent none of these, so convergence must not hand upstream a
	// header it would never have received.
	for _, key := range []string{"X-Codex-Installation-Id", "X-Codex-Window-Id", "Thread-Id"} {
		if got := req.Header.Get(key); got != "" {
			t.Fatalf("%s = %q, want no header when the client sent none", key, got)
		}
	}
}

func TestApplyCodexConvergenceClientMetadataRewritesOnlyPresentFields(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	ids := resolveCodexConvergedIDs(cfg, codexConvergenceTestAuth("codex-body"), nil)
	if ids == nil {
		t.Fatal("convergence returned nil for an auth with a resolvable scope")
	}

	body := []byte(`{"model":"gpt-5","client_metadata":{"session_id":"client-session","x-codex-installation-id":"client-install","cli_version":"0.144.1"}}`)
	got := applyCodexConvergenceClientMetadata(body, ids)

	if v := gjson.GetBytes(got, "client_metadata.session_id").String(); v != ids.sessionID {
		t.Fatalf("client_metadata.session_id = %q, want converged %q", v, ids.sessionID)
	}
	if v := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); v != ids.installationID {
		t.Fatalf("client_metadata installation id = %q, want converged %q", v, ids.installationID)
	}
	if v := gjson.GetBytes(got, "client_metadata.cli_version").String(); v != "0.144.1" {
		t.Fatalf("unrelated client_metadata field was altered: %q", v)
	}
	// thread_id was absent from the client payload and must stay absent.
	if gjson.GetBytes(got, "client_metadata.thread_id").Exists() {
		t.Fatal("convergence added a client_metadata field the client never sent")
	}
	if v := gjson.GetBytes(got, "model").String(); v != "gpt-5" {
		t.Fatalf("model = %q, want the request to be otherwise untouched", v)
	}
}

func TestApplyCodexConvergenceLeavesOmittedTurnMetadataFieldsAbsent(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	ids := resolveCodexConvergedIDs(cfg, codexConvergenceTestAuth("codex-partial"), nil)
	if ids == nil {
		t.Fatal("convergence returned nil for an auth with a resolvable scope")
	}

	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{"installation_id":"client-install","sandbox":"read-only"}`)
	applyCodexConvergenceHeaders(headers, ids, http.Header{})

	metadata := headers.Get("X-Codex-Turn-Metadata")
	if got := gjson.Get(metadata, "installation_id").String(); got != ids.installationID {
		t.Fatalf("installation_id = %q, want the converged value", got)
	}
	// The client omitted these; convergence must not introduce them.
	for _, field := range []string{"session_id", "thread_id", "turn_id", "window_id", "turn_started_at_unix_ms"} {
		if gjson.Get(metadata, field).Exists() {
			t.Fatalf("turn metadata gained %s, which the client never sent", field)
		}
	}
	if got := gjson.Get(metadata, "sandbox").String(); got != "read-only" {
		t.Fatalf("sandbox = %q, want unrelated fields preserved", got)
	}
}

func TestApplyCodexConvergenceClientMetadataSkipsBodiesWithoutIt(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	ids := resolveCodexConvergedIDs(cfg, codexConvergenceTestAuth("codex-nobody"), nil)

	body := []byte(`{"model":"gpt-5","input":[]}`)
	got := applyCodexConvergenceClientMetadata(body, ids)
	if string(got) != string(body) {
		t.Fatalf("body without client_metadata was modified: %s", got)
	}
	if gjson.GetBytes(got, "client_metadata").Exists() {
		t.Fatal("convergence fabricated a client_metadata object")
	}
}

func TestApplyCodexConvergenceHeadersSharesTurnIDWithBody(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	ids := resolveCodexConvergedIDs(cfg, codexConvergenceTestAuth("codex-shared"), nil)
	if ids == nil {
		t.Fatal("convergence returned nil for an auth with a resolvable scope")
	}

	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{"turn_id":"client-turn"}`)
	applyCodexConvergenceHeaders(headers, ids, http.Header{})

	body := []byte(`{"client_metadata":{"turn_id":"client-turn"}}`)
	body = applyCodexConvergenceClientMetadata(body, ids)

	headerTurn := gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "turn_id").String()
	bodyTurn := gjson.GetBytes(body, "client_metadata.turn_id").String()
	if headerTurn == "" || headerTurn != bodyTurn {
		t.Fatalf("header turn id %q and body turn id %q must match", headerTurn, bodyTurn)
	}
}

func TestDeviceOnlyLeavesSessionUntouched(t *testing.T) {
	cfg := codexConvergenceTestConfig(config.CodexFingerprintConvergenceSession)
	ids := resolveCodexConvergedIDs(cfg, codexConvergenceTestAuth("codex-ws"), nil)
	if ids == nil {
		t.Fatal("convergence returned nil for an auth with a resolvable scope")
	}

	headers := http.Header{}
	headers.Set("Session_id", "prompt-cache-key")
	headers.Set("Conversation_id", "prompt-cache-key")
	applyCodexConvergenceHeaders(headers, ids.deviceOnly(), http.Header{})

	if got := headers.Get("Session_id"); got != "prompt-cache-key" {
		t.Fatalf("Session_id = %q, want the prompt cache association preserved", got)
	}
	if got := headers.Get("Conversation_id"); got != "prompt-cache-key" {
		t.Fatalf("Conversation_id = %q, want it preserved", got)
	}
}

func TestCodexConvergenceDefaultsToDeviceMode(t *testing.T) {
	cfg := codexConvergenceTestConfig("")
	if got := codexConvergenceMode(cfg, codexConvergenceTestAuth("codex-default")); got != config.CodexFingerprintConvergenceDevice {
		t.Fatalf("convergence mode = %q, want the device default", got)
	}

	cfg = codexConvergenceTestConfig("nonsense")
	if got := codexConvergenceMode(cfg, codexConvergenceTestAuth("codex-bogus")); got != config.CodexFingerprintConvergenceDevice {
		t.Fatalf("convergence mode = %q, want an unrecognised value to fall back to the default", got)
	}

	// Test per-account override
	authWithOverride := codexConvergenceTestAuth("codex-override")
	authWithOverride.Metadata["codex_convergence_mode"] = "full"
	if got := codexConvergenceMode(cfg, authWithOverride); got != config.CodexFingerprintConvergenceFull {
		t.Fatalf("convergence mode = %q, want overridden full", got)
	}

	authWithOff := codexConvergenceTestAuth("codex-off")
	authWithOff.Attributes = map[string]string{"codex_convergence_mode": "off"}
	if got := codexConvergenceMode(cfg, authWithOff); got != config.CodexFingerprintConvergenceOff {
		t.Fatalf("convergence mode = %q, want overridden off", got)
	}
}
