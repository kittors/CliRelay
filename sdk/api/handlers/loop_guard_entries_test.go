package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	"github.com/tidwall/gjson"
)

// streamingExecutor answers a stream with the chunks it is given, in the form
// the client's API takes them, and records what it was asked and how each
// request ended.
type streamingExecutor struct {
	chunks [][]byte
	body   []byte

	mu    sync.Mutex
	calls int
	marks []*loopdetect.Mark
	done  []chan struct{}
}

func (e *streamingExecutor) Identifier() string { return "codex" }

func (e *streamingExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return coreexecutor.Response{Payload: e.body}, nil
}

func (e *streamingExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	e.marks = append(e.marks, loopdetect.MarkFrom(ctx))
	ended := make(chan struct{})
	e.done = append(e.done, ended)
	e.mu.Unlock()

	out := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(ended)
		defer close(out)
		for _, chunk := range e.chunks {
			select {
			case out <- coreexecutor.StreamChunk{Payload: chunk}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &coreexecutor.StreamResult{Chunks: out}, nil
}

func (e *streamingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *streamingExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *streamingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *streamingExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// recordingObserver counts what the handlers report.
type recordingObserver struct {
	mu    sync.Mutex
	cuts  []loopdetect.ReplyCut
	tools []loopdetect.ToolAnswered
}

func (o *recordingObserver) ReplyCut(_ context.Context, cut loopdetect.ReplyCut) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cuts = append(o.cuts, cut)
}

func (o *recordingObserver) ToolLoopAnswered(_ context.Context, answered loopdetect.ToolAnswered) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tools = append(o.tools, answered)
}

// entries serves the four public entry handlers, as the server wires them.
type entries struct {
	router   *gin.Engine
	executor *streamingExecutor
	observer *recordingObserver
	policy   *loopdetect.Policy
}

func newEntries(t *testing.T, executor *streamingExecutor, policy func(*recordingObserver) *loopdetect.Policy) *entries {
	t.Helper()
	return newEntriesWithConfig(t, &sdkconfig.SDKConfig{}, executor, policy)
}

func newEntriesWithConfig(t *testing.T, cfg *sdkconfig.SDKConfig, executor *streamingExecutor, policy func(*recordingObserver) *loopdetect.Policy) *entries {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-loop-guard", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	base := handlers.NewBaseAPIHandlers(cfg, manager)
	e := &entries{router: gin.New(), executor: executor, observer: &recordingObserver{}}
	e.policy = policy(e.observer)
	e.router.Use(func(c *gin.Context) {
		if e.policy != nil {
			c.Set(handlers.LoopGuardGateContextKey, handlers.LoopGuardGate(func() *loopdetect.Policy { return e.policy }))
		}
		c.Next()
	})
	e.router.POST("/v1/messages", claude.NewClaudeCodeAPIHandler(base).ClaudeMessages)
	e.router.POST("/v1/chat/completions", openai.NewOpenAIAPIHandler(base).ChatCompletions)
	e.router.POST("/v1/responses", openai.NewOpenAIResponsesAPIHandler(base).Responses)
	e.router.POST("/v1/responses/compact", openai.NewOpenAIResponsesAPIHandler(base).Compact)
	e.router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(base).GeminiHandler)
	return e
}

func (e *entries) post(path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.router.ServeHTTP(rec, req)
	return rec
}

// loopConversation is an agent's request that ends in n identical turns.
func loopConversation(entry string, n int, stream bool) string {
	var b strings.Builder
	switch entry {
	case "claude":
		b.WriteString(`{"model":"test-model","max_tokens":64,"stream":` + fmt.Sprint(stream) + `,"messages":[{"role":"user","content":"fix it"}`)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `,{"role":"assistant","content":[{"type":"tool_use","id":"toolu_%d","name":"Read","input":{"file_path":"/a.go"}}]}`, i)
			fmt.Fprintf(&b, `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":"package a"}]}`, i)
		}
	case "chat":
		b.WriteString(`{"model":"test-model","stream":` + fmt.Sprint(stream) + `,"messages":[{"role":"user","content":"fix it"}`)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `,{"role":"assistant","content":null,"tool_calls":[{"id":"call_%d","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/a.go\"}"}}]}`, i)
			fmt.Fprintf(&b, `,{"role":"tool","tool_call_id":"call_%d","content":"package a"}`, i)
		}
	case "responses":
		b.WriteString(`{"model":"test-model","stream":` + fmt.Sprint(stream) + `,"input":[{"role":"user","content":"fix it"}`)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `,{"type":"function_call","call_id":"call_%d","name":"Read","arguments":"{\"file_path\":\"/a.go\"}"}`, i)
			fmt.Fprintf(&b, `,{"type":"function_call_output","call_id":"call_%d","output":"package a"}`, i)
		}
		b.WriteString(`]}`)
		return b.String()
	case "gemini":
		b.WriteString(`{"contents":[{"role":"user","parts":[{"text":"fix it"}]}`)
		for i := 0; i < n; i++ {
			b.WriteString(`,{"role":"model","parts":[{"functionCall":{"name":"Read","args":{"file_path":"/a.go"}}}]}`)
			b.WriteString(`,{"role":"user","parts":[{"functionResponse":{"name":"Read","response":{"output":"package a"}}}]}`)
		}
	}
	b.WriteString(`]}`)
	return b.String()
}

