package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	"github.com/tidwall/gjson"
)

// The loop guard's gate is published on the upgrade request and resolved by the
// frame loop for every turn: a Responses WebSocket serves turns for hours, and a
// setting changed while it is open applies from the next turn on.

type wsLoopExecutor struct {
	provider string
	calls    atomic.Int64
	// loop makes the turn's reply a loop; otherwise it completes at once.
	loop atomic.Bool
}

func (e *wsLoopExecutor) Identifier() string { return e.provider }

func (e *wsLoopExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsLoopExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls.Add(1)
	out := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(out)
		send := func(payload string) bool {
			select {
			case out <- coreexecutor.StreamChunk{Payload: []byte(payload)}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if e.loop.Load() {
			lines := []string{"Okay.", "Let me think.", "Hmm.", "Proceeding."}
			for i := 0; i < 1000; i++ {
				if !send(fmt.Sprintf("data: {\"type\":\"response.output_text.delta\",\"delta\":%q}", lines[i%len(lines)]+"\n")) {
					return
				}
			}
		}
		send(`data: {"type":"response.completed","response":{"id":"resp_ws_loop","output":[]}}`)
	}()
	return &coreexecutor.StreamResult{Chunks: out}, nil
}

func (e *wsLoopExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *wsLoopExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsLoopExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

type wsLoopObserver struct {
	mu    sync.Mutex
	cuts  int
	tools int
}

func (o *wsLoopObserver) ReplyCut(context.Context, loopdetect.ReplyCut) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cuts++
}

func (o *wsLoopObserver) ToolLoopAnswered(context.Context, loopdetect.ToolAnswered) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tools++
}

func (o *wsLoopObserver) counts() (cuts, tools int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cuts, o.tools
}

