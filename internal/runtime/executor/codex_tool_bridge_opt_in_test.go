package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// Regression coverage for #1041. The Codex tool bridge appends a fixed list of
// Computer Use and node_repl definitions, so it is an operator opt-in: unless
// the provider entry sets codex-tool-bridge, the upstream is offered exactly the
// tools the caller declared.

// toolBridgeOptIn is the auth attribute the config synthesizer writes for an
// entry with codex-tool-bridge enabled.
var toolBridgeOptIn = map[string]string{"codex_tool_bridge": "true"}

// toolBridgeCallerTools is the tool list every entrypoint below declares.
var toolBridgeCallerTools = []string{"tool_a"}

// toolBridgeEntrypoint is one client-facing API shape declaring tool_a.
type toolBridgeEntrypoint struct {
	name   string
	source sdktranslator.Format
	body   string
}

var toolBridgeEntrypoints = []toolBridgeEntrypoint{
	{
		name:   "chat",
		source: sdktranslator.FormatOpenAI,
		body:   `{"model":"tool-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"tool_a","description":"t","parameters":{"type":"object","properties":{}}}}]}`,
	},
	{
		name:   "responses",
		source: sdktranslator.FormatOpenAIResponse,
		body:   `{"model":"tool-model","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"tool_a","description":"t","parameters":{"type":"object","properties":{}}}]}`,
	},
	{
		name:   "messages",
		source: sdktranslator.FormatClaude,
		body:   `{"model":"tool-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"tool_a","description":"t","input_schema":{"type":"object","properties":{}}}]}`,
	},
}

func (e toolBridgeEntrypoint) call(model string, stream bool) toolBridgeCall {
	body := strings.Replace(e.body, `"model":"tool-model"`, `"model":"`+model+`"`, 1)
	if stream {
		body = strings.Replace(body, "{", `{"stream":true,`, 1)
	}
	return toolBridgeCall{model: model, source: e.source, payload: []byte(body), stream: stream}
}

type toolBridgeCall struct {
	model   string
	source  sdktranslator.Format
	payload []byte
	stream  bool
	alt     string
}

func (c toolBridgeCall) name() string {
	mode := "non-stream"
	if c.stream {
		mode = "stream"
	}
	if c.alt != "" {
		mode = c.alt
	}
	return c.source.String() + "/" + mode
}

func (c toolBridgeCall) run(t *testing.T, exec cliproxyauth.ProviderExecutor, auth *cliproxyauth.Auth) {
	t.Helper()
	req := cliproxyexecutor.Request{Model: c.model, Payload: c.payload, Format: c.source}
	opts := cliproxyexecutor.Options{SourceFormat: c.source, Stream: c.stream, OriginalRequest: c.payload, Alt: c.alt}
	if !c.stream {
		if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return
	}
	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk: %v", chunk.Err)
		}
	}
}

// toolBridgeUpstream records what the upstream is offered and answers in
// whichever shape the executor asked for.
type toolBridgeUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
}

func newToolBridgeUpstream(t *testing.T) (*httptest.Server, *toolBridgeUpstream) {
	t.Helper()
	rec := &toolBridgeUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, body)
		rec.mu.Unlock()
		stream := gjson.GetBytes(body, "stream").Bool()
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages") && stream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, toolBridgeClaudeStream)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, toolBridgeClaudeMessage)
		case strings.HasSuffix(r.URL.Path, "/responses/compact"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, toolBridgeResponsesCompact)
		case stream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, toolBridgeChatStream)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, toolBridgeChatCompletion)
		}
	}))
	t.Cleanup(server.Close)
	return server, rec
}

// offeredTools lists, in order, the tools of the last forwarded request in any
// of the chat, Responses or Claude shapes.
func (u *toolBridgeUpstream) offeredTools(t *testing.T) []string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatal("upstream received no request")
	}
	names := []string{}
	for _, tool := range gjson.GetBytes(u.bodies[len(u.bodies)-1], "tools").Array() {
		name := tool.Get("function.name").String()
		if name == "" {
			name = tool.Get("name").String()
		}
		names = append(names, name)
	}
	return names
}