func entryPath(entry string, stream bool) string {
	switch entry {
	case "claude":
		return "/v1/messages"
	case "chat":
		return "/v1/chat/completions"
	case "responses":
		return "/v1/responses"
	}
	if stream {
		return "/v1beta/models/test-model:streamGenerateContent"
	}
	return "/v1beta/models/test-model:generateContent"
}

var allEntries = []string{"claude", "chat", "responses", "gemini"}

func toolPolicy(repeats int) func(*recordingObserver) *loopdetect.Policy {
	return func(o *recordingObserver) *loopdetect.Policy {
		return &loopdetect.Policy{Tool: &loopdetect.ToolLimits{Repeats: repeats}, Observer: o}
	}
}

func TestToolLoopIsAnsweredLocallyByEveryEntry(t *testing.T) {
	notice := map[string]string{
		"claude":    "content.0.text",
		"chat":      "choices.0.message.content",
		"responses": "output.0.content.0.text",
		"gemini":    "candidates.0.content.parts.0.text",
	}
	for _, entry := range allEntries {
		t.Run(entry, func(t *testing.T) {
			executor := &streamingExecutor{body: []byte(`{"never":"sent"}`)}
			e := newEntries(t, executor, toolPolicy(8))
			rec := e.post(entryPath(entry, false), loopConversation(entry, 9, false))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if got := gjson.Get(rec.Body.String(), notice[entry]).String(); !strings.Contains(got, "loop protection") || !strings.Contains(got, "Read") {
				t.Fatalf("no notice at %s: %s", notice[entry], rec.Body)
			}
			if rec.Header().Get(handlers.LoopGuardHeader) != string(loopdetect.KindToolLoop) {
				t.Errorf("header %q", rec.Header().Get(handlers.LoopGuardHeader))
			}
			if executor.Calls() != 0 {
				t.Errorf("the request was sent upstream %d times", executor.Calls())
			}
			if len(e.observer.tools) != 1 || e.observer.tools[0].Stream || e.observer.tools[0].Model != "test-model" ||
				e.observer.tools[0].Loop.Turns != 9 || e.observer.tools[0].Limits.Repeats != 8 {
				t.Errorf("observed %+v", e.observer.tools)
			}
		})
	}
}

func TestToolLoopIsAnsweredLocallyAsAStreamByEveryEntry(t *testing.T) {
	for _, entry := range allEntries {
		t.Run(entry, func(t *testing.T) {
			executor := &streamingExecutor{}
			e := newEntries(t, executor, toolPolicy(8))
			rec := e.post(entryPath(entry, true), loopConversation(entry, 9, true))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			out := rec.Body.String()
			if executor.Calls() != 0 {
				t.Errorf("the request was sent upstream %d times", executor.Calls())
			}
			if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
				t.Errorf("content type %q", rec.Header().Get("Content-Type"))
			}
			if rec.Header().Get(handlers.LoopGuardHeader) != string(loopdetect.KindToolLoop) {
				t.Errorf("header %q", rec.Header().Get(handlers.LoopGuardHeader))
			}
			// every event is well formed and the text reaches the client
			var text strings.Builder
			var last string
			for _, line := range strings.Split(out, "\n") {
				data, ok := strings.CutPrefix(line, "data: ")
				if !ok || data == "[DONE]" {
					continue
				}
				if !gjson.Valid(data) {
					t.Fatalf("a data line that is no JSON: %q", line)
				}
				root := gjson.Parse(data)
				for _, path := range []string{"delta.text", "choices.0.delta.content", "delta", "candidates.0.content.parts.0.text"} {
					if v := root.Get(path); v.Type == gjson.String && !strings.HasPrefix(root.Get("type").String(), "response.output_item") &&
						(path != "delta" || root.Get("type").String() == "response.output_text.delta") {
						text.WriteString(v.String())
					}
				}
				last = data
			}
			if !strings.Contains(text.String(), "loop protection") || !strings.Contains(text.String(), "Read") {
				t.Fatalf("no notice in the stream:\n%s", out)
			}
			switch entry {
			case "claude":
				if gjson.Get(last, "type").String() != "message_stop" {
					t.Errorf("the stream does not end with message_stop: %s", last)
				}
			case "responses":
				if gjson.Get(last, "type").String() != "response.completed" {
					t.Errorf("the stream does not end with response.completed: %s", last)
				}
			case "chat":
				if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
					t.Errorf("the stream does not end with [DONE]:\n%s", out)
				}
			}
			if len(e.observer.tools) != 1 || !e.observer.tools[0].Stream {
				t.Errorf("observed %+v", e.observer.tools)
			}
		})
	}
}