func newWSLoopServer(t *testing.T, exec *wsLoopExecutor, gate handlers.LoopGuardGate) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: "auth-" + exec.provider, Provider: exec.provider, Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, exec.provider, []*registry.ModelInfo{{ID: wsTurnModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	engine := gin.New()
	engine.GET("/v1/responses", func(c *gin.Context) {
		c.Set(handlers.LoopGuardGateContextKey, gate)
		h.ResponsesWebsocket(c)
	})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readTurn reads the events of one turn, up to response.done or an error.
func readTurn(t *testing.T, conn *websocket.Conn) []string {
	t.Helper()
	var events []string
	for {
		frame := string(wsTurnRead(t, conn))
		events = append(events, frame)
		switch gjson.Get(frame, "type").String() {
		case "response.done", "error":
			return events
		}
	}
}

func toolLoopFrame(rounds int) string {
	var input strings.Builder
	input.WriteString(`[{"role":"user","content":"fix it"}`)
	for i := 0; i < rounds; i++ {
		fmt.Fprintf(&input, `,{"type":"function_call","call_id":"call_%d","name":"Read","arguments":"{\"file_path\":\"/a.go\"}"}`, i)
		fmt.Fprintf(&input, `,{"type":"function_call_output","call_id":"call_%d","output":"package a"}`, i)
	}
	input.WriteString(`]`)
	return fmt.Sprintf(`{"type":"response.create","model":%q,"input":%s}`, wsTurnModel, input.String())
}

func TestResponsesWebsocketAnswersAToolLoopTurnLocally(t *testing.T) {
	exec := &wsLoopExecutor{provider: "ws-loop-tool"}
	observer := &wsLoopObserver{}
	var on atomic.Bool
	on.Store(true)
	url := newWSLoopServer(t, exec, func() *loopdetect.Policy {
		if !on.Load() {
			return nil
		}
		return &loopdetect.Policy{Tool: &loopdetect.ToolLimits{Repeats: 8}, Observer: observer}
	})
	conn := dialWS(t, url)

	wsTurnSend(t, conn, toolLoopFrame(9))
	events := readTurn(t, conn)
	last := events[len(events)-1]
	if gjson.Get(last, "type").String() != "response.done" {
		t.Fatalf("the turn did not end with response.done: %v", events)
	}
	if text := gjson.Get(last, "response.output.0.content.0.text").String(); !strings.Contains(text, "loop protection") {
		t.Fatalf("no notice in %s", last)
	}
	if exec.calls.Load() != 0 {
		t.Fatalf("the turn was sent upstream %d times", exec.calls.Load())
	}
	if _, tools := observer.counts(); tools != 1 {
		t.Fatalf("observed %d tool-loop answers", tools)
	}

	// The setting is read again for each turn: turned off, the same conversation
	// goes upstream.
	on.Store(false)
	wsTurnSend(t, conn, toolLoopFrame(9))
	events = readTurn(t, conn)
	if gjson.Get(events[len(events)-1], "type").String() != "response.done" || exec.calls.Load() != 1 {
		t.Fatalf("calls %d, events %v", exec.calls.Load(), events)
	}
	if _, tools := observer.counts(); tools != 1 {
		t.Fatalf("observed %d tool-loop answers", tools)
	}
}

func TestResponsesWebsocketEndsALoopingReplyAndKeepsTheConnection(t *testing.T) {
	exec := &wsLoopExecutor{provider: "ws-loop-reply"}
	exec.loop.Store(true)
	observer := &wsLoopObserver{}
	url := newWSLoopServer(t, exec, func() *loopdetect.Policy {
		return &loopdetect.Policy{Reply: &loopdetect.ReplyLimits{}, Observer: observer}
	})
	conn := dialWS(t, url)

	wsTurnSend(t, conn, wsTurnCreate)
	events := readTurn(t, conn)
	last := events[len(events)-1]
	if gjson.Get(last, "type").String() != "error" || gjson.Get(last, "status").Int() != http.StatusUnprocessableEntity ||
		gjson.Get(last, "error.code").String() != loopdetect.ReplyLoopCode {
		t.Fatalf("the looping turn did not end with the loop error: %s", last)
	}
	if msg := gjson.Get(last, "error.message").String(); !strings.Contains(msg, `"Okay."`) || !strings.Contains(msg, "Stop repeating replies") {
		t.Fatalf("message %q", msg)
	}
	if got := len(events); got > 300 {
		t.Fatalf("%d events reached the client, the window is 256 lines", got)
	}
	if cuts, _ := observer.counts(); cuts != 1 {
		t.Fatalf("observed %d cuts", cuts)
	}

	// Only that turn failed; the connection carries the next one, whose reply
	// is counted from its own start.
	exec.loop.Store(false)
	wsTurnSend(t, conn, wsTurnCreate)
	events = readTurn(t, conn)
	if gjson.Get(events[len(events)-1], "type").String() != "response.done" {
		t.Fatalf("next turn: %v", events)
	}
	if cuts, _ := observer.counts(); cuts != 1 {
		t.Fatalf("observed %d cuts", cuts)
	}
}

// A stream that ends with an error closes its data channel in the same moment,
// so both are ready to the select that reads them. The error is what tells the
// client why the reply stopped, and the generic "stream closed" must not win
// over it by chance.
func TestResponsesWebsocketSurfacesTheErrorOfAStreamThatEndedWithOne(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil)))
	router := gin.New()
	router.GET("/ws", func(c *gin.Context) {
		conn, err := (&websocket.Upgrader{}).Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		data := make(chan []byte)
		close(data)
		errs := make(chan *interfaces.ErrorMessage, 1)
		errs <- &interfaces.ErrorMessage{
			StatusCode: loopdetect.ReplyLoopStatus,
			Error:      &loopdetect.ReplyLoopError{Trip: loopdetect.ReplyTrip{Window: 256, Distinct: 4, Units: []string{"Okay."}}},
		}
		close(errs)
		_, _ = h.forwardResponsesWebsocket(c, conn, func(...interface{}) {}, data, errs, &strings.Builder{}, "test")
	})
	server := httptest.NewServer(router)
	defer server.Close()

	// The old behavior was a coin toss; a hundred turns make a pass by luck
	// impossible.
	for i := 0; i < 100; i++ {
		conn := dialWS(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws")
		_, message, err := conn.ReadMessage()
		_ = conn.Close()
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if got := gjson.GetBytes(message, "error.code").String(); got != loopdetect.ReplyLoopCode {
			t.Fatalf("turn %d ended with %s, want the stream's own error", i, message)
		}
	}
}
