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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

// The quota middleware publishes a handlers.QuotaGate on the upgrade request.
// The frame loop must call it once per billable turn, before the executor, and
// release an admitted turn exactly once however the turn ends: a missed release
// leaks the key's concurrency slot, a second one frees a slot another request
// still holds.

const wsTurnModel = "ws-turn-model"

type wsTurnGate struct {
	mu            sync.Mutex
	refusal       *handlers.QuotaRejection
	calls         int
	admitted      int
	released      int
	doubleRelease int
}

func (g *wsTurnGate) gate() handlers.QuotaGate {
	return func() (func(), *handlers.QuotaRejection) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls++
		if g.refusal != nil {
			return nil, g.refusal
		}
		g.admitted++
		done := false
		return func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if done {
				g.doubleRelease++
				return
			}
			done = true
			g.released++
		}, nil
	}
}

func (g *wsTurnGate) refuseWith(r *handlers.QuotaRejection) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refusal = r
}

func (g *wsTurnGate) assertCounts(t *testing.T, calls, admitted int) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.calls != calls || g.admitted != admitted {
		t.Fatalf("gate calls/admitted = %d/%d, want %d/%d", g.calls, g.admitted, calls, admitted)
	}
	if g.released != g.admitted || g.doubleRelease != 0 {
		t.Fatalf("released %d of %d admitted turns, %d released twice; want each released exactly once", g.released, g.admitted, g.doubleRelease)
	}
}

type wsTurnExecutor struct {
	provider string
	calls    atomic.Int64
	started  chan struct{}

	mu   sync.Mutex
	hold chan struct{}
	fail bool
}