func TestToolLoopLeavesOtherRequestsToTheUpstream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy func(*recordingObserver) *loopdetect.Policy
		turns  int
	}{
		{"under the limit", toolPolicy(8), 7},
		{"the check is off", func(o *recordingObserver) *loopdetect.Policy {
			return &loopdetect.Policy{Reply: &loopdetect.ReplyLimits{}, Observer: o}
		}, 20},
		{"no policy", func(*recordingObserver) *loopdetect.Policy { return nil }, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, entry := range allEntries {
				executor := &streamingExecutor{body: []byte(`{"from":"upstream"}`)}
				e := newEntries(t, executor, tc.policy)
				rec := e.post(entryPath(entry, false), loopConversation(entry, tc.turns, false))
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "upstream") {
					t.Errorf("%s: %d %s", entry, rec.Code, rec.Body)
				}
				if executor.Calls() != 1 || len(e.observer.tools) != 0 || rec.Header().Get(handlers.LoopGuardHeader) != "" {
					t.Errorf("%s: calls=%d observed=%d header=%q", entry, executor.Calls(), len(e.observer.tools), rec.Header().Get(handlers.LoopGuardHeader))
				}
			}
		})
	}
}

// A Responses compaction carries the conversation too, but what it asks for is a
// summary of it and no turn of the agent: a stuck conversation is still
// compacted, and the check never answers in its place.
func TestToolLoopLeavesACompactionToTheUpstream(t *testing.T) {
	executor := &streamingExecutor{body: []byte(`{"from":"upstream"}`)}
	e := newEntries(t, executor, toolPolicy(8))

	rec := e.post("/v1/responses/compact", loopConversation("responses", 20, false))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "upstream") {
		t.Fatalf("the compaction was not served by the upstream: %d %s", rec.Code, rec.Body)
	}
	if executor.Calls() != 1 || len(e.observer.tools) != 0 || rec.Header().Get(handlers.LoopGuardHeader) != "" {
		t.Fatalf("calls=%d observed=%d header=%q", executor.Calls(), len(e.observer.tools), rec.Header().Get(handlers.LoopGuardHeader))
	}

	// the same conversation as a turn is the loop it is
	rec = e.post("/v1/responses", loopConversation("responses", 20, false))
	if rec.Header().Get(handlers.LoopGuardHeader) != string(loopdetect.KindToolLoop) || executor.Calls() != 1 {
		t.Fatalf("the turn was not answered by the check: header=%q calls=%d", rec.Header().Get(handlers.LoopGuardHeader), executor.Calls())
	}
}

// The check reads JSON a client wrote. A defect in it costs the request its
// protection and nothing else: the request is served, as with the check off.
func TestToolLoopCheckThatFailsLeavesTheRequestToTheUpstream(t *testing.T) {
	defer handlers.FailToolLoopChecksForTest()()
	for _, entry := range allEntries {
		t.Run(entry, func(t *testing.T) {
			executor := &streamingExecutor{body: []byte(`{"from":"upstream"}`)}
			e := newEntries(t, executor, toolPolicy(8))
			rec := e.post(entryPath(entry, false), loopConversation(entry, 20, false))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "upstream") {
				t.Fatalf("the request was not served: %d %s", rec.Code, rec.Body)
			}
			if executor.Calls() != 1 || len(e.observer.tools) != 0 || rec.Header().Get(handlers.LoopGuardHeader) != "" {
				t.Errorf("calls=%d observed=%d header=%q", executor.Calls(), len(e.observer.tools), rec.Header().Get(handlers.LoopGuardHeader))
			}
		})
	}
}

