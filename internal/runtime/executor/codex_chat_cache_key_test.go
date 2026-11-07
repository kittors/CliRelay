package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	chatKeyTenantA = "11111111-1111-1111-1111-111111111111"
	chatKeyTenantB = "22222222-2222-2222-2222-222222222222"
)

func chatKeyPayload(firstUser string, extra ...string) []byte {
	body := `{"model":"gpt-5.4","messages":[{"role":"system","content":"be brief"},{"role":"user","content":` + jsonString(firstUser) + `}`
	for _, m := range extra {
		body += `,` + m
	}
	return []byte(body + `]}`)
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func chatKeyOpts(tenant string) cliproxyexecutor.Options {
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}
	if tenant != "" {
		opts.Metadata = map[string]any{cliproxyexecutor.TenantMetadataKey: tenant}
	}
	return opts
}

func chatKeyCtx(apiKey string) context.Context {
	if apiKey == "" {
		return context.Background()
	}
	return context.WithValue(context.Background(), util.ContextKeyAPIKey, apiKey)
}

func TestCodexChatPromptCacheKeyIsStableAcrossTurns(t *testing.T) {
	auth := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	opts := chatKeyOpts(chatKeyTenantA)
	ctx := chatKeyCtx("sk-caller-1")

	turn1 := chatKeyPayload("explain the borrow checker")
	turn2 := chatKeyPayload("explain the borrow checker",
		`{"role":"assistant","content":"it tracks ownership"}`,
		`{"role":"user","content":"and lifetimes?"}`)

	first := codexChatPromptCacheKey(ctx, auth, "gpt-5.4", opts, turn1)
	second := codexChatPromptCacheKey(ctx, auth, "gpt-5.4", opts, turn2)
	if first == "" {
		t.Fatal("no key derived for a plain chat request")
	}
	if first != second {
		t.Fatalf("key changed between turns of one conversation: %q vs %q", first, second)
	}
	parsed, err := uuid.Parse(first)
	if err != nil {
		t.Fatalf("key %q is not UUID-shaped: %v", first, err)
	}
	if parsed.Version() != 4 {
		t.Fatalf("key version = %d, want 4 (the shape the fingerprint layer already uses for session ids)", parsed.Version())
	}
}

func TestCodexChatPromptCacheKeyKeepsCallersAndAccountsApart(t *testing.T) {
	authA := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	authB := codexSessionIsolationAuth("auth-b", "acct-b", "b@example.com")
	payload := chatKeyPayload("same first message everywhere")
	base := codexChatPromptCacheKey(chatKeyCtx("sk-caller-1"), authA, "gpt-5.4", chatKeyOpts(chatKeyTenantA), payload)

	cases := []struct {
		name string
		got  string
	}{
		{"other tenant", codexChatPromptCacheKey(chatKeyCtx("sk-caller-1"), authA, "gpt-5.4", chatKeyOpts(chatKeyTenantB), payload)},
		{"other downstream key", codexChatPromptCacheKey(chatKeyCtx("sk-caller-2"), authA, "gpt-5.4", chatKeyOpts(chatKeyTenantA), payload)},
		{"no downstream key", codexChatPromptCacheKey(chatKeyCtx(""), authA, "gpt-5.4", chatKeyOpts(chatKeyTenantA), payload)},
		{"other upstream account", codexChatPromptCacheKey(chatKeyCtx("sk-caller-1"), authB, "gpt-5.4", chatKeyOpts(chatKeyTenantA), payload)},
		{"other model", codexChatPromptCacheKey(chatKeyCtx("sk-caller-1"), authA, "gpt-5.5", chatKeyOpts(chatKeyTenantA), payload)},
		{"other conversation", codexChatPromptCacheKey(chatKeyCtx("sk-caller-1"), authA, "gpt-5.4", chatKeyOpts(chatKeyTenantA), chatKeyPayload("a different opening message"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got == "" {
				t.Fatal("no key derived")
			}
			if tc.got == base {
				t.Fatalf("key is shared across this boundary: %q", tc.got)
			}
		})
	}

	t.Run("system tenant equals unset tenant", func(t *testing.T) {
		unset := codexChatPromptCacheKey(chatKeyCtx("sk-caller-1"), authA, "gpt-5.4", chatKeyOpts(""), payload)
		system := codexChatPromptCacheKey(chatKeyCtx("sk-caller-1"), authA, "gpt-5.4", chatKeyOpts(cliproxyauth.NormalizedTenantID("")), payload)
		if unset != system {
			t.Fatalf("an unset tenant must resolve to the system tenant: %q vs %q", unset, system)
		}
	})
	t.Run("reasoning suffix does not move the conversation", func(t *testing.T) {
		withSuffix := codexChatPromptCacheKey(chatKeyCtx("sk-caller-1"), authA, "gpt-5.4(high)", chatKeyOpts(chatKeyTenantA), payload)
		if withSuffix != base {
			t.Fatalf("changing reasoning effort mid-conversation re-keyed it: %q vs %q", withSuffix, base)
		}
	})
}

func TestCodexChatPromptCacheKeyConversationIdentity(t *testing.T) {
	auth := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	ctx := chatKeyCtx("sk-caller-1")
	key := func(opts cliproxyexecutor.Options, payload []byte) string {
		return codexChatPromptCacheKey(ctx, auth, "gpt-5.4", opts, payload)
	}

	t.Run("client prompt_cache_key wins over the body-derived identity", func(t *testing.T) {
		a := key(chatKeyOpts(chatKeyTenantA), []byte(`{"prompt_cache_key":"thread-7","messages":[{"role":"user","content":"first"}]}`))
		b := key(chatKeyOpts(chatKeyTenantA), []byte(`{"prompt_cache_key":"thread-7","messages":[{"role":"user","content":"something else"}]}`))
		other := key(chatKeyOpts(chatKeyTenantA), []byte(`{"prompt_cache_key":"thread-8","messages":[{"role":"user","content":"first"}]}`))
		if a == "" || a != b {
			t.Fatalf("one client key must give one cache key regardless of content: %q vs %q", a, b)
		}
		if a == other {
			t.Fatalf("different client keys collided: %q", a)
		}
		if strings.Contains(a, "thread-7") {
			t.Fatalf("client key leaked into the derived value: %q", a)
		}
	})

	t.Run("sticky session metadata keeps conversations with different openings together", func(t *testing.T) {
		opts := chatKeyOpts(chatKeyTenantA)
		opts.Metadata[cliproxyexecutor.SessionStickyMetadataKey] = "session-42"
		a := key(opts, chatKeyPayload("opening one"))
		b := key(opts, chatKeyPayload("opening two"))
		if a == "" || a != b {
			t.Fatalf("same session must give the same key: %q vs %q", a, b)
		}
	})

	t.Run("nothing to key on gives no key", func(t *testing.T) {
		if got := key(chatKeyOpts(chatKeyTenantA), []byte(`{"messages":[]}`)); got != "" {
			t.Fatalf("empty conversation must not invent a key, got %q", got)
		}
	})

	t.Run("no upstream account identity gives no key", func(t *testing.T) {
		if got := codexChatPromptCacheKey(ctx, nil, "gpt-5.4", chatKeyOpts(chatKeyTenantA), chatKeyPayload("hi")); got != "" {
			t.Fatalf("a key that cannot be kept apart per account must not be sent, got %q", got)
		}
	})
}

func TestCodexChatPromptCacheKeyRevealsNothingReadable(t *testing.T) {
	auth := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	const apiKey = "sk-live-super-secret-key"
	const opening = "my password is hunter2"
	got := codexChatPromptCacheKey(chatKeyCtx(apiKey), auth, "gpt-5.4", chatKeyOpts(chatKeyTenantA), chatKeyPayload(opening))

	uuidShape := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidShape.MatchString(got) {
		t.Fatalf("key %q is not a UUIDv4-shaped value", got)
	}
	keyDigest := sha256.Sum256([]byte(apiKey))
	for _, forbidden := range []string{apiKey, "hunter2", chatKeyTenantA, "acct-a", "a@example.com", hex.EncodeToString(keyDigest[:8])} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("derived key %q contains %q", got, forbidden)
		}
	}
}

