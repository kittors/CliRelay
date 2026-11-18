package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

// A Responses WebSocket session pins the credential that served its first turn.
// When that account runs out of quota the session has to move to one that can
// still serve it, as an HTTP request would: the pin used to hold, and every
// later turn on the connection failed with a 429 model_cooldown until the client
// reconnected, while the other account was never tried.

type wsStatusErr struct {
	status     int
	body       string
	retryAfter time.Duration
}

func (e *wsStatusErr) Error() string   { return e.body }
func (e *wsStatusErr) StatusCode() int { return e.status }
func (e *wsStatusErr) RetryAfter() *time.Duration {
	d := e.retryAfter
	return &d
}

const wsUsageLimitBody = `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":7200}}`

type wsQuotaExecutor struct {
	provider  string
	mu        sync.Mutex
	exhausted map[string]bool
	// once fails the next call on an account with the error, then clears.
	once     map[string]error
	calls    []string
	payloads [][]byte
}

func (e *wsQuotaExecutor) Identifier() string { return e.provider }

func (e *wsQuotaExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsQuotaExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	e.payloads = append(e.payloads, append([]byte(nil), req.Payload...))
	failure := e.once[auth.ID]
	delete(e.once, auth.ID)
	exhausted := e.exhausted[auth.ID]
	e.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	if exhausted {
		return nil, &wsStatusErr{status: http.StatusTooManyRequests, body: wsUsageLimitBody, retryAfter: 2 * time.Hour}
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"id":"resp_ws_quota","output":[]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *wsQuotaExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *wsQuotaExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsQuotaExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (e *wsQuotaExecutor) exhaust(authID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.exhausted == nil {
		e.exhausted = map[string]bool{}
	}
	e.exhausted[authID] = true
}

func (e *wsQuotaExecutor) failOnce(authID string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.once == nil {
		e.once = map[string]error{}
	}
	e.once[authID] = err
}

func (e *wsQuotaExecutor) served() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *wsQuotaExecutor) payload(i int) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.payloads[i]
}

// newWSQuotaServer serves two accounts of one provider and model. attributes
// are set on both, e.g. websockets=true for an upstream that keeps the context.
func newWSQuotaServer(t *testing.T, exec *wsQuotaExecutor, model string, attributes map[string]string) (*wsTurnServer, *coreauth.Manager, [2]string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	ids := [2]string{wsPinName(t, "auth-a"), wsPinName(t, "auth-b")}
	for _, id := range ids {
		auth := &coreauth.Auth{ID: id, Provider: exec.provider, Status: coreauth.StatusActive, Attributes: attributes}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, exec.provider, []*registry.ModelInfo{{ID: model}})
		clientID := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })
	}
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	s := &wsTurnServer{t: t, handlerDone: make(chan struct{}, 4)}
	engine := gin.New()
	engine.GET("/v1/responses", func(c *gin.Context) {
		h.ResponsesWebsocket(c)
		s.handlerDone <- struct{}{}
	})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	s.url = "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
	return s, manager, ids
}

func other(ids [2]string, id string) string {
	if ids[0] == id {
		return ids[1]
	}
	return ids[0]
}

func wantServed(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("accounts called = %v, want %v", got, want)
	}
}

func wantCompleted(t *testing.T, turn string, reply []byte) {
	t.Helper()
	if gjson.GetBytes(reply, "type").String() != "response.completed" {
		t.Fatalf("%s: reply = %s, want response.completed", turn, reply)
	}
}

func TestResponsesWebsocketMovesTheTurnOffAnAccountThatRanOutOfQuota(t *testing.T) {
	exec := &wsQuotaExecutor{provider: wsPinName(t, "codex")}
	model := wsPinName(t, "model")
	server, _, ids := newWSQuotaServer(t, exec, model, nil)
	conn := server.dial()
	defer server.hangUp(conn)

	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "turn 1", wsTurnRead(t, conn))
	pinned := exec.served()[0]

	// The pinned account's 5-hour window runs out during turn 2: the turn moves
	// to the other account instead of reporting the 429, and stays there.
	exec.exhaust(pinned)
	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "turn 2", wsTurnRead(t, conn))
	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "turn 3", wsTurnRead(t, conn))

	wantServed(t, exec.served(), pinned, pinned, other(ids, pinned), other(ids, pinned))
}

