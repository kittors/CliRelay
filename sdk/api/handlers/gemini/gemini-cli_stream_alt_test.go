package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// okStreamExecutor answers every stream with one chunk.
type okStreamExecutor struct{}

func (okStreamExecutor) Identifier() string { return "gemini" }

func (okStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (okStreamExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	ch := make(chan coreexecutor.StreamChunk, 1)
	ch <- coreexecutor.StreamChunk{Payload: []byte("ok")}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (okStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (okStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (okStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

// cliStreamAgainstParkedAccount sends a local streamGenerateContent request
// (with the given query) to a pool whose only account is cooling for 40s, in a
// group that waits for it, and returns what the client received.
func cliStreamAgainstParkedAccount(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(okStreamExecutor{})
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "cli-alt-account", Provider: "gemini", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient("cli-alt-account", "gemini", []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("cli-alt-account") })
	manager.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{
		ChannelGroups: []sdkconfig.RoutingChannelGroup{{Name: "default", BusyWaitMaxSeconds: new(60)}},
	}})
	parked := 40 * time.Second
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID: "cli-alt-account", Provider: "gemini", Model: "test-model",
		Error:      &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"},
		RetryAfter: &parked,
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 15}}, manager)
	router.POST("/v1internal:method", NewGeminiCLIAPIHandler(base).CLIHandler)

	req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent"+query, strings.NewReader(`{"model":"test-model"}`))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// The scheduler's waits before a stream starts send SSE heartbeats, and commit
// the response as 200 text/event-stream, only for a client that asked for SSE.
// A client that asked for another format (alt=json) must get none of that: the
// handler passes the real alt on, as the public Gemini handler does.
func TestCLIStreamWaitSendsNoSSEHeartbeatsForANonSSEAlt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		rec := cliStreamAgainstParkedAccount(t, "?alt=json")
		if waited := time.Since(start); waited < 40*time.Second {
			t.Fatalf("waited %s, want the 40s until the account recovers", waited)
		}
		if body := rec.Body.String(); body != "ok" {
			t.Fatalf("body = %q, want the bare payload with no SSE comments or framing", body)
		}
		if got := rec.Header().Get("Content-Type"); strings.Contains(got, "text/event-stream") {
			t.Fatalf("Content-Type = %q, want the response not committed as SSE", got)
		}
	})
}

// Control: the same wait for an SSE client is kept alive with heartbeats, and
// its payload is framed as an SSE event.
func TestCLIStreamWaitKeepsSSEClientsAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := cliStreamAgainstParkedAccount(t, "")
		body := rec.Body.String()
		if beats := strings.Count(body, ": keep-alive\n\n"); beats < 2 {
			t.Fatalf("heartbeats = %d in %q, want one per 15s of the wait", beats, body)
		}
		if !strings.Contains(body, "data: ok\n\n") {
			t.Fatalf("body = %q, want the payload framed as an SSE event", body)
		}
		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
			t.Fatalf("Content-Type = %q, want text/event-stream", got)
		}
	})
}
