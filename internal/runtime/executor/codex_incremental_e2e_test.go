package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// incUpstream fakes the ChatGPT Codex backend on both transports. POST answers
// with SSE; a websocket keeps only the last response it produced, which is all
// a continuation may name (store=false), as the backend does.
type incUpstream struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	conns      int
	ws         []incWSMessage
	httpBodies []string
	replies    [][]string
	served     int

	pongs     atomic.Int64
	ids       atomic.Int64
	forget    atomic.Bool
	closeNext atomic.Bool
	errorNext atomic.Pointer[string]
	// stall sends only response.created for the next turn and then waits for
	// the relay to give up on the socket.
	stall      atomic.Bool
	readErrors atomic.Int64
	handshake  int
	pingEvery  time.Duration
}

type incWSMessage struct {
	conn int
	raw  string
}

func newIncUpstream(t *testing.T, replies ...[]string) *incUpstream {
	u := &incUpstream{t: t, replies: replies}
	u.server = httptest.NewServer(u)
	t.Cleanup(u.server.Close)
	return u
}

func (u *incUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		u.serveWebsocket(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.httpBodies = append(u.httpBodies, string(body))
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range u.nextEvents() {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(event, "type").String(), event)
	}
}

func (u *incUpstream) serveWebsocket(w http.ResponseWriter, r *http.Request) {
	if u.handshake != 0 {
		http.Error(w, `{"error":{"message":"no"}}`, u.handshake)
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	u.mu.Lock()
	u.conns++
	index := u.conns
	u.mu.Unlock()
	conn.SetPongHandler(func(string) error { u.pongs.Add(1); return nil })
	done := make(chan struct{})
	defer close(done)
	if u.pingEvery > 0 {
		go func() {
			ticker := time.NewTicker(u.pingEvery)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					_ = conn.WriteControl(websocket.PingMessage, []byte("p"), time.Now().Add(time.Second))
				}
			}
		}()
	}
	last := ""
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			u.readErrors.Add(1)
			return
		}
		u.mu.Lock()
		u.ws = append(u.ws, incWSMessage{conn: index, raw: string(msg)})
		u.mu.Unlock()
		if gjson.GetBytes(msg, "type").String() != "response.create" {
			u.t.Errorf("upstream got %s, want response.create", gjson.GetBytes(msg, "type").String())
			return
		}
		if prev := gjson.GetBytes(msg, "previous_response_id").String(); prev != "" && (prev != last || u.forget.Swap(false)) {
			last = ""
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response with id '`+prev+`' not found."}}`))
			continue
		}
		if event := u.errorNext.Swap(nil); event != nil {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(*event))
			continue
		}
		events := u.nextEvents()
		if u.stall.Swap(false) {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(events[0]))
			continue
		}
		for _, event := range events {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
		}
		last = gjson.Get(events[len(events)-1], "response.id").String()
		if u.closeNext.Swap(false) {
			return
		}
	}
}

// nextEvents streams the next reply the way the backend does: text as deltas,
// arguments as deltas, then the final event carrying the whole output.
func (u *incUpstream) nextEvents() []string {
	u.mu.Lock()
	output := u.replies[u.served%len(u.replies)]
	u.served++
	u.mu.Unlock()
	id := fmt.Sprintf("resp_%d", u.ids.Add(1))
	events := []string{`{"type":"response.created","response":{"id":"` + id + `","object":"response","status":"in_progress","output":[]}}`}
	for i, item := range output {
		switch gjson.Get(item, "type").String() {
		case "message":
			added, _ := sjson.SetRaw(item, "content", `[]`)
			events = append(events, fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":%s}`, i, added))
			delta, _ := sjson.Set(`{"type":"response.output_text.delta","content_index":0}`, "delta", gjson.Get(item, "content.0.text").String())
			delta, _ = sjson.Set(delta, "output_index", i)
			events = append(events, delta)
		case "function_call":
			added, _ := sjson.Set(item, "arguments", "")
			events = append(events, fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":%s}`, i, added))
			delta, _ := sjson.Set(`{"type":"response.function_call_arguments.delta"}`, "delta", gjson.Get(item, "arguments").String())
			delta, _ = sjson.Set(delta, "output_index", i)
			events = append(events, delta)
		}
		events = append(events, fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, i, item))
	}
	events = append(events, `{"type":"response.completed","response":{"id":"`+id+`","object":"response","status":"completed","output":[`+strings.Join(output, ",")+`],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`)
	return events
}

func (u *incUpstream) websocketMessages() []incWSMessage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]incWSMessage(nil), u.ws...)
}

