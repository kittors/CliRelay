package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loopguard"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	"github.com/tidwall/gjson"
)

const (
	loopGuardE2EModel = "loop-guard-e2e-model"
	// loopingChunk is what a stuck model keeps saying: three lines, over and
	// over, none of them different.
	loopingChunk       = "Let me try that once more.\nOkay, running the command again.\nHmm, the same error.\n"
	loopUpstreamChunks = 400
)

// loopUpstream is an OpenAI-compatible upstream whose reply never stops looping.
type loopUpstream struct {
	server   *httptest.Server
	requests atomic.Int32
	// written is how many chunks of the looping reply were sent, and abandoned
	// says the gateway dropped the connection before the end of it.
	written   atomic.Int32
	abandoned atomic.Bool
}

func newLoopUpstream(t *testing.T) *loopUpstream {
	t.Helper()
	u := &loopUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		content, _ := json.Marshal(loopingChunk)
		for range loopUpstreamChunks {
			select {
			case <-r.Context().Done():
				u.abandoned.Store(true)
				return
			default:
			}
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-loop\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%s},\"finish_reason\":null}]}\n\n", loopGuardE2EModel, content)
			if flusher != nil {
				flusher.Flush()
			}
			u.written.Add(1)
			time.Sleep(time.Millisecond)
		}
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-loop\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":900,\"total_tokens\":907}}\n\n", loopGuardE2EModel)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(u.server.Close)
	return u
}

// newLoopGuardServer assembles the gateway the way the binary does, with the
// loop protection settings store attached, in front of the looping upstream.
func newLoopGuardServer(t *testing.T, keys []proxyconfig.APIKeyEntry) (*Server, *loopUpstream) {
	t.Helper()
	usage.CloseDB()
	// Content is stored, as it is where the diagnostics of a request are read.
	if err := usage.InitDB(filepath.Join(t.TempDir(), "loop-guard.db"), proxyconfig.RequestLogStorageConfig{StoreContent: true}, time.UTC); err != nil {
		t.Fatalf("usage.InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	guardRegistry := loopguard.Default()
	guardRegistry.SetStore(settingsstore.LoopGuardSettings{})
	t.Cleanup(func() { guardRegistry.SetStore(nil) })
	upstream := newLoopUpstream(t)

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"e2e-key"}, APIKeyEntries: keys},
		AuthDir:   t.TempDir(),
	}
	cfg.RequestLogStorage.StoreContent = true
	// A channel group, for the routes that name one in their path.
	cfg.Routing.IncludeDefaultGroup = true
	cfg.Routing.ChannelGroups = []proxyconfig.RoutingChannelGroup{{Name: "team", Match: proxyconfig.ChannelGroupMatch{Channels: []string{"Loop Channel"}}}}
	cfg.SanitizeRouting()
	authManager := auth.NewManager(nil, nil, nil)
	authManager.SetConfig(cfg)
	executor := runtimeexecutor.NewOpenAICompatExecutor("mock-openai", cfg)
	authManager.RegisterExecutor(executor)
	credential := &auth.Auth{
		ID:       "loop-guard-e2e-auth",
		Label:    "Loop Channel",
		Provider: executor.Identifier(),
		Status:   auth.StatusActive,
		Attributes: map[string]string{
			"base_url": upstream.server.URL + "/v1",
			"api_key":  "upstream-test-key",
		},
	}
	if _, err := authManager.Register(context.Background(), credential); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: loopGuardE2EModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })

	// A request logger, as the binary has: it is what puts the per-request
	// diagnostics on the request that the log row's details are read from.
	logsDir := t.TempDir()
	server := NewServer(cfg, authManager, sdkaccess.NewManager(), filepath.Join(t.TempDir(), "config.yaml"),
		WithRequestLoggerFactory(func(*proxyconfig.Config, string) logging.RequestLogger {
			return logging.NewFileRequestLogger(false, logsDir, "", 0)
		}))
	return server, upstream
}

func enableLoopGuard(t *testing.T, mutate func(*loopguard.Settings)) {
	t.Helper()
	if _, err := loopguard.Default().Update(context.Background(), identity.SystemTenantID, func(s *loopguard.Settings) error {
		mutate(s)
		return nil
	}); err != nil {
		t.Fatalf("saving loop protection settings: %v", err)
	}
}