func newWSTurnExecutor(t *testing.T) *wsTurnExecutor {
	return &wsTurnExecutor{provider: "ws-turn-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()), started: make(chan struct{}, 8)}
}

func (e *wsTurnExecutor) Identifier() string { return e.provider }

func (e *wsTurnExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsTurnExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls.Add(1)
	e.mu.Lock()
	hold, fail := e.hold, e.fail
	e.mu.Unlock()
	select {
	case e.started <- struct{}{}:
	default:
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		return nil, errors.New("upstream refused the request")
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"id":"resp_ws_turn","output":[]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *wsTurnExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *wsTurnExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsTurnExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

// wsTurnServer serves ResponsesWebsocket with the given gates published the way
// the middleware publishes them. handlerDone receives once per connection, after
// the handler has returned.
type wsTurnServer struct {
	t           *testing.T
	url         string
	handlerDone chan struct{}
}

func newWSTurnServer(t *testing.T, exec *wsTurnExecutor, quotaGate handlers.QuotaGate, modelGate handlers.ModelGate) *wsTurnServer {
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
	s := &wsTurnServer{t: t, handlerDone: make(chan struct{}, 4)}
	engine := gin.New()
	engine.GET("/v1/responses", func(c *gin.Context) {
		if quotaGate != nil {
			c.Set(handlers.QuotaGateContextKey, quotaGate)
		}
		if modelGate != nil {
			c.Set(handlers.ModelGateContextKey, modelGate)
		}
		h.ResponsesWebsocket(c)
		s.handlerDone <- struct{}{}
	})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	s.url = "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
	return s
}

func (s *wsTurnServer) dial() *websocket.Conn {
	s.t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(s.url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		s.t.Fatalf("dial: %v", err)
	}
	return conn
}

// hangUp closes the client side and waits for the handler to return, so every
// release it owes has happened.
func (s *wsTurnServer) hangUp(conn *websocket.Conn) {
	s.t.Helper()
	_ = conn.Close()
	select {
	case <-s.handlerDone:
	case <-time.After(5 * time.Second):
		s.t.Fatal("handler did not return after the client left")
	}
}

func wsTurnSend(t *testing.T, conn *websocket.Conn, frame string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func wsTurnRead(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, reply, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return reply
}

var wsTurnCreate = fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, wsTurnModel)

func TestResponsesWebsocketQuotaRefusalIsAnErrorEvent(t *testing.T) {
	exec := newWSTurnExecutor(t)
	probe := &wsTurnGate{refusal: &handlers.QuotaRejection{
		StatusCode: http.StatusTooManyRequests,
		Body:       []byte(`{"error":{"code":"daily_limit_exceeded","message":"Daily request limit exceeded: 5/5 requests used today.","type":"rate_limit_exceeded"}}`),
		Headers:    http.Header{"X-Clirelay-Quota-Code": {"daily_limit_exceeded"}},
	}}
	server := newWSTurnServer(t, exec, probe.gate(), nil)
	conn := server.dial()

	wsTurnSend(t, conn, wsTurnCreate)
	reply := wsTurnRead(t, conn)
	want := map[string]string{
		"type":                          "error",
		"status":                        "429",
		"error.code":                    "daily_limit_exceeded",
		"error.type":                    "rate_limit_exceeded",
		"error.message":                 "Daily request limit exceeded: 5/5 requests used today.",
		`headers.X-Clirelay-Quota-Code`: "daily_limit_exceeded",
	}
	for path, value := range want {
		if got := gjson.GetBytes(reply, path).String(); got != value {
			t.Fatalf("%s = %q, want %q (event %s)", path, got, value, reply)
		}
	}
	if calls := exec.calls.Load(); calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}

	// Only that turn failed; the connection carries the next one.
	probe.refuseWith(nil)
	wsTurnSend(t, conn, wsTurnCreate)
	if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "type").String() != "response.done" {
		t.Fatalf("next turn reply = %s, want response.done", reply)
	}
	server.hangUp(conn)
	probe.assertCounts(t, 2, 1)
}

func TestResponsesWebsocketReleasesEachAdmittedTurnOnce(t *testing.T) {
	t.Run("turns complete", func(t *testing.T) {
		exec := newWSTurnExecutor(t)
		probe := &wsTurnGate{}
		server := newWSTurnServer(t, exec, probe.gate(), nil)
		conn := server.dial()
		for i := 0; i < 2; i++ {
			wsTurnSend(t, conn, wsTurnCreate)
			if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "type").String() != "response.done" {
				t.Fatalf("reply = %s, want response.done", reply)
			}
		}
		server.hangUp(conn)
		probe.assertCounts(t, 2, 2)
	})

	t.Run("executor fails", func(t *testing.T) {
		exec := newWSTurnExecutor(t)
		exec.fail = true
		probe := &wsTurnGate{}
		server := newWSTurnServer(t, exec, probe.gate(), nil)
		conn := server.dial()
		wsTurnSend(t, conn, wsTurnCreate)
		if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "type").String() != "error" {
			t.Fatalf("reply = %s, want an error event", reply)
		}
		server.hangUp(conn)
		probe.assertCounts(t, 1, 1)
	})

	t.Run("model refused after admission", func(t *testing.T) {
		exec := newWSTurnExecutor(t)
		probe := &wsTurnGate{}
		refuseAll := handlers.ModelGate(func(model string) *interfaces.ErrorMessage {
			return &interfaces.ErrorMessage{StatusCode: http.StatusForbidden, Error: fmt.Errorf("model '%s' is not allowed for this API key", model)}
		})
		server := newWSTurnServer(t, exec, probe.gate(), refuseAll)
		conn := server.dial()
		wsTurnSend(t, conn, wsTurnCreate)
		if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "status").Int() != http.StatusForbidden {
			t.Fatalf("reply = %s, want the 403 model refusal", reply)
		}
		server.hangUp(conn)
		if calls := exec.calls.Load(); calls != 0 {
			t.Fatalf("executor calls = %d, want 0", calls)
		}
		probe.assertCounts(t, 1, 1)
	})

	t.Run("client leaves mid-turn", func(t *testing.T) {
		exec := newWSTurnExecutor(t)
		hold := make(chan struct{})
		exec.hold = hold
		probe := &wsTurnGate{}
		server := newWSTurnServer(t, exec, probe.gate(), nil)
		conn := server.dial()
		wsTurnSend(t, conn, wsTurnCreate)
		select {
		case <-exec.started:
		case <-time.After(5 * time.Second):
			t.Fatal("the turn never reached the executor")
		}
		_ = conn.Close()
		close(hold)
		server.hangUp(conn)
		probe.assertCounts(t, 1, 1)
	})
}

// A prewarm (generate:false) is answered locally and never reaches an executor,
// so it is not a billable turn.
func TestResponsesWebsocketPrewarmIsNotABillableTurn(t *testing.T) {
	exec := newWSTurnExecutor(t)
	probe := &wsTurnGate{}
	server := newWSTurnServer(t, exec, probe.gate(), nil)
	conn := server.dial()
	wsTurnSend(t, conn, fmt.Sprintf(`{"type":"response.create","model":%q,"generate":false,"input":[]}`, wsTurnModel))
	for _, want := range []string{"response.created", "response.done"} {
		if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "type").String() != want {
			t.Fatalf("prewarm reply = %s, want %s", reply, want)
		}
	}
	server.hangUp(conn)
	if calls := exec.calls.Load(); calls != 0 {
		t.Fatalf("executor calls = %d, want 0", calls)
	}
	probe.assertCounts(t, 0, 0)
}