func (u *incUpstream) counts() (conns, wsMessages, httpRequests int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conns, len(u.ws), len(u.httpBodies)
}

func (u *incUpstream) auth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         "codex-incremental-auth",
		Provider:   "codex",
		Status:     cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": u.server.URL + "/backend-api/codex"},
		Metadata:   map[string]any{"access_token": "token", "account_id": "account-1"},
	}
}

func newIncExecutor(t *testing.T, inc config.CodexUpstreamIncrementalConfig) (*CodexExecutor, *codexIncrementalPool) {
	exec := NewCodexExecutor(&config.Config{CodexUpstreamIncremental: inc})
	exec.incremental = newCodexIncrementalPool()
	t.Cleanup(exec.incremental.closeAll)
	return exec, exec.incremental
}

func incCtx() context.Context {
	return context.WithValue(context.Background(), util.ContextKeyAPIKey, "sk-incremental-test")
}

const (
	incChatTools  = `[{"type":"function","function":{"name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]`
	incReplyCall  = `{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}`
	incReplyText1 = `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Checking.","annotations":[]}]}`
	incReplyText2 = `{"type":"message","id":"msg_2","status":"completed","role":"assistant","content":[{"type":"output_text","text":"There is a.txt.","annotations":[]}]}`
	incReplyText3 = `{"type":"message","id":"msg_3","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Bye.","annotations":[]}]}`
)

// chatTurn is what a Chat client got back: the assistant message it sends on
// with the next turn.
type chatTurn struct {
	content string
	calls   []string
}

func (c chatTurn) message() string {
	msg := `{"role":"assistant","content":""}`
	msg, _ = sjson.Set(msg, "content", c.content)
	for _, call := range c.calls {
		msg, _ = sjson.SetRaw(msg, "tool_calls.-1", call)
	}
	return msg
}

func runChatTurn(t *testing.T, exec *CodexExecutor, auth *cliproxyauth.Auth, messages []string) chatTurn {
	t.Helper()
	payload := `{"model":"gpt-5.4","stream":true,"messages":[` + strings.Join(messages, ",") + `],"tools":` + incChatTools + `}`
	format := sdktranslator.FromString("openai")
	stream, err := exec.ExecuteStream(incCtx(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(payload), Format: format}, cliproxyexecutor.Options{SourceFormat: format, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var turn chatTurn
	calls := map[int64]string{}
	var order []int64
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		for _, line := range strings.Split(string(chunk.Payload), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
			if !gjson.Valid(line) {
				continue
			}
			delta := gjson.Get(line, "choices.0.delta")
			turn.content += delta.Get("content").String()
			delta.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
				index := tc.Get("index").Int()
				call, seen := calls[index]
				if !seen {
					call = `{"type":"function","function":{"name":"","arguments":""}}`
					order = append(order, index)
				}
				if id := tc.Get("id").String(); id != "" {
					call, _ = sjson.Set(call, "id", id)
				}
				if name := tc.Get("function.name").String(); name != "" {
					call, _ = sjson.Set(call, "function.name", name)
				}
				call, _ = sjson.Set(call, "function.arguments", gjson.Get(call, "function.arguments").String()+tc.Get("function.arguments").String())
				calls[index] = call
				return true
			})
		}
	}
	for _, index := range order {
		turn.calls = append(turn.calls, calls[index])
	}
	return turn
}