func post(server *Server, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	return recorder
}

const streamingChat = `{"model":"` + loopGuardE2EModel + `","messages":[{"role":"user","content":"fix it"}],"stream":true}`

// stuckConversation is a chat request whose last turns are one tool call
// answered with one result, n times over.
func stuckConversation(turns int, lastResult string) string {
	messages := []map[string]any{{"role": "user", "content": "run the tests"}}
	for i := range turns {
		result := "FAIL: connection refused"
		if i == turns-1 {
			result = lastResult
		}
		id := fmt.Sprintf("call_%d", i)
		messages = append(messages,
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": id, "type": "function", "function": map[string]any{"name": "run_tests", "arguments": `{"path":"./..."}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": result},
		)
	}
	body, _ := json.Marshal(map[string]any{"model": loopGuardE2EModel, "messages": messages})
	return string(body)
}

// guardLog waits for the request log row of the model that loop protection
// ended; the rows are written after the response.
func guardLog(t *testing.T, kind string) usage.LogRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		result, err := usage.QueryLogs(usage.LogQueryParams{Days: 1, Page: 1, Size: 20, Models: []string{loopGuardE2EModel}, GuardKinds: []string{kind}})
		if err == nil && len(result.Items) > 0 {
			return result.Items[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no request log row with guard_kind %q (last error %v)", kind, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// guardDiagnostic reads what the request log kept of the request's diagnostics:
// the loop protection section, which says which check ended it and how.
func guardDiagnostic(t *testing.T, row usage.LogRow) gjson.Result {
	t.Helper()
	part, err := usage.QueryLogContentPart(row.ID, "details")
	if err != nil {
		t.Fatalf("QueryLogContentPart: %v", err)
	}
	section := gjson.Get(part.Content, "diagnostic.loop_guard")
	if !section.Exists() {
		t.Fatalf("the request log details carry no loop_guard diagnostic: %.800s", part.Content)
	}
	return section
}

func guardRows(t *testing.T) int {
	t.Helper()
	// Give a row that is being written a moment to land before saying there is none.
	time.Sleep(300 * time.Millisecond)
	result, err := usage.QueryLogs(usage.LogQueryParams{Days: 1, Page: 1, Size: 20, GuardKinds: []string{"reply_loop", "tool_loop"}})
	if err != nil {
		t.Fatal(err)
	}
	return len(result.Items)
}

func TestLoopGuardEndToEndEndsALoopingReply(t *testing.T) {
	server, upstream := newLoopGuardServer(t, nil)
	enableLoopGuard(t, func(s *loopguard.Settings) {
		s.Reply = loopguard.ReplyRule{Enabled: loopguard.Bool(true), Window: 64, MaxDistinct: 4}
	})

	rec := post(server, "/v1/chat/completions", "e2e-key", streamingChat)
	body := rec.Body.String()

	if !strings.Contains(body, loopdetect.ReplyLoopCode) {
		t.Fatalf("the client was not told the reply was cut; body has %d bytes: %.400s", len(body), body)
	}
	if strings.Contains(body, "[DONE]") && strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatal("the reply ran to its natural end")
	}
	if !strings.Contains(body, "Stop repeating replies") {
		t.Fatalf("the error does not say how to turn the check off: %.600s", body)
	}
	// The upstream was told to stop: the whole point is not to pay for the rest.
	deadline := time.Now().Add(3 * time.Second)
	for !upstream.abandoned.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !upstream.abandoned.Load() {
		t.Fatal("the upstream request was not abandoned when the reply was cut")
	}
	if written := upstream.written.Load(); written >= loopUpstreamChunks/2 {
		t.Fatalf("the upstream wrote %d of %d chunks before it was stopped", written, loopUpstreamChunks)
	}

	row := guardLog(t, "reply_loop")
	if !row.Failed || !row.Streaming {
		t.Fatalf("log row = failed %v streaming %v; a cut reply is a failed stream", row.Failed, row.Streaming)
	}
	// The diagnostics say which check, at which level, with which thresholds,
	// and where in the reply it was cut.
	d := guardDiagnostic(t, row)
	if d.Get("kind").String() != "reply_loop" || d.Get("source").String() != "tenant" ||
		d.Get("window").Int() != 64 || d.Get("max_distinct").Int() != 4 ||
		d.Get("cut_at_chunk").Int() <= 0 || d.Get("cut_at_bytes").Int() <= 0 || d.Get("distinct").Int() != 3 ||
		!strings.Contains(d.Get("units.0").String(), "again") && !strings.Contains(d.Get("units.0").String(), "once more") && !strings.Contains(d.Get("units.0").String(), "same error") {
		t.Fatalf("diagnostic = %s", d.Raw)
	}
	// 64 units at three lines a chunk: the 22nd chunk fills the window, and that
	// chunk is not forwarded. The upstream may have written a few more before the
	// cancellation reached it, but never fewer than the cut was made at.
	if cut := d.Get("cut_at_chunk").Int(); cut != 22 || cut > int64(upstream.written.Load()) {
		t.Fatalf("cut at chunk %d with %d written by the upstream, want the 22nd chunk", cut, upstream.written.Load())
	}
}

func TestLoopGuardEndToEndIsOffUntilATenantTurnsItOn(t *testing.T) {
	server, upstream := newLoopGuardServer(t, nil)

	rec := post(server, "/v1/chat/completions", "e2e-key", streamingChat)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "[DONE]") || strings.Contains(body, loopdetect.ReplyLoopCode) {
		t.Fatalf("a looping reply was touched with nothing switched on: status %d, %d bytes", rec.Code, len(body))
	}
	if got := strings.Count(body, "data: {"); got < loopUpstreamChunks {
		t.Fatalf("only %d of %d chunks reached the client", got, loopUpstreamChunks)
	}

	before := upstream.requests.Load()
	rec = post(server, "/v1/chat/completions", "e2e-key", stuckConversation(12, "FAIL: connection refused"))
	if rec.Header().Get("X-CliRelay-Loop-Guard") != "" {
		t.Fatal("a tool loop was answered by the gateway with nothing switched on")
	}
	if upstream.requests.Load() != before+1 {
		t.Fatalf("the request did not reach the upstream: %d -> %d", before, upstream.requests.Load())
	}
	if n := guardRows(t); n != 0 {
		t.Fatalf("%d requests were filed as ended by loop protection with nothing switched on", n)
	}
}