func TestResponsesWebsocketReleasesThePinOfAnAccountCoolingDownBeforeTheTurn(t *testing.T) {
	exec := &wsQuotaExecutor{provider: wsPinName(t, "codex")}
	model := wsPinName(t, "model")
	server, manager, ids := newWSQuotaServer(t, exec, model, nil)
	conn := server.dial()
	defer server.hangUp(conn)

	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "turn 1", wsTurnRead(t, conn))
	pinned := exec.served()[0]

	// Another request ran the pinned account out of quota in the meantime.
	resetIn := 2 * time.Hour
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:     pinned,
		Provider:   exec.provider,
		Model:      model,
		Error:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: wsUsageLimitBody},
		RetryAfter: &resetIn,
	})
	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "turn 2", wsTurnRead(t, conn))

	// The cooling account is not even tried.
	wantServed(t, exec.served(), pinned, other(ids, pinned))
}

func TestResponsesWebsocketAsksForTheFullTurnWhenTheUpstreamKeptTheContext(t *testing.T) {
	exec := &wsQuotaExecutor{provider: wsPinName(t, "codex")}
	model := wsPinName(t, "model")
	server, _, ids := newWSQuotaServer(t, exec, model, map[string]string{"websockets": "true"})
	conn := server.dial()
	defer server.hangUp(conn)

	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "turn 1", wsTurnRead(t, conn))
	pinned := exec.served()[0]

	// The increment names a response only the pinned account's upstream holds,
	// so it cannot be sent to the other account; the client is asked for the
	// turn in full, as Codex does on its own when it opens a new connection.
	exec.exhaust(pinned)
	increment := `{"type":"response.create","model":"` + model + `","previous_response_id":"resp_ws_quota","input":[{"role":"user","content":[{"type":"input_text","text":"next"}]}]}`
	wsTurnSend(t, conn, increment)
	reply := wsTurnRead(t, conn)
	if gjson.GetBytes(reply, "type").String() != "error" || gjson.GetBytes(reply, "error.code").String() != wsErrorCodePreviousResponseNotFound {
		t.Fatalf("increment after the account ran out: reply = %s, want previous_response_not_found", reply)
	}

	// The full turn goes to the other account, complete.
	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "full turn", wsTurnRead(t, conn))
	wantServed(t, exec.served(), pinned, pinned, other(ids, pinned))
	if gjson.GetBytes(exec.payload(2), "previous_response_id").Exists() {
		t.Fatalf("request on the other account = %s, want it without previous_response_id", exec.payload(2))
	}
}

func TestResponsesWebsocketKeepsThePinThroughAShortBackOff(t *testing.T) {
	exec := &wsQuotaExecutor{provider: wsPinName(t, "codex")}
	model := wsPinName(t, "model")
	server, _, _ := newWSQuotaServer(t, exec, model, nil)
	conn := server.dial()
	defer server.hangUp(conn)

	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "turn 1", wsTurnRead(t, conn))
	pinned := exec.served()[0]

	// A load-shed with a two-second wait: the pin keeps its upstream state, the
	// turn reports the failure, and the next turn runs on the same account.
	exec.failOnce(pinned, &wsStatusErr{status: http.StatusServiceUnavailable, body: `{"error":{"message":"busy"}}`, retryAfter: 2 * time.Second})
	wsTurnSend(t, conn, wsPinCreate(model))
	if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "status").Int() != http.StatusServiceUnavailable {
		t.Fatalf("turn 2: reply = %s, want the 503", reply)
	}
	wsTurnSend(t, conn, wsPinCreate(model))
	wantCompleted(t, "turn 3", wsTurnRead(t, conn))

	wantServed(t, exec.served(), pinned, pinned, pinned)
}