func incChatConversation(t *testing.T, exec *CodexExecutor, auth *cliproxyauth.Auth, between func(turn int)) []chatTurn {
	t.Helper()
	messages := []string{`{"role":"system","content":"be brief"}`, `{"role":"user","content":"look at the screen"}`}
	first := runChatTurn(t, exec, auth, messages)
	between(1)
	messages = append(messages, first.message(), `{"role":"tool","tool_call_id":"call_1","content":"a.txt"}`)
	second := runChatTurn(t, exec, auth, messages)
	between(2)
	messages = append(messages, second.message(), `{"role":"user","content":"thanks"}`)
	third := runChatTurn(t, exec, auth, messages)
	return []chatTurn{first, second, third}
}

func TestCodexIncrementalChatTurnsGoAsIncrements(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReasoning, incReplyText1, incReplyCall}, []string{incReplyText2}, []string{incReplyText3})
	upstream.pingEvery = 20 * time.Millisecond
	exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})

	turns := incChatConversation(t, exec, upstream.auth(), func(int) {
		// Idle for several pings between turns: the connection must answer
		// them to stay open, as the backend closes a socket that does not.
		time.Sleep(120 * time.Millisecond)
	})
	if turns[0].content != "Checking." || len(turns[0].calls) != 1 || turns[1].content != "There is a.txt." || turns[2].content != "Bye." {
		t.Fatalf("client turns = %+v", turns)
	}

	msgs := upstream.websocketMessages()
	if conns, ws, httpRequests := upstream.counts(); conns != 1 || ws != 3 || httpRequests != 0 {
		t.Fatalf("upstream conns=%d websocket messages=%d http=%d, want one connection carrying all three turns", conns, ws, httpRequests)
	}
	if gjson.Get(msgs[0].raw, "previous_response_id").Exists() || len(gjson.Get(msgs[0].raw, "input").Array()) != 2 {
		t.Fatalf("first turn = %s, want the whole input", msgs[0].raw)
	}
	second := gjson.Parse(msgs[1].raw)
	if second.Get("previous_response_id").String() != "resp_1" || second.Get("input").Raw != `[{"type":"function_call_output","call_id":"call_1","output":"a.txt"}]` {
		t.Fatalf("second turn = %s, want only the tool output on top of resp_1", msgs[1].raw)
	}
	third := gjson.Parse(msgs[2].raw)
	if third.Get("previous_response_id").String() != "resp_2" || len(third.Get("input").Array()) != 1 || third.Get("input.0.role").String() != "user" {
		t.Fatalf("third turn = %s, want only the new user message on top of resp_2", msgs[2].raw)
	}
	if !second.Get("tools").Exists() || second.Get("prompt_cache_key").String() == "" || second.Get("prompt_cache_key").String() != gjson.Get(msgs[0].raw, "prompt_cache_key").String() {
		t.Fatalf("second turn lost request properties: %s", msgs[1].raw)
	}
	if upstream.pongs.Load() == 0 {
		t.Fatal("the connection answered no ping")
	}
	if got := pool.stats.byMode["on"]; got == nil || got.hits != 2 || got.turns != 3 || got.websocket != 3 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestCodexIncrementalResendsInFullWhenTheBaseIsGone(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReasoning, incReplyText1, incReplyCall}, []string{incReplyText2}, []string{incReplyText3})
	exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})

	turns := incChatConversation(t, exec, upstream.auth(), func(turn int) {
		if turn == 1 {
			upstream.forget.Store(true)
		}
	})
	if turns[1].content != "There is a.txt." || turns[2].content != "Bye." {
		t.Fatalf("client turns = %+v, want the replies as if nothing happened", turns)
	}
	msgs := upstream.websocketMessages()
	if len(msgs) != 4 {
		t.Fatalf("websocket messages = %d, want 4 (refused increment, then in full)", len(msgs))
	}
	if gjson.Get(msgs[1].raw, "previous_response_id").String() != "resp_1" {
		t.Fatalf("second turn did not try the increment first: %s", msgs[1].raw)
	}
	retry := gjson.Parse(msgs[2].raw)
	if retry.Get("previous_response_id").Exists() || len(retry.Get("input").Array()) != 5 || msgs[2].conn != msgs[1].conn {
		t.Fatalf("retry = %s on conn %d, want the whole input on the same connection", msgs[2].raw, msgs[2].conn)
	}
	// The chain rebuilt itself on the retry: the next turn is an increment again.
	if gjson.Get(msgs[3].raw, "previous_response_id").String() != "resp_2" || len(gjson.Get(msgs[3].raw, "input").Array()) != 1 {
		t.Fatalf("third turn = %s, want an increment on the rebuilt chain", msgs[3].raw)
	}
	if got := pool.stats.byMode["on"]; got == nil || got.hits != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestCodexIncrementalReconnectsAfterTheBackendClosedTheSocket(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReasoning, incReplyText1, incReplyCall}, []string{incReplyText2}, []string{incReplyText3})
	upstream.closeNext.Store(true)
	exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})

	turns := incChatConversation(t, exec, upstream.auth(), func(turn int) {
		if turn == 1 {
			// Let the reader notice the close before the next turn.
			time.Sleep(50 * time.Millisecond)
		}
	})
	if turns[1].content != "There is a.txt." {
		t.Fatalf("client turns = %+v", turns)
	}
	msgs := upstream.websocketMessages()
	if conns, _, httpRequests := upstream.counts(); conns != 2 || httpRequests != 0 {
		t.Fatalf("conns=%d http=%d, want a second connection and no HTTP", conns, httpRequests)
	}
	if gjson.Get(msgs[1].raw, "previous_response_id").Exists() || msgs[1].conn != 2 {
		t.Fatalf("second turn = %s on conn %d, want the whole input on a new connection", msgs[1].raw, msgs[1].conn)
	}
	if gjson.Get(msgs[2].raw, "previous_response_id").String() != "resp_2" {
		t.Fatalf("third turn = %s, want an increment again", msgs[2].raw)
	}
}

