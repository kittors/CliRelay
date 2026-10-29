package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// A multi-turn agent request as clients without their own caching send it:
// tools, a plain-string system prompt, and several user turns (tool results
// are user turns on the Claude wire).
const cacheProbePayload = `{"model":"claude-sonnet-4-5","max_tokens":16,
	"system":"You are a coding agent. Long instructions follow.",
	"tools":[{"name":"read","input_schema":{"type":"object"}},{"name":"bash","input_schema":{"type":"object"}}],
	"messages":[
		{"role":"user","content":[{"type":"text","text":"first task"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},
		{"role":"assistant","content":[{"type":"text","text":"done"}]},
		{"role":"user","content":[{"type":"text","text":"next task"}]}
	]}`

func captureClaudeUpstreamBody(t *testing.T, fingerprint bool, payload string) []byte {
	t.Helper()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{}
	cfg.IdentityFingerprint.Claude = config.ClaudeIdentityFingerprintConfig{
		Enabled:     fingerprint,
		CLIVersion:  "2.1.88",
		Entrypoint:  "cli",
		SessionMode: "fixed",
		SessionID:   "session-fixed-123",
		DeviceID:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{"api_key": "oauth-access-token", "base_url": server.URL},
		Metadata:   map[string]any{"type": "claude", "account_uuid": "account-uuid-123"},
	}
	if _, err := NewClaudeExecutor(cfg).Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	return gotBody
}

// With the fingerprint on, its own breakpoint on the "You are Claude Code"
// block used to count as the client's, so no other breakpoint was ever added
// and nothing was cached. Tools and the conversation must get breakpoints.
func TestClaudeExecutorCachesConversationWithIdentityFingerprint(t *testing.T) {
	body := captureClaudeUpstreamBody(t, true, cacheProbePayload)

	if got := gjson.GetBytes(body, "system.1.text").String(); got != claudeCodeIdentityText {
		t.Fatalf("system.1.text = %q, want the fingerprint identity block", got)
	}
	if !gjson.GetBytes(body, "tools.1.cache_control").Exists() {
		t.Fatalf("last tool has no cache_control: %s", gjson.GetBytes(body, "tools").Raw)
	}
	// The client's system prompt follows the identity block and needs its own
	// breakpoint; the identity block's one does not cover it.
	if !gjson.GetBytes(body, "system.2.cache_control").Exists() {
		t.Fatalf("client system block has no cache_control: %s", gjson.GetBytes(body, "system").Raw)
	}
	// Second-to-last user turn is the tool_result turn (messages.2).
	if !gjson.GetBytes(body, "messages.2.content.0.cache_control").Exists() {
		t.Fatalf("conversation has no breakpoint: %s", gjson.GetBytes(body, "messages").Raw)
	}
	if n := countCacheControls(body); n != 4 {
		t.Fatalf("cache_control breakpoints = %d, want 4 (identity, tools, system, conversation); Anthropic allows at most 4", n)
	}
}

// A single-turn request has no conversation breakpoint, so the system prompt is
// only cached if its own block is marked.
func TestClaudeExecutorCachesSystemOnFirstTurnWithIdentityFingerprint(t *testing.T) {
	payload := `{"model":"claude-sonnet-4-5","max_tokens":16,
		"system":[{"type":"text","text":"part one"},{"type":"text","text":"part two"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"go"}]}]}`
	body := captureClaudeUpstreamBody(t, true, payload)

	system := gjson.GetBytes(body, "system").Array()
	if len(system) != 4 {
		t.Fatalf("system blocks = %d, want billing, identity and the client's two: %s", len(system), gjson.GetBytes(body, "system").Raw)
	}
	if !system[3].Get("cache_control").Exists() {
		t.Fatalf("last client system block has no cache_control: %s", gjson.GetBytes(body, "system").Raw)
	}
	if system[2].Get("cache_control").Exists() {
		t.Fatalf("only the last client system block should be marked: %s", gjson.GetBytes(body, "system").Raw)
	}
}

// Without the fingerprint the behaviour is unchanged: breakpoints are injected.
func TestClaudeExecutorCachesConversationWithoutIdentityFingerprint(t *testing.T) {
	body := captureClaudeUpstreamBody(t, false, cacheProbePayload)
	if !gjson.GetBytes(body, "tools.1.cache_control").Exists() ||
		!gjson.GetBytes(body, "messages.2.content.0.cache_control").Exists() {
		t.Fatalf("breakpoints missing without fingerprint: %s", body)
	}
}

// A client that places its own breakpoints keeps full control; nothing is
// added beyond the fingerprint's identity block.
func TestClaudeExecutorKeepsClientBreakpointsWithIdentityFingerprint(t *testing.T) {
	payload := `{"model":"claude-sonnet-4-5","max_tokens":16,
		"system":[{"type":"text","text":"client system","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"read","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"a"}]},{"role":"assistant","content":[{"type":"text","text":"b"}]},{"role":"user","content":[{"type":"text","text":"c"}]}]}`
	body := captureClaudeUpstreamBody(t, true, payload)

	if gjson.GetBytes(body, "tools.0.cache_control").Exists() {
		t.Fatalf("a breakpoint was added to tools although the client manages caching: %s", body)
	}
	if gjson.GetBytes(body, "messages.0.content.0.cache_control").Exists() {
		t.Fatalf("a breakpoint was added to messages although the client manages caching: %s", body)
	}
}

func TestCountClientCacheControlsIgnoresFingerprintBlock(t *testing.T) {
	payload := []byte(`{"system":[{"type":"text","text":"billing"},{"type":"text","text":"` + claudeCodeIdentityText + `","cache_control":{"type":"ephemeral"}},{"type":"text","text":"client"}]}`)
	if n := countClientCacheControls(payload); n != 0 {
		t.Fatalf("countClientCacheControls = %d, want 0", n)
	}
	payload = []byte(`{"system":[{"type":"text","text":"` + claudeCodeIdentityText + `","cache_control":{"type":"ephemeral"}},{"type":"text","text":"client","cache_control":{"type":"ephemeral"}}]}`)
	if n := countClientCacheControls(payload); n != 1 {
		t.Fatalf("countClientCacheControls = %d, want 1", n)
	}
}
