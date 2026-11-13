package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The error in the report names input[140]. Keep that position and more than
// six incompatible items: a per-index retry would exhaust a bounded budget.
func schemaReasoningHistory() string {
	items := []string{`{"type":"message","role":"user","content":[{"type":"input_text","text":"task"}]}`}
	for i := 1; i <= 150; i++ {
		items = append(items, fmt.Sprintf(`{"type":"reasoning","summary":[{"type":"summary_text","text":"summary %d"}],"content":[{"type":"reasoning_text","text":"provider text"}],"opaque":9007199254740993}`, i))
	}
	items = append(items, reasoningItem(openAICipherA), `{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}`, `{"type":"function_call_output","call_id":"c1","output":"tool result"}`)
	return `{"model":"gpt-5.4","reasoning":{"effort":"low"},"input":[` + strings.Join(items, ",") + `]}`
}

func schemaHasReasoningContent(body []byte) bool {
	found := false
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" && item.Get("content").IsArray() && item.Get("content.#").Int() > 0 {
			found = true
		}
		return !found
	})
	return found
}

const schemaReasoningError = `{"error":{"code":"array_above_max_length","message":"Invalid 'input[140].content': array too long. Expected an array with maximum length 0, but got an array with length 1 instead.","param":"input[140].content","type":"invalid_request_error"}}`

func checkSchemaReasoningHistory(t *testing.T, body []byte) {
	t.Helper()
	if schemaHasReasoningContent(body) {
		t.Fatal("outbound history still contains nonempty reasoning.content")
	}
	if got := gjson.GetBytes(body, "input.#").Int(); got != 154 {
		t.Fatalf("items=%d, want 154", got)
	}
	for _, i := range []int{1, 140, 150} {
		item := gjson.GetBytes(body, fmt.Sprintf("input.%d", i))
		if item.Get("type").String() != "reasoning" || item.Get("summary.0.text").String() != fmt.Sprintf("summary %d", i) || item.Get("opaque").Raw != "9007199254740993" {
			t.Fatalf("reasoning fields changed: %s", item.Raw)
		}
	}
	if gjson.GetBytes(body, "input.0.content.0.text").String() != "task" || gjson.GetBytes(body, "input.151.encrypted_content").String() != openAICipherA || gjson.GetBytes(body, "input.152.call_id").String() != "c1" || gjson.GetBytes(body, "input.153.output").String() != "tool result" {
		t.Fatal("portable history or tool pairing was lost")
	}
}

func TestCodexReasoningContentHTTPRegression(t *testing.T) {
	for _, mode := range []string{"nonstream", "stream", "compact"} {
		t.Run(mode, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			env.Upstream.respond = func(_ int, call codexWireCall) *http.Response {
				if schemaHasReasoningContent(call.Plain(t)) {
					return codexErrorResponse(400, schemaReasoningError)
				}
				if mode == "compact" {
					return codexErrorResponse(200, `{"object":"response.compaction","output":[]}`)
				}
				return nil
			}
			payload := schemaReasoningHistory()
			if !schemaHasReasoningContent([]byte(payload)) {
				t.Fatal("fixture must exercise the rejected field")
			}
			call := codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: payload, Stream: mode == "stream"}
			if mode == "compact" {
				call.Alt = "responses/compact"
			}
			if _, err := env.Do(t, call); err != nil {
				t.Fatalf("reported upstream rejection reached the client: %v", err)
			}
			calls := env.Upstream.Calls()
			if len(calls) != 1 {
				t.Fatalf("upstream calls=%d, want 1", len(calls))
			}
			checkSchemaReasoningHistory(t, calls[0].Plain(t))
		})
	}
}

func TestCodexReasoningContentWebsocketRegression(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			captured := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, body, err := conn.ReadMessage()
				if err != nil {
					return
				}
				captured <- body
				event := `{"type":"response.completed","response":{"id":"resp_schema","object":"response","status":"completed","output":[]}}`
				if schemaHasReasoningContent(body) {
					event = `{"type":"error","error":{"code":"array_above_max_length","message":"reasoning.content maximum length 0"}}`
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
			}))
			defer server.Close()
			// Preconnect a local peer to the real ChatGPT account's session. This runs
			// destination-scoped rules and both websocket entry points without public IO.
			conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			executor := NewCodexWebsocketsExecutor(&config.Config{})
			sess := executor.getOrCreateSession("schema-regression")
			sess.conn = conn
			defer executor.CloseExecutionSession("schema-regression")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(schemaReasoningHistory())}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: stream, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "schema-regression"}}
			if stream {
				result, err := executor.ExecuteStream(ctx, codexOAuthAuth(), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := executor.Execute(ctx, codexOAuthAuth(), req, opts); err != nil {
				t.Fatal(err)
			}
			checkSchemaReasoningHistory(t, <-captured)
		})
	}
}