// runResponsesExecute runs two non-streaming Responses turns, the second
// sending the first reply back the way the official client does.
func runResponsesExecute(t *testing.T, exec *CodexExecutor, auth *cliproxyauth.Auth) (first, second []byte) {
	t.Helper()
	format := sdktranslator.FromString("openai-response")
	user1 := `{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`
	run := func(input ...string) []byte {
		payload := `{"model":"gpt-5.4","prompt_cache_key":"conv-1","input":[` + strings.Join(input, ",") + `]}`
		resp, err := exec.Execute(incCtx(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(payload), Format: format}, cliproxyexecutor.Options{SourceFormat: format})
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		return resp.Payload
	}
	first = run(user1)
	input := []string{user1}
	for _, item := range gjson.GetBytes(first, "output").Array() {
		input = append(input, item.Raw)
	}
	second = run(append(input, `{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}`)...)
	return first, second
}

func TestCodexIncrementalShadowLeavesTheTransportAlone(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReasoning, incReplyText1}, []string{incReplyText2})
	exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "shadow"})
	runResponsesExecute(t, exec, upstream.auth())

	if conns, ws, httpRequests := upstream.counts(); conns != 0 || ws != 0 || httpRequests != 2 {
		t.Fatalf("conns=%d ws=%d http=%d, want both turns over HTTP", conns, ws, httpRequests)
	}
	got := pool.stats.byMode["shadow"]
	if got == nil || got.turns != 2 || got.hits != 1 || got.sentBytes >= got.fullBytes {
		t.Fatalf("shadow stats = %+v, want the second turn counted as an increment", got)
	}
}

func TestCodexIncrementalNonStreamingTurns(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReasoning, incReplyText1}, []string{incReplyText2})
	exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
	_, second := runResponsesExecute(t, exec, upstream.auth())
	if got := gjson.GetBytes(second, "output.0.content.0.text").String(); got != "There is a.txt." {
		t.Fatalf("second reply = %s", second)
	}
	msgs := upstream.websocketMessages()
	if len(msgs) != 2 || gjson.Get(msgs[1].raw, "previous_response_id").String() != "resp_1" || len(gjson.Get(msgs[1].raw, "input").Array()) != 1 {
		t.Fatalf("upstream messages = %+v, want the second turn as an increment", msgs)
	}
}