func (u *toolBridgeUpstream) assertOffered(t *testing.T, want []string) {
	t.Helper()
	if got := u.offeredTools(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream was offered %d tools %v, want %d %v", len(got), got, len(want), want)
	}
}

// callerToolsWithCodexToolBridge is the caller's tools followed by every bridge
// definition exactly once, in definition order, which is what the bridge has
// always produced when it runs.
func callerToolsWithCodexToolBridge(t *testing.T) []string {
	t.Helper()
	want := append([]string(nil), toolBridgeCallerTools...)
	for _, fn := range mcpCodexToolBridgeFunctions {
		name, _, _, ok := opencodeGoBridgeFunctionParts(fn)
		if !ok {
			t.Fatalf("malformed bridge definition: %v", fn)
		}
		want = append(want, name)
	}
	return want
}

// toolBridgeAuth builds an auth with the given attributes plus optional extras.
func toolBridgeAuth(provider string, attrs map[string]string, extra map[string]string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: provider + "-auth", Provider: provider, Status: cliproxyauth.StatusActive, Attributes: withAttributes(attrs, extra)}
}

// compatEntryAuth mirrors what the config synthesizer builds for an
// openai-compatibility entry named My-Compat.
func compatEntryAuth(baseURL string, extra map[string]string) *cliproxyauth.Auth {
	return toolBridgeAuth("my-compat", map[string]string{
		"base_url":     baseURL,
		"api_key":      "test-upstream-key",
		"compat_name":  "My-Compat",
		"provider_key": "my-compat",
	}, extra)
}

// compatCalls covers chat, Responses and Messages clients, streaming and not,
// plus the Responses compact endpoint served by the same executor.
func compatCalls() []toolBridgeCall {
	calls := make([]toolBridgeCall, 0, 2*len(toolBridgeEntrypoints)+1)
	for _, entry := range toolBridgeEntrypoints {
		calls = append(calls, entry.call("tool-model", false), entry.call("tool-model", true))
	}
	compact := toolBridgeEntrypoints[1].call("tool-model", false)
	compact.alt = "responses/compact"
	return append(calls, compact)
}

func TestOpenAICompatExecutorOffersOnlyCallerToolsByDefault(t *testing.T) {
	for _, call := range compatCalls() {
		t.Run(call.name(), func(t *testing.T) {
			server, upstream := newToolBridgeUpstream(t)
			exec := NewOpenAICompatExecutor("my-compat", &config.Config{})
			call.run(t, exec, compatEntryAuth(server.URL+"/v1", nil))
			upstream.assertOffered(t, toolBridgeCallerTools)
		})
	}
}

func TestOpenAICompatExecutorAddsCodexToolBridgeWhenEntryOptsIn(t *testing.T) {
	want := callerToolsWithCodexToolBridge(t)
	for _, call := range compatCalls() {
		t.Run(call.name(), func(t *testing.T) {
			server, upstream := newToolBridgeUpstream(t)
			exec := NewOpenAICompatExecutor("my-compat", &config.Config{})
			call.run(t, exec, compatEntryAuth(server.URL+"/v1", toolBridgeOptIn))
			upstream.assertOffered(t, want)
		})
	}
}