func TestPrepareCodexReasoningContentBoundaries(t *testing.T) {
	for _, content := range []string{"", `,"content":[]`, `,"content":null`, `,"content":"provider text"`} {
		body := []byte(`{"input":[{"type":"reasoning","summary":[]` + content + `},{"type":"message","content":[{"type":"input_text","text":"keep"}]}]}`)
		got := prepareCodexResponsesBody(body, nil, "", true)
		if !bytes.Equal(body, got) {
			t.Fatalf("portable history changed: %s", got)
		}
	}
	body := []byte(schemaReasoningHistory())
	original := bytes.Clone(body)
	got := prepareCodexResponsesBody(body, nil, "", false)
	if !bytes.Equal(got, original) || !bytes.Equal(body, original) {
		t.Fatal("compatible provider history was rewritten")
	}
	got = prepareCodexResponsesBody(body, nil, "", true)
	checkSchemaReasoningHistory(t, got)
	if !bytes.Equal(body, original) {
		t.Fatal("client payload mutated")
	}
	if twice := prepareCodexResponsesBody(got, nil, "", true); !bytes.Equal(got, twice) {
		t.Fatal("normalization is not idempotent")
	}
}

func schemaClaudePayload(clientBreakpoints int, automatic bool) string {
	blocks := make([]string, clientBreakpoints)
	for i := range blocks {
		blocks[i] = fmt.Sprintf(`{"type":"text","text":"instructions %d","cache_control":{"type":"ephemeral"}}`, i)
	}
	system := `"client instructions"`
	if clientBreakpoints > 0 {
		system = `[` + strings.Join(blocks, ",") + `]`
	}
	body := `{"model":"claude-sonnet-4-5","max_tokens":16,"system":` + system + `,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`
	if automatic {
		body, _ = sjson.SetRaw(body, "cache_control", `{"type":"ephemeral"}`)
	}
	return body
}

func schemaClaudeMarkerCount(body []byte) int {
	count := 0
	var visit func(gjson.Result)
	visit = func(node gjson.Result) {
		if node.IsArray() || node.IsObject() {
			node.ForEach(func(key, value gjson.Result) bool {
				if key.String() == "cache_control" {
					count++
				} else {
					visit(value)
				}
				return true
			})
		}
	}
	visit(gjson.ParseBytes(body))
	return count
}

func TestClaudeCacheBudgetRegression(t *testing.T) {
	for _, mode := range []string{"nonstream", "stream", "count_tokens", "skip_processing"} {
		for _, tc := range []struct {
			name                   string
			client                 int
			automatic, fingerprint bool
			want                   int
		}{
			{"client_four", 4, false, true, 4},
			{"client_five", 5, false, true, 4},
			{"client_five_without_fingerprint", 5, false, false, 4},
			{"automatic_only", 0, true, true, 2},
			{"automatic_plus_three", 3, true, true, 4},
			{"automatic_plus_four", 4, true, true, 4},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				captured := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					captured <- body
					w.Header().Set("Content-Type", "application/json")
					if schemaClaudeMarkerCount(body) > 4 {
						w.WriteHeader(400)
						_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"A maximum of 4 blocks with cache_control may be provided. Found 5."}}`))
						return
					}
					if mode == "count_tokens" {
						_, _ = w.Write([]byte(`{"input_tokens":42}`))
						return
					}
					if mode == "stream" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_schema\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
						return
					}
					_, _ = w.Write([]byte(`{"id":"msg_schema","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
				}))
				defer server.Close()
				cfg := &config.Config{}
				cfg.IdentityFingerprint.Claude = config.DefaultClaudeIdentityFingerprint()
				cfg.IdentityFingerprint.Claude.Enabled = tc.fingerprint
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
				if mode == "skip_processing" {
					auth.Attributes["skip_anthropic_processing"] = "true"
				}
				payload := schemaClaudePayload(tc.client, tc.automatic)
				req := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: []byte(payload)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), Stream: mode == "stream"}
				executor := NewClaudeExecutor(cfg)
				switch mode {
				case "count_tokens":
					_, err := executor.CountTokens(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
				case "stream":
					result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				default:
					_, err := executor.Execute(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
				}
				got := <-captured
				want := tc.want
				if mode == "skip_processing" && tc.automatic && tc.client == 0 {
					want = 1
				}
				if n := schemaClaudeMarkerCount(got); n != want {
					t.Fatalf("outbound markers=%d, want %d", n, want)
				}
				if tc.automatic && !gjson.GetBytes(got, "cache_control").Exists() {
					t.Fatal("client automatic cache was removed")
				}
				if tc.client == 4 && !tc.automatic {
					for i := 0; i < 4; i++ {
						found := false
						gjson.GetBytes(got, "system").ForEach(func(_, block gjson.Result) bool {
							if block.Get("text").String() == fmt.Sprintf("instructions %d", i) && block.Get("cache_control").Exists() {
								found = true
							}
							return true
						})
						if !found {
							t.Fatalf("legal client breakpoint %d was removed", i)
						}
					}
				}
				if gjson.GetBytes(got, "messages.2.content").String() != "next" || gjson.GetBytes(got, "tools.0.name").String() != "read" {
					t.Fatal("request content changed")
				}
			})
		}
	}
}