func TestCodexIncrementalOnlyForAllowedKeys(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReplyText1})
	other := sha256.Sum256([]byte("sk-someone-else"))
	exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on", AllowKeys: []string{"sha256:" + hex.EncodeToString(other[:])}})
	runChatTurn(t, exec, upstream.auth(), []string{`{"role":"user","content":"hi"}`})
	if conns, _, httpRequests := upstream.counts(); conns != 0 || httpRequests != 1 {
		t.Fatalf("conns=%d http=%d, want HTTP for a key not on the list", conns, httpRequests)
	}
	if pool.stats.byMode["shadow"] == nil || pool.stats.byMode["on"] != nil {
		t.Fatalf("stats = %+v, want the request counted in shadow mode", pool.stats.byMode)
	}

	digest := sha256.Sum256([]byte("sk-incremental-test"))
	allowed, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on", AllowKeys: []string{"sha256:" + hex.EncodeToString(digest[:])}})
	runChatTurn(t, allowed, upstream.auth(), []string{`{"role":"user","content":"hi"}`})
	if conns, _, _ := upstream.counts(); conns != 1 {
		t.Fatalf("conns=%d, want the allowed key on the websocket", conns)
	}
}

func TestCodexIncrementalPassesOnARefusal(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReplyText1})
	refusal := `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"},"headers":{"retry-after":"30"}}`
	upstream.errorNext.Store(&refusal)
	exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
	format := sdktranslator.FromString("openai")
	_, err := exec.ExecuteStream(incCtx(), upstream.auth(), cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}`), Format: format}, cliproxyexecutor.Options{SourceFormat: format, Stream: true})
	var status interface{ StatusCode() int }
	if err == nil || !errors.As(err, &status) || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("ExecuteStream() error = %v, want the 429 the backend answered", err)
	}
	if _, _, httpRequests := upstream.counts(); httpRequests != 0 {
		t.Fatalf("a refusal was sent again over HTTP (%d requests)", httpRequests)
	}
}

func TestCodexIncrementalFallsBackToHTTP(t *testing.T) {
	t.Run("handshake refused", func(t *testing.T) {
		upstream := newIncUpstream(t, []string{incReplyText1})
		upstream.handshake = http.StatusForbidden
		exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
		if turn := runChatTurn(t, exec, upstream.auth(), []string{`{"role":"user","content":"hi"}`}); turn.content != "Checking." {
			t.Fatalf("turn = %+v", turn)
		}
		if _, _, httpRequests := upstream.counts(); httpRequests != 1 {
			t.Fatalf("http = %d, want the turn sent over HTTP", httpRequests)
		}
		if got := pool.stats.byMode["on"]; got == nil || got.fallback != 1 || got.reasons["fallback:handshake_403"] != 1 {
			t.Fatalf("stats = %+v", got)
		}
	})
	t.Run("replayed reasoning refused", func(t *testing.T) {
		upstream := newIncUpstream(t, []string{incReplyText1})
		refusal := `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be decrypted."}}`
		upstream.errorNext.Store(&refusal)
		exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
		if turn := runChatTurn(t, exec, upstream.auth(), []string{`{"role":"user","content":"hi"}`}); turn.content != "Checking." {
			t.Fatalf("turn = %+v, want the reply HTTP got", turn)
		}
		if _, _, httpRequests := upstream.counts(); httpRequests != 1 {
			t.Fatalf("http = %d, want the HTTP path to take the turn", httpRequests)
		}
	})
	t.Run("first request too large", func(t *testing.T) {
		upstream := newIncUpstream(t, []string{incReplyText1})
		exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on", MaxFirstRequestMB: 1})
		big := `{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}`
		runChatTurn(t, exec, upstream.auth(), []string{big})
		if conns, _, httpRequests := upstream.counts(); conns != 0 || httpRequests != 1 {
			t.Fatalf("conns=%d http=%d, want a large first request over HTTP without a socket", conns, httpRequests)
		}
	})
}
