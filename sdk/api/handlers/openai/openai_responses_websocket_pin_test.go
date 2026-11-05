package openai

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
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

// A Responses WebSocket session pins the credential that served its first turn.
// When the client switches to a model that credential cannot serve, the pin has
// to give way; when it only adds a thinking suffix, the pin has to hold.

type wsPinExecutor struct {
	provider string
	mu       sync.Mutex
	authIDs  []string
}

func (e *wsPinExecutor) Identifier() string { return e.provider }

func (e *wsPinExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsPinExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.authIDs = append(e.authIDs, auth.ID)
	e.mu.Unlock()
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"id":"resp_ws_pin","output":[]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *wsPinExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *wsPinExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsPinExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (e *wsPinExecutor) served() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

type wsPinAuth struct {
	id       string
	executor *wsPinExecutor
	models   []string
}

func newWSPinServer(t *testing.T, auths ...wsPinAuth) *wsTurnServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	registered := map[*wsPinExecutor]bool{}
	for _, a := range auths {
		if !registered[a.executor] {
			manager.RegisterExecutor(a.executor)
			registered[a.executor] = true
		}
		auth := &coreauth.Auth{ID: a.id, Provider: a.executor.provider, Status: coreauth.StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", a.id, err)
		}
		models := make([]*registry.ModelInfo, 0, len(a.models))
		for _, id := range a.models {
			models = append(models, &registry.ModelInfo{ID: id})
		}
		registry.GetGlobalRegistry().RegisterClient(a.id, a.executor.provider, models)
		id := a.id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
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
	return s
}

func wsPinName(t *testing.T, suffix string) string {
	return "ws-pin-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + suffix
}

func wsPinCreate(model string) string {
	return fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, model)
}

func TestResponsesWebsocketReleasesPinWhenModelSwitchesProvider(t *testing.T) {
	claude := &wsPinExecutor{provider: wsPinName(t, "claude")}
	gemini := &wsPinExecutor{provider: wsPinName(t, "gemini")}
	claudeModel, geminiModel := wsPinName(t, "claude-model"), wsPinName(t, "gemini-model")
	server := newWSPinServer(t,
		wsPinAuth{id: wsPinName(t, "claude-auth"), executor: claude, models: []string{claudeModel}},
		wsPinAuth{id: wsPinName(t, "gemini-auth"), executor: gemini, models: []string{geminiModel}},
	)
	conn := server.dial()

	for _, model := range []string{claudeModel, geminiModel, claudeModel} {
		wsTurnSend(t, conn, wsPinCreate(model))
		if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "type").String() != "response.done" {
			t.Fatalf("turn on %s: reply = %s, want response.done", model, reply)
		}
	}
	server.hangUp(conn)

	if got := claude.served(); len(got) != 2 {
		t.Fatalf("claude turns served = %v, want 2", got)
	}
	if got := gemini.served(); len(got) != 1 {
		t.Fatalf("gemini turns served = %v, want 1", got)
	}
}

func TestResponsesWebsocketKeepsPinAcrossThinkingSuffix(t *testing.T) {
	exec := &wsPinExecutor{provider: wsPinName(t, "codex")}
	model := wsPinName(t, "model")
	server := newWSPinServer(t,
		wsPinAuth{id: wsPinName(t, "auth-a"), executor: exec, models: []string{model}},
		wsPinAuth{id: wsPinName(t, "auth-b"), executor: exec, models: []string{model}},
	)
	conn := server.dial()

	for _, requested := range []string{model, model + "(high)", model + "(low)", model} {
		wsTurnSend(t, conn, wsPinCreate(requested))
		if reply := wsTurnRead(t, conn); gjson.GetBytes(reply, "type").String() != "response.done" {
			t.Fatalf("turn on %s: reply = %s, want response.done", requested, reply)
		}
	}
	server.hangUp(conn)

	served := exec.served()
	if len(served) != 4 {
		t.Fatalf("turns served = %v, want 4", served)
	}
	for _, id := range served[1:] {
		if id != served[0] {
			t.Fatalf("turns served by %v, want every turn on the pinned %s", served, served[0])
		}
	}
}