func chatFingerprintConfig(enabled bool, sessionMode string) *config.Config {
	cfg := &config.Config{}
	cfg.IdentityFingerprint.Codex = config.DefaultCodexIdentityFingerprint()
	cfg.IdentityFingerprint.Codex.Enabled = enabled
	cfg.IdentityFingerprint.Codex.SessionMode = sessionMode
	return cfg
}

func TestCodexCacheHelperChatSourceKeyAndHeaders(t *testing.T) {
	auth := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: chatKeyPayload("hello")}
	opts := chatKeyOpts(chatKeyTenantA)
	run := func(cfg *config.Config) *http.Request {
		t.Helper()
		got, err := (&CodexExecutor{cfg: cfg}).cacheHelper(chatKeyCtx("sk-caller-1"), auth, sdktranslator.FromString("openai"),
			"https://chatgpt.com/backend-api/codex/responses", req, req.Payload, opts)
		if err != nil {
			t.Fatalf("cacheHelper() error = %v", err)
		}
		return got
	}

	t.Run("body key always, Session_id only where one is sent anyway", func(t *testing.T) {
		cases := []struct {
			name          string
			cfg           *config.Config
			wantSessionID bool
		}{
			{"fingerprint on, per-request session", chatFingerprintConfig(true, "per-request"), true},
			{"fingerprint on, default session mode", chatFingerprintConfig(true, ""), true},
			{"fingerprint on, fixed session", chatFingerprintConfig(true, "fixed"), false},
			{"fingerprint on, server-stable session", chatFingerprintConfig(true, "server-stable"), false},
			{"fingerprint off", chatFingerprintConfig(false, "per-request"), false},
			{"no config", nil, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				httpReq := run(tc.cfg)
				key := gjson.GetBytes(readRequestBody(t, httpReq), "prompt_cache_key").String()
				if key == "" {
					t.Fatal("prompt_cache_key missing from the body")
				}
				if got := httpReq.Header.Get("Session_id"); tc.wantSessionID && got != key {
					t.Fatalf("Session_id = %q, want the cache key %q", got, key)
				} else if !tc.wantSessionID && got != "" {
					t.Fatalf("Session_id = %q, want none: the operator's session choice must stand", got)
				}
				for _, header := range []string{"Conversation_id", "Thread-Id", "Session-Id", "X-Codex-Window-Id"} {
					if got := httpReq.Header.Get(header); got != "" {
						t.Fatalf("%s = %q: a Chat client sends none of these, so this layer must not add them", header, got)
					}
				}
			})
		}
	})

	t.Run("responses and claude sources keep their existing header pair", func(t *testing.T) {
		respReq := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"prompt_cache_key":"k"}`)}
		got, err := (&CodexExecutor{}).cacheHelper(context.Background(), auth, sdktranslator.FromString("openai-response"),
			"https://chatgpt.com/backend-api/codex/responses", respReq, respReq.Payload, cliproxyexecutor.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Header.Get("Conversation_id") == "" || got.Header.Get("Session_id") == "" {
			t.Fatalf("responses source lost its headers: %#v", got.Header)
		}
	})
}