// A loop's chunks in each API's own form: the model says the same few lines.
func loopChunks(entry string, lines int) [][]byte {
	var chunks [][]byte
	text := []string{"Okay.", "Let me think.", "Hmm.", "Proceeding."}
	for i := 0; i < lines; i++ {
		line := text[i%len(text)] + "\n"
		switch entry {
		case "claude":
			chunks = append(chunks, []byte(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", line)))
		case "chat":
			chunks = append(chunks, []byte(fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q}}]}`, line)))
		case "responses":
			chunks = append(chunks, []byte(fmt.Sprintf("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}", line)))
		case "gemini":
			chunks = append(chunks, []byte(fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]}}]}`, line)))
		}
	}
	return chunks
}

func replyPolicy(limits loopdetect.ReplyLimits) func(*recordingObserver) *loopdetect.Policy {
	return func(o *recordingObserver) *loopdetect.Policy {
		return &loopdetect.Policy{Reply: &limits, Observer: o}
	}
}

// The reply is ended with the entry's own in-stream error, a 4xx the client does
// not retry, quoting what repeated and saying how to turn the check off.
func TestReplyLoopEndsEveryEntrysStreamWithItsOwnError(t *testing.T) {
	for _, mode := range streamModes {
		for _, entry := range allEntries {
			t.Run(mode.name+"/"+entry, func(t *testing.T) {
				replyLoopEndsStream(t, mode.config(), entry)
			})
		}
	}
}

// streamModes are the two forwarders a stream runs through: the default, which
// can move a stream that fails before its first byte to another account, and the
// one a deployment with failover switched off gets.
var streamModes = []streamMode{
	{"with failover", nil},
	{"without failover", new(0)},
}

// streamMode is an SDK configuration of the stream's retry budget; nil retries
// is the default.
type streamMode struct {
	name    string
	retries *int
}

func (m streamMode) config() *sdkconfig.SDKConfig {
	cfg := &sdkconfig.SDKConfig{}
	if m.retries != nil {
		cfg.Streaming.BootstrapRetries = m.retries
	}
	return cfg
}

func replyLoopEndsStream(t *testing.T, cfg *sdkconfig.SDKConfig, entry string) {
	t.Helper()
	executor := &streamingExecutor{chunks: loopChunks(entry, 1000)}
	e := newEntriesWithConfig(t, cfg, executor, replyPolicy(loopdetect.ReplyLimits{}))
	rec := e.post(entryPath(entry, true), loopConversation(entry, 0, true))
	out := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, out)
	}
	// the error event is the last thing the client reads
	var payload string
	switch entry {
	case "claude":
		i := strings.LastIndex(out, "event: error\ndata: ")
		if i < 0 {
			t.Fatalf("no Anthropic error event in\n%s", tail(out))
		}
		payload = strings.TrimSpace(out[i+len("event: error\ndata: "):])
		if gjson.Get(payload, "type").String() != "error" || gjson.Get(payload, "error.type").String() != "invalid_request_error" {
			t.Errorf("error event %s", payload)
		}
		payload = gjson.Get(payload, "error.message").String()
	case "chat":
		i := strings.LastIndex(out, "data: {\"error\"")
		if i < 0 {
			t.Fatalf("no OpenAI error chunk in\n%s", tail(out))
		}
		payload = strings.TrimSpace(strings.TrimPrefix(out[i:], "data: "))
		if gjson.Get(payload, "error.code").String() != loopdetect.ReplyLoopCode || gjson.Get(payload, "error.type").String() != "invalid_request_error" {
			t.Errorf("error chunk %s", payload)
		}
		payload = gjson.Get(payload, "error.message").String()
	case "responses":
		i := strings.LastIndex(out, "event: error\ndata: ")
		if i < 0 {
			t.Fatalf("no Responses error event in\n%s", tail(out))
		}
		payload = strings.TrimSpace(out[i+len("event: error\ndata: "):])
		// the Responses API's error event is flat
		if gjson.Get(payload, "type").String() != "error" || gjson.Get(payload, "code").String() != loopdetect.ReplyLoopCode {
			t.Errorf("error event %s", payload)
		}
		payload = gjson.Get(payload, "message").String()
	case "gemini":
		i := strings.LastIndex(out, "event: error\ndata: ")
		if i < 0 {
			t.Fatalf("no Gemini error event in\n%s", tail(out))
		}
		payload = strings.TrimSpace(out[i+len("event: error\ndata: "):])
		if gjson.Get(payload, "error.code").Int() != 422 {
			t.Errorf("error event %s", payload)
		}
		payload = gjson.Get(payload, "error.message").String()
	}
	for _, want := range []string{"loop protection", `"Okay."`, "Stop repeating replies", "256 lines and sentences"} {
		if !strings.Contains(payload, want) {
			t.Errorf("message %q lacks %q", payload, want)
		}
	}
	// it ended the stream, which the upstream was not allowed to finish
	if got := strings.Count(out, "Let me think."); got > 300 {
		t.Errorf("%d chunks reached the client, the window is 256 lines", got)
	}
	if len(e.observer.cuts) != 1 || e.observer.cuts[0].Model != "test-model" || e.observer.cuts[0].Trip.Window != 256 {
		t.Fatalf("observed %+v", e.observer.cuts)
	}
	// the executor's usage reporter finds the cut on the request's context
	executor.mu.Lock()
	marks, done := executor.marks, executor.done
	executor.mu.Unlock()
	if len(marks) != 1 || marks[0] == nil {
		t.Fatalf("the executor saw no mark: %v", marks)
	}
	cut, ok := marks[0].Cut()
	if !ok || cut.Kind != loopdetect.KindReplyLoop || !strings.Contains(cut.Detail, "loop protection") {
		t.Errorf("mark %+v %v", cut, ok)
	}
	<-done[0] // the upstream request was abandoned, not left to run
}

