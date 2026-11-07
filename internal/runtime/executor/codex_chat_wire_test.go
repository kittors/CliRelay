package executor

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

// chatWireKey runs one Chat Completions request through the executor and returns
// what reached the upstream: the cache key in the body and the Session_id header.
func chatWireKey(t *testing.T, cfg *config.Config, c codexCall) (key, sessionID string, call codexWireCall) {
	t.Helper()
	env := newCodexExecEnv(t, cfg)
	c.From = "openai"
	if c.Auth == nil {
		c.Auth = codexOAuthAuth()
	}
	if _, err := env.Do(t, c); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	calls := env.Upstream.Calls()
	if len(calls) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(calls))
	}
	call = calls[0]
	return gjson.GetBytes(call.Plain(t), "prompt_cache_key").String(), call.Header.Get("Session_id"), call
}

// What actually leaves for a Chat Completions caller: the body key follows the
// conversation, and under the default identity fingerprint the Session_id header
// that used to be a fresh UUID every turn is the same key.
func TestCodexChatRequestKeepsOneCacheIdentityAcrossTurns(t *testing.T) {
	cfg := chatFingerprintConfig(true, "per-request")
	turn1 := string(chatKeyPayload("explain the borrow checker"))
	turn2 := string(chatKeyPayload("explain the borrow checker",
		`{"role":"assistant","content":"it tracks ownership"}`,
		`{"role":"user","content":"and lifetimes?"}`))

	key1, session1, call1 := chatWireKey(t, cfg, codexCall{Payload: turn1, APIKey: "sk-caller-1", Tenant: chatKeyTenantA})
	key2, session2, _ := chatWireKey(t, cfg, codexCall{Payload: turn2, APIKey: "sk-caller-1", Tenant: chatKeyTenantA})

	if _, err := uuid.Parse(key1); err != nil {
		t.Fatalf("prompt_cache_key %q is not UUID-shaped: %v", key1, err)
	}
	if key1 != key2 {
		t.Fatalf("cache key moved between turns: %q vs %q", key1, key2)
	}
	if session1 != key1 || session2 != key2 {
		t.Fatalf("Session_id = %q / %q, want the cache key %q on every turn", session1, session2, key1)
	}
	for _, header := range []string{"Conversation_id", "Thread-Id", "Session-Id", "X-Codex-Window-Id"} {
		if got := call1.Header.Get(header); got != "" {
			t.Fatalf("%s = %q was added; a Chat client sends none of these", header, got)
		}
	}
	if call1.Header.Get("Content-Encoding") != "zstd" {
		t.Fatal("the request should also leave compressed, as every ChatGPT /responses call does")
	}
}

func TestCodexChatRequestsOfDifferentCallersDoNotShareACacheDomain(t *testing.T) {
	cfg := chatFingerprintConfig(true, "per-request")
	payload := string(chatKeyPayload("the very same prompt"))
	base, _, _ := chatWireKey(t, cfg, codexCall{Payload: payload, APIKey: "sk-caller-1", Tenant: chatKeyTenantA})

	for name, other := range map[string]codexCall{
		"another downstream key": {Payload: payload, APIKey: "sk-caller-2", Tenant: chatKeyTenantA},
		"another tenant":         {Payload: payload, APIKey: "sk-caller-1", Tenant: chatKeyTenantB},
		"no key at all":          {Payload: payload, Tenant: chatKeyTenantA},
	} {
		t.Run(name, func(t *testing.T) {
			key, session, _ := chatWireKey(t, cfg, other)
			if key == "" || key == base {
				t.Fatalf("key = %q: callers must not share %q", key, base)
			}
			if session != key {
				t.Fatalf("Session_id = %q, want the caller's own key %q", session, key)
			}
		})
	}
}

// The header follows the operator's session choice; only the body key is ours.
func TestCodexChatSessionHeaderYieldsToOperatorSessionChoice(t *testing.T) {
	payload := string(chatKeyPayload("hello"))
	cases := []struct {
		name          string
		cfg           *config.Config
		wantSessionID bool
	}{
		{"default identity fingerprint", chatFingerprintConfig(true, ""), true},
		{"per-request session", chatFingerprintConfig(true, "per-request"), true},
		{"fixed session", chatFingerprintConfig(true, "fixed"), false},
		{"server-stable session", chatFingerprintConfig(true, "server-stable"), false},
		{"fingerprint off", chatFingerprintConfig(false, "per-request"), false},
		{"no config", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, session, _ := chatWireKey(t, tc.cfg, codexCall{Payload: payload, APIKey: "sk-caller-1"})
			if key == "" {
				t.Fatal("the body key must be sent whatever the fingerprint says")
			}
			if tc.wantSessionID && session != key {
				t.Fatalf("Session_id = %q, want the cache key %q", session, key)
			}
			if !tc.wantSessionID && session == key {
				t.Fatalf("Session_id = %q equals the cache key: the operator's session choice was overridden", session)
			}
		})
	}
}

// A Responses client already has a session identity of its own; the Chat rules
// must not reach it.
func TestCodexResponsesRequestKeepsItsExistingSessionHeaders(t *testing.T) {
	env := newCodexExecEnv(t, chatFingerprintConfig(true, "per-request"))
	if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload}); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	call := env.Upstream.Calls()[0]
	key := gjson.GetBytes(call.Plain(t), "prompt_cache_key").String()
	if key == "" || call.Header.Get("Conversation_id") != key || call.Header.Get("Session_id") != key {
		t.Fatalf("responses source lost its header pair: key=%q conversation=%q session=%q", key, call.Header.Get("Conversation_id"), call.Header.Get("Session_id"))
	}
	if !strings.HasPrefix(key, "session-abc") {
		t.Fatalf("key = %q, want one derived from the client's own", key)
	}
}