func TestLoopGuardEndToEndAnswersAToolLoopWithoutAnUpstream(t *testing.T) {
	server, upstream := newLoopGuardServer(t, nil)
	enableLoopGuard(t, func(s *loopguard.Settings) {
		s.Tool = loopguard.ToolRule{Enabled: loopguard.Bool(true), Repeats: 3}
	})

	rec := post(server, "/v1/chat/completions", "e2e-key", stuckConversation(5, "FAIL: connection refused"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %.400s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-CliRelay-Loop-Guard"); got != "tool_loop" {
		t.Fatalf("X-CliRelay-Loop-Guard = %q", got)
	}
	var reply struct {
		Choices []struct {
			Message      struct{ Content string }
			FinishReason string `json:"finish_reason"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || len(reply.Choices) != 1 {
		t.Fatalf("not a chat completion: %v: %.400s", err, rec.Body.String())
	}
	if reply.Choices[0].FinishReason != "stop" || !strings.Contains(reply.Choices[0].Message.Content, "run_tests") || !strings.Contains(reply.Choices[0].Message.Content, "Stop tool-call loops") {
		t.Fatalf("the answer does not end the turn and name the loop: %+v", reply.Choices[0])
	}
	if upstream.requests.Load() != 0 {
		t.Fatalf("the upstream was asked %d times for a request answered locally", upstream.requests.Load())
	}

	row := guardLog(t, "tool_loop")
	if !row.Failed {
		t.Fatal("a request answered by the gateway is filed as a success")
	}
	d := guardDiagnostic(t, row)
	if d.Get("kind").String() != "tool_loop" || d.Get("source").String() != "tenant" || d.Get("repeats").Int() != 3 ||
		d.Get("turns").Int() != 5 || d.Get("calls.0.name").String() != "run_tests" || d.Get("conversation_items").Int() != 11 {
		t.Fatalf("diagnostic = %s", d.Raw)
	}
	if row.ChannelName != "" || row.AuthIndex != "" || row.Source == "e2e-key" {
		t.Fatalf("log row names an account or the caller's secret: channel %q auth %q source %q", row.ChannelName, row.AuthIndex, row.Source)
	}

	// Progress is not a loop: the same call with a result that changed goes through.
	rec = post(server, "/v1/chat/completions", "e2e-key", stuckConversation(5, "ok: 12 tests passed"))
	if rec.Header().Get("X-CliRelay-Loop-Guard") != "" || upstream.requests.Load() != 1 {
		t.Fatalf("a turn that got a different result was held back (requests %d)", upstream.requests.Load())
	}
}

// stuckGeminiConversation is a Gemini request whose last turns are one function
// call answered with one result, turns times over.
func stuckGeminiConversation(turns int) string {
	contents := []map[string]any{{"role": "user", "parts": []any{map[string]any{"text": "run the tests"}}}}
	for range turns {
		contents = append(contents,
			map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "run_tests", "args": map[string]any{"path": "./..."}}}}},
			map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "run_tests", "response": map[string]any{"output": "FAIL: connection refused"}}}}},
		)
	}
	body, _ := json.Marshal(map[string]any{"contents": contents})
	return string(body)
}

// stuckResponsesConversation is the same loop as a Responses request.
func stuckResponsesConversation(turns int) string {
	input := []map[string]any{{"role": "user", "content": "run the tests"}}
	for i := range turns {
		id := fmt.Sprintf("call_%d", i)
		input = append(input,
			map[string]any{"type": "function_call", "call_id": id, "name": "run_tests", "arguments": `{"path":"./..."}`},
			map[string]any{"type": "function_call_output", "call_id": id, "output": "FAIL: connection refused"},
		)
	}
	body, _ := json.Marshal(map[string]any{"model": loopGuardE2EModel, "input": input})
	return string(body)
}

// Every way in to the inference handlers runs the loop protection, in the API
// its client speaks: a route group that was left out would serve a stuck agent
// at the upstream's expense with the check switched on.
func TestLoopGuardEndToEndCoversEveryInferenceRoute(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, notice string
	}{
		{"chat completions", "/v1/chat/completions", stuckConversation(5, "FAIL: connection refused"), "choices.0.message.content"},
		{"gemini", "/v1beta/models/" + loopGuardE2EModel + ":generateContent", stuckGeminiConversation(5), "candidates.0.content.parts.0.text"},
		{"codex direct responses", "/backend-api/codex/responses", stuckResponsesConversation(5), "output.0.content.0.text"},
		{"grouped chat completions", "/team/v1/chat/completions", stuckConversation(5, "FAIL: connection refused"), "choices.0.message.content"},
		{"grouped gemini", "/team/v1beta/models/" + loopGuardE2EModel + ":generateContent", stuckGeminiConversation(5), "candidates.0.content.parts.0.text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, upstream := newLoopGuardServer(t, nil)
			enableLoopGuard(t, func(s *loopguard.Settings) {
				s.Tool = loopguard.ToolRule{Enabled: loopguard.Bool(true), Repeats: 3}
			})
			rec := post(server, tc.path, "e2e-key", tc.body)
			if got := rec.Header().Get("X-CliRelay-Loop-Guard"); got != "tool_loop" || upstream.requests.Load() != 0 {
				t.Fatalf("header %q, upstream requests %d, status %d, body %.300s", got, upstream.requests.Load(), rec.Code, rec.Body.String())
			}
			if notice := gjson.Get(rec.Body.String(), tc.notice).String(); !strings.Contains(notice, "run_tests") {
				t.Fatalf("no notice at %s: %.300s", tc.notice, rec.Body.String())
			}
		})
	}
}

// The Amp provider routes are a second way in to the same handlers.
func TestLoopGuardEndToEndCoversTheAmpProviderRoutes(t *testing.T) {
	server, upstream := newLoopGuardServer(t, nil)
	enableLoopGuard(t, func(s *loopguard.Settings) {
		s.Tool = loopguard.ToolRule{Enabled: loopguard.Bool(true), Repeats: 3}
	})

	rec := post(server, "/api/provider/openai/v1/chat/completions", "e2e-key", stuckConversation(5, "FAIL: connection refused"))
	if got := rec.Header().Get("X-CliRelay-Loop-Guard"); got != "tool_loop" || upstream.requests.Load() != 0 {
		t.Fatalf("Amp route: header %q, upstream requests %d, status %d, body %.300s", got, upstream.requests.Load(), rec.Code, rec.Body.String())
	}
}

func TestLoopGuardEndToEndAPIKeyOverrides(t *testing.T) {
	keys := []proxyconfig.APIKeyEntry{
		{ID: "key-plain", Key: "plain-key", Name: "Plain"},
		{ID: "key-exempt", Key: "exempt-key", Name: "Exempt"},
		{ID: "key-optin", Key: "optin-key", Name: "Opt in"},
	}
	loop := stuckConversation(5, "FAIL: connection refused")
	answered := func(rec *httptest.ResponseRecorder) bool {
		return rec.Header().Get("X-CliRelay-Loop-Guard") == "tool_loop"
	}

	t.Run("a key can be exempted from a tenant that has the check on", func(t *testing.T) {
		server, upstream := newLoopGuardServer(t, keys)
		enableLoopGuard(t, func(s *loopguard.Settings) {
			s.Tool = loopguard.ToolRule{Enabled: loopguard.Bool(true), Repeats: 3}
			s.Keys = map[string]loopguard.Scope{"key-exempt": {Tool: loopguard.ToolRule{Enabled: loopguard.Bool(false)}}}
		})
		if rec := post(server, "/v1/chat/completions", "plain-key", loop); !answered(rec) {
			t.Fatalf("the tenant's setting did not apply to a key with no override: %d %.200s", rec.Code, rec.Body.String())
		}
		if rec := post(server, "/v1/chat/completions", "exempt-key", loop); answered(rec) || upstream.requests.Load() != 1 {
			t.Fatalf("an exempted key was still checked (upstream requests %d)", upstream.requests.Load())
		}
	})

	t.Run("a key can opt in under a tenant that has the check off", func(t *testing.T) {
		server, upstream := newLoopGuardServer(t, keys)
		enableLoopGuard(t, func(s *loopguard.Settings) {
			s.Keys = map[string]loopguard.Scope{"key-optin": {Tool: loopguard.ToolRule{Enabled: loopguard.Bool(true), Repeats: 4}}}
		})
		if rec := post(server, "/v1/chat/completions", "optin-key", loop); !answered(rec) {
			t.Fatalf("a key that opted in was not checked: %d %.200s", rec.Code, rec.Body.String())
		}
		if rec := post(server, "/v1/chat/completions", "plain-key", loop); answered(rec) || upstream.requests.Load() != 1 {
			t.Fatalf("a key that did not opt in was checked (upstream requests %d)", upstream.requests.Load())
		}
		// The key's own threshold applies: three stuck turns are fewer than its four.
		if rec := post(server, "/v1/chat/completions", "optin-key", stuckConversation(3, "FAIL: connection refused")); answered(rec) {
			t.Fatal("the key's own threshold was not applied")
		}
	})
}

// A setting saved while the gateway runs applies to the next request, with no
// restart and no wait for the cache.
func TestLoopGuardEndToEndSettingsApplyAtOnceOnThisNode(t *testing.T) {
	server, _ := newLoopGuardServer(t, nil)
	loop := stuckConversation(5, "FAIL: connection refused")

	if rec := post(server, "/v1/chat/completions", "e2e-key", loop); rec.Header().Get("X-CliRelay-Loop-Guard") != "" {
		t.Fatal("answered with nothing switched on")
	}
	enableLoopGuard(t, func(s *loopguard.Settings) { s.Tool = loopguard.ToolRule{Enabled: loopguard.Bool(true), Repeats: 3} })
	if rec := post(server, "/v1/chat/completions", "e2e-key", loop); rec.Header().Get("X-CliRelay-Loop-Guard") != "tool_loop" {
		t.Fatal("a saved setting did not apply to the next request")
	}
	enableLoopGuard(t, func(s *loopguard.Settings) { s.Tool.Enabled = loopguard.Bool(false) })
	if rec := post(server, "/v1/chat/completions", "e2e-key", loop); rec.Header().Get("X-CliRelay-Loop-Guard") != "" {
		t.Fatal("switching the check off did not apply to the next request")
	}
}