func tail(s string) string {
	if len(s) > 600 {
		return "…" + s[len(s)-600:]
	}
	return s
}

func TestReplyLoopLeavesOrdinaryRepliesAlone(t *testing.T) {
	for _, entry := range allEntries {
		t.Run(entry, func(t *testing.T) {
			var chunks [][]byte
			for i := 0; i < 600; i++ {
				switch entry {
				case "claude":
					chunks = append(chunks, []byte(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Step %d: check item %d and move on.\\n\"}}\n\n", i, i*3)))
				case "chat":
					chunks = append(chunks, []byte(fmt.Sprintf(`{"id":"c","choices":[{"index":0,"delta":{"content":"Step %d: check item %d and move on.\n"}}]}`, i, i*3)))
				case "responses":
					chunks = append(chunks, []byte(fmt.Sprintf("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Step %d: check item %d and move on.\\n\"}", i, i*3)))
				case "gemini":
					chunks = append(chunks, []byte(fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Step %d: check item %d and move on.\n"}]}}]}`, i, i*3)))
				}
			}
			executor := &streamingExecutor{chunks: chunks}
			e := newEntries(t, executor, replyPolicy(loopdetect.ReplyLimits{}))
			rec := e.post(entryPath(entry, true), loopConversation(entry, 0, true))
			out := rec.Body.String()
			if !strings.Contains(out, "Step 599") || strings.Contains(out, loopdetect.ReplyLoopCode) {
				t.Fatalf("the reply was cut:\n%s", tail(out))
			}
			if len(e.observer.cuts) != 0 {
				t.Errorf("observed %+v", e.observer.cuts)
			}
		})
	}
}

// Without the check on, nothing watches the stream and no mark rides on the context.
func TestReplyLoopIsInertWhenOff(t *testing.T) {
	for name, policy := range map[string]func(*recordingObserver) *loopdetect.Policy{
		"no policy":       func(*recordingObserver) *loopdetect.Policy { return nil },
		"tool check only": toolPolicy(8),
	} {
		t.Run(name, func(t *testing.T) {
			executor := &streamingExecutor{chunks: loopChunks("chat", 1000)}
			e := newEntries(t, executor, policy)
			rec := e.post(entryPath("chat", true), loopConversation("chat", 0, true))
			if strings.Contains(rec.Body.String(), loopdetect.ReplyLoopCode) || strings.Count(rec.Body.String(), "Let me think.") != 250 {
				t.Fatalf("the reply was touched: %d lines", strings.Count(rec.Body.String(), "Let me think."))
			}
			executor.mu.Lock()
			defer executor.mu.Unlock()
			if len(executor.marks) != 1 || executor.marks[0] != nil {
				t.Errorf("a mark rode on the context of a request that is not checked: %v", executor.marks)
			}
		})
	}
}