// Every other provider served by the compatibility executor has no
// codex-tool-bridge setting, so its credentials never carry the opt-in.
func TestCompatBackedProvidersWithoutTheSettingNeverBridgeCodexTools(t *testing.T) {
	chat := toolBridgeEntrypoints[0]
	compact := toolBridgeEntrypoints[1].call("tool-model", false)
	compact.alt = "responses/compact"
	targets := []struct {
		name  string
		exec  cliproxyauth.ProviderExecutor
		auth  func(serverURL string) *cliproxyauth.Auth
		calls []toolBridgeCall
	}{
		{
			name: "cline",
			exec: NewOpenAICompatExecutor("cline", &config.Config{}),
			auth: func(serverURL string) *cliproxyauth.Auth {
				return toolBridgeAuth("cline", map[string]string{"base_url": serverURL + "/api/v1", "api_key": "k", "compat_name": "ClinePass", "provider_key": "cline"}, nil)
			},
			calls: []toolBridgeCall{chat.call("cline-pass/deepseek-v4-flash", false), chat.call("cline-pass/deepseek-v4-flash", true)},
		},
		{
			name: "commandcode",
			exec: NewOpenAICompatExecutor(commandCodeProvider, &config.Config{}),
			auth: func(serverURL string) *cliproxyauth.Auth {
				return toolBridgeAuth(commandCodeProvider, map[string]string{"base_url": serverURL + "/provider/v1", "api_key": "k", "compat_name": "Command Code", "provider_key": commandCodeProvider}, nil)
			},
			calls: []toolBridgeCall{chat.call("tool-model", false), chat.call("tool-model", true)},
		},
		{
			name: "minimax chat",
			exec: NewMiniMaxExecutor("minimax", &config.Config{}),
			auth: func(serverURL string) *cliproxyauth.Auth {
				return toolBridgeAuth("minimax", map[string]string{"base_url": serverURL + "/v1", "api_key": "k", "compat_name": "MiniMax", "provider_key": "minimax"}, nil)
			},
			calls: []toolBridgeCall{chat.call("MiniMax-M2.7", false), chat.call("MiniMax-M2.7", true)},
		},
		{
			name: "registry default branch",
			exec: NewOpenAICompatExecutor("some-unknown-provider", &config.Config{}),
			auth: func(serverURL string) *cliproxyauth.Auth {
				return toolBridgeAuth("some-unknown-provider", map[string]string{"base_url": serverURL + "/v1", "api_key": "k"}, nil)
			},
			calls: []toolBridgeCall{chat.call("tool-model", false), chat.call("tool-model", true)},
		},
		{
			name: "ollama-cloud compat fallback",
			exec: NewOllamaCloudExecutor(&config.Config{}),
			auth: func(serverURL string) *cliproxyauth.Auth {
				return toolBridgeAuth("ollama-cloud", map[string]string{"base_url": serverURL, "api_key": "k", "compat_name": "Ollama Cloud", "provider_key": "ollama-cloud"}, nil)
			},
			calls: []toolBridgeCall{compact},
		},
	}
	for _, target := range targets {
		for _, call := range target.calls {
			t.Run(target.name+"/"+call.name(), func(t *testing.T) {
				server, upstream := newToolBridgeUpstream(t)
				call.run(t, target.exec, target.auth(server.URL))
				upstream.assertOffered(t, toolBridgeCallerTools)
			})
		}
	}
}

// OpenCode Go runs the bridge twice: once on the client payload and again when
// it hands chat-completions models to the compatibility executor. Both points
// follow the key's setting, and together they still add each tool once.
func TestOpenCodeGoExecutorCodexToolBridgeFollowsTheKeySetting(t *testing.T) {
	bridged := callerToolsWithCodexToolBridge(t)
	for _, model := range []struct {
		label string
		name  string
	}{
		{label: "chat-completions model", name: "glm-5"},
		{label: "messages model", name: "minimax-m2.7"},
	} {
		for _, entry := range toolBridgeEntrypoints {
			for _, stream := range []bool{false, true} {
				call := entry.call(model.name, stream)
				for _, tc := range []struct {
					label string
					extra map[string]string
					want  []string
				}{
					{label: "default", want: toolBridgeCallerTools},
					{label: "opted in", extra: toolBridgeOptIn, want: bridged},
				} {
					t.Run(model.label+"/"+call.name()+"/"+tc.label, func(t *testing.T) {
						server, upstream := newToolBridgeUpstream(t)
						oldURL := opencodeGoBaseURL
						opencodeGoBaseURL = server.URL + "/v1"
						t.Cleanup(func() { opencodeGoBaseURL = oldURL })

						exec := NewOpenCodeGoExecutor(&config.Config{})
						call.run(t, exec, toolBridgeAuth(openCodeGoProvider, map[string]string{"api_key": "test-key"}, tc.extra))
						upstream.assertOffered(t, tc.want)
					})
				}
			}
		}
	}
}