func TestCodexReasoningContentDestinationBoundaries(t *testing.T) {
	body := []byte(schemaReasoningHistory())
	for _, tc := range []struct {
		name, base       string
		oauth, wantClean bool
	}{
		{"default OAuth", "", true, true},
		{"ChatGPT OAuth", "https://chatgpt.com/backend-api/codex", true, true},
		{"ChatGPT API key", "https://chatgpt.com/backend-api/codex", false, true},
		{"official API key", "https://api.openai.com/v1", false, true},
		{"official OAuth", "https://API.OPENAI.COM:443/v1", true, true},
		{"compatible API key", "https://compatible.example/v1", false, false},
		{"compatible OAuth", "https://compatible.example/backend-api/codex", true, false},
		{"lookalike host", "https://api.openai.com.example/v1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := codexAPIKeyAuth()
			if tc.oauth {
				auth = codexOAuthAuth()
			}
			edits := &codexBodyEdits{}
			got := prepareCodexResponsesForUpstream(body, body, sdktranslator.FormatOpenAIResponse, auth, tc.base, edits)
			wantRemoved := 0
			if tc.wantClean {
				wantRemoved = 150
			}
			if edits.ReasoningContentsRemoved != wantRemoved {
				t.Fatalf("diagnostic removed=%d, want %d", edits.ReasoningContentsRemoved, wantRemoved)
			}
			prepareCodexResponsesForUpstream(got, body, sdktranslator.FormatOpenAIResponse, auth, tc.base, edits)
			if edits.ReasoningContentsRemoved != wantRemoved {
				t.Fatal("clean history was counted twice")
			}
			if schemaHasReasoningContent(got) == tc.wantClean {
				t.Fatalf("reasoning content normalization does not match destination %s", tc.base)
			}
			if tc.wantClean {
				checkSchemaReasoningHistory(t, got)
			} else if !bytes.Equal(body, got) {
				t.Fatal("compatible provider payload changed")
			}
		})
	}
}

func TestClaudeCacheBudgetPreservesMixedClientAnchors(t *testing.T) {
	// Client uses all four slots across tools/system/messages. Only the
	// gateway-owned identity mark may go; no client content or TTL may change.
	payload := []byte(`{"tools":[{"name":"read","cache_control":{"type":"ephemeral","ttl":"1h"}}],"system":[{"type":"text","text":"` + claudeCodeIdentityText + `","cache_control":{"type":"ephemeral"}},{"type":"text","text":"client","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"one","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"text","text":"two","cache_control":{"type":"ephemeral"}}]}]}`)
	original := bytes.Clone(payload)
	got := limitClaudeCacheControls(payload)
	want, err := sjson.DeleteBytes(payload, "system.0.cache_control")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || !bytes.Equal(payload, original) {
		t.Fatalf("client anchors changed:\n%s", got)
	}
	if twice := limitClaudeCacheControls(got); !bytes.Equal(twice, got) {
		t.Fatal("cache limit not idempotent")
	}
	for _, body := range []string{`{}`, `{"system":"text"}`, `{"cache_control":{"type":"ephemeral"}}`, string(want)} {
		if got := limitClaudeCacheControls([]byte(body)); string(got) != body {
			t.Fatalf("valid budget changed: %s", got)
		}
	}
}

func TestCodexOfficialAPIKeyReasoningContentHTTPRegression(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			auth := codexAPIKeyAuth()
			auth.Attributes["base_url"] = "https://api.openai.com/v1"
			env.Upstream.respond = func(_ int, call codexWireCall) *http.Response {
				if schemaHasReasoningContent(call.Plain(t)) {
					return codexErrorResponse(400, schemaReasoningError)
				}
				return nil
			}
			if _, err := env.Do(t, codexCall{Auth: auth, From: "openai-response", Payload: schemaReasoningHistory(), Stream: stream}); err != nil {
				t.Fatal(err)
			}
			calls := env.Upstream.Calls()
			if len(calls) != 1 {
				t.Fatalf("calls=%d", len(calls))
			}
			checkSchemaReasoningHistory(t, calls[0].Plain(t))
		})
	}
}
