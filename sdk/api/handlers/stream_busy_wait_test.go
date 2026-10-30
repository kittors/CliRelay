package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// parkAccount leaves an account cooling for d after a recoverable 503.
func parkAccount(manager *coreauth.Manager, id string, d time.Duration) {
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID:     id,
		Provider:   "codex",
		Model:      "test-model",
		Error:      &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"},
		RetryAfter: &d,
	})
}

type busyWaitRequest struct {
	recorder *httptest.ResponseRecorder
	dataChan <-chan []byte
	errChan  <-chan *StreamErrorMessage
	startErr *StreamErrorMessage
}

// startBusyStream starts a stream the way the protocol handlers do, with a
// gin context whose writer records what the client would have received.
func startBusyStream(handler *BaseAPIHandler, alt string, decorate func(context.Context) context.Context) busyWaitRequest {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
	if decorate != nil {
		ctx = decorate(ctx)
	}
	dataChan, _, errChan, startErr := handler.StartStreamWithAuthManager(ctx, "openai", "test-model", []byte(`{}`), alt)
	return busyWaitRequest{recorder: recorder, dataChan: dataChan, errChan: errChan, startErr: startErr}
}

func keepAliveConfig() *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 15}}
}

// A wait shorter than one keep-alive interval writes nothing, so a request
// that still fails gets its real status and Retry-After as a JSON error.
func TestBusyWaitShorterThanAKeepAliveKeepsTheErrorStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{failBeforeFirstByte(http.StatusServiceUnavailable)}}
		executor.failToOpen = true
		manager := newFailoverTestManager(t, executor, "codex", "bw-short")
		parkAccount(manager, "bw-short", 12*time.Second)
		handler := NewBaseAPIHandlers(keepAliveConfig(), manager)

		req := startBusyStream(handler, "", nil)

		if req.startErr == nil || req.startErr.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("startErr = %+v, want the 503 as a normal error response", req.startErr)
		}
		if req.recorder.Body.Len() != 0 {
			t.Fatalf("a short wait wrote %q before the error", req.recorder.Body.String())
		}
		if calls := executor.AuthIDs(); len(calls) != 1 {
			t.Fatalf("calls = %v, want the one retry after the wait", calls)
		}
	})
}

// A longer wait commits the response and sends the same heartbeat as
// ForwardStream, at the same interval, until the account recovers.
func TestBusyWaitLongerThanAKeepAliveSendsHeartbeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &scriptedStreamExecutor{provider: "codex"}
		manager := newFailoverTestManager(t, executor, "codex", "bw-long")
		parkAccount(manager, "bw-long", 40*time.Second)
		handler := NewBaseAPIHandlers(keepAliveConfig(), manager)

		req := startBusyStream(handler, "", nil)
		if req.startErr != nil {
			t.Fatalf("startErr = %+v", req.startErr)
		}
		if out := drainStream(t, req.dataChan, req.errChan); out.err != nil || out.payload != "ok" {
			t.Fatalf("outcome = %+v, want ok once the account recovered", out)
		}
		body := req.recorder.Body.String()
		if beats := strings.Count(body, sseKeepAliveComment); beats < 2 || beats > 3 {
			t.Fatalf("heartbeats = %d in %q, want one per 15s of a 40-48s wait", beats, body)
		}
		if got := req.recorder.Header().Get("Content-Type"); got != "text/event-stream" || req.recorder.Code != http.StatusOK {
			t.Fatalf("committed as %d %q, want 200 text/event-stream", req.recorder.Code, got)
		}
	})
}

// After a heartbeat the status is spent: a failure at the end of the wait is
// delivered in the stream, for the protocol handler's terminal error event.
func TestBusyWaitFailureAfterAHeartbeatIsReportedInTheStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &scriptedStreamExecutor{provider: "codex", steps: []streamStep{failBeforeFirstByte(http.StatusServiceUnavailable)}}
		executor.failToOpen = true
		manager := newFailoverTestManager(t, executor, "codex", "bw-late-fail")
		parkAccount(manager, "bw-late-fail", 40*time.Second)
		handler := NewBaseAPIHandlers(keepAliveConfig(), manager)

		req := startBusyStream(handler, "", nil)
		if req.startErr != nil {
			t.Fatalf("startErr = %+v, want the failure inside the committed stream", req.startErr)
		}
		out := drainStream(t, req.dataChan, req.errChan)
		if out.status != http.StatusServiceUnavailable {
			t.Fatalf("in-stream failure = %+v, want the 503", out)
		}
		if !strings.Contains(req.recorder.Body.String(), sseKeepAliveComment) {
			t.Fatalf("expected the heartbeat that committed the response, got %q", req.recorder.Body.String())
		}
	})
}

// No SSE heartbeat where the response is not SSE (Gemini with alt set) or the
// client is a websocket; the wait itself still happens.
func TestBusyWaitHeartbeatsOnlyForSSE(t *testing.T) {
	for name, tc := range map[string]struct {
		alt      string
		decorate func(context.Context) context.Context
	}{
		"gemini alt":           {alt: "json"},
		"downstream websocket": {decorate: coreexecutor.WithDownstreamWebsocket},
	} {
		synctest.Test(t, func(t *testing.T) {
			executor := &scriptedStreamExecutor{provider: "codex"}
			manager := newFailoverTestManager(t, executor, "codex", "bw-quiet")
			parkAccount(manager, "bw-quiet", 40*time.Second)
			handler := NewBaseAPIHandlers(keepAliveConfig(), manager)

			start := time.Now()
			req := startBusyStream(handler, tc.alt, tc.decorate)
			if req.startErr != nil {
				t.Fatalf("%s: startErr = %+v", name, req.startErr)
			}
			if waited := time.Since(start); waited < 40*time.Second {
				t.Fatalf("%s: waited %s, want the 40s until the account recovers", name, waited)
			}
			drainStream(t, req.dataChan, req.errChan)
			if req.recorder.Body.Len() != 0 {
				t.Fatalf("%s: wrote %q into a non-SSE response", name, req.recorder.Body.String())
			}
		})
	}
}