func TestMaybeInjectCodexToolBridgeToolsRequiresExplicitOptIn(t *testing.T) {
	payload := []byte(toolBridgeEntrypoints[0].body)
	for _, tc := range []struct {
		name string
		auth *cliproxyauth.Auth
		want bool
	}{
		{name: "no credential", auth: nil},
		{name: "no attributes", auth: &cliproxyauth.Auth{}},
		{name: "explicitly off", auth: &cliproxyauth.Auth{Attributes: map[string]string{"codex_tool_bridge": "false"}}},
		{name: "opted in", auth: &cliproxyauth.Auth{Attributes: toolBridgeOptIn}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := maybeInjectCodexToolBridgeTools(payload, tc.auth)
			bridged := !bytes.Equal(got, payload)
			if bridged != tc.want {
				t.Fatalf("bridged = %t, want %t; payload=%s", bridged, tc.want, got)
			}
		})
	}
}

// A Cline image turn can fall back to an OpenCode Go key. It then runs on that
// key, so the key's own codex-tool-bridge setting decides, as it would for a
// request routed to the key directly.
func TestClineVisionFallbackToOpenCodeGoFollowsTheKeySetting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{name: "key default"},
		{name: "key opted in", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clineHit := false
			clineServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clineHit = true
				http.Error(w, "cline should not receive cross-provider fallback", http.StatusInternalServerError)
			}))
			t.Cleanup(clineServer.Close)
			server, upstream := newToolBridgeUpstream(t)
			oldURL := opencodeGoBaseURL
			opencodeGoBaseURL = server.URL + "/v1"
			t.Cleanup(func() { opencodeGoBaseURL = oldURL })

			exec := NewOpenAICompatExecutor("cline", &config.Config{
				OpenCodeGoKey: []config.OpenCodeGoKey{{
					APIKey:          "go-key",
					Models:          []config.OpenCodeGoModel{{Name: "qwen3.5-plus"}},
					CodexToolBridge: tc.enabled,
				}},
			})
			auth := toolBridgeAuth("cline", map[string]string{
				"base_url":              clineServer.URL + "/api/v1",
				"api_key":               "cline-key",
				"compat_name":           "ClinePass",
				"provider_key":          "cline",
				"vision_fallback_model": "qwen3.5-plus",
			}, nil)
			payload := []byte(`{"model":"cline-pass/deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}],"tools":[{"type":"function","function":{"name":"tool_a","description":"t","parameters":{"type":"object","properties":{}}}}]}`)
			toolBridgeCall{model: "cline-pass/deepseek-v4-flash", source: sdktranslator.FormatOpenAI, payload: payload}.run(t, exec, auth)

			if clineHit {
				t.Fatal("the image turn reached Cline instead of the OpenCode Go fallback")
			}
			want := toolBridgeCallerTools
			if tc.enabled {
				want = callerToolsWithCodexToolBridge(t)
			}
			upstream.assertOffered(t, want)
		})
	}
}

const (
	toolBridgeChatCompletion = `{"id":"chatcmpl_bridge","object":"chat.completion","created":1,"model":"tool-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	toolBridgeChatStream = `data: {"id":"chatcmpl_bridge","object":"chat.completion.chunk","created":1,"model":"tool-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl_bridge","object":"chat.completion.chunk","created":1,"model":"tool-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"

	toolBridgeResponsesCompact = `{"id":"resp_bridge","object":"response","created_at":1,"status":"completed","model":"tool-model","output":[{"type":"message","id":"msg_bridge","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`

	toolBridgeClaudeMessage = `{"id":"msg_bridge","type":"message","role":"assistant","model":"minimax-m2.7","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`

	toolBridgeClaudeStream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_bridge","type":"message","role":"assistant","model":"minimax-m2.7","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
)
