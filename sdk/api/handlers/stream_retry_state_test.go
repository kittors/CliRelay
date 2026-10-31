package handlers

import (
	"context"
	"net/http"
	"runtime"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// gatedStreamExecutor opens a stream whose first chunk waits for firstGate and
// whose second waits for release. It keeps neither the request nor the options:
// the test is about who else holds them.
type gatedStreamExecutor struct {
	provider  string
	firstGate chan struct{}
	release   chan struct{}
}

func (e *gatedStreamExecutor) Identifier() string { return e.provider }

func (e *gatedStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (e *gatedStreamExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	ch := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(ch)
		for _, step := range []struct {
			gate    chan struct{}
			payload string
		}{{e.firstGate, "first"}, {e.release, "second"}} {
			select {
			case <-step.gate:
			case <-ctx.Done():
				return
			}
			select {
			case ch <- coreexecutor.StreamChunk{Payload: []byte(step.payload)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *gatedStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *gatedStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *gatedStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

// startStreamWithLargeBody starts a stream whose request body is several MB and
// reports on collected once that body has been garbage collected. The body does
// not outlive this function in the test, so only the code under test can keep it.
func startStreamWithLargeBody(t *testing.T, handler *BaseAPIHandler, collected chan<- struct{}) (<-chan []byte, <-chan *StreamErrorMessage) {
	t.Helper()
	body := make([]byte, 4<<20)
	runtime.AddCleanup(&body[0], func(done chan<- struct{}) { close(done) }, collected)
	dataChan, _, errChan, startErr := handler.StartStreamWithAuthManager(context.Background(), "openai", "test-model", body, "")
	if startErr != nil {
		t.Fatalf("start: %+v", startErr)
	}
	return dataChan, errChan
}

// bodyCollectedWithin reports whether the request body was garbage collected
// within d, running the collector while it waits.
func bodyCollectedWithin(collected <-chan struct{}, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		runtime.GC()
		select {
		case <-collected:
			return true
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}

// While no byte has reached the client the stream may still be retried on
// another account, which needs the request, so it is kept. From the first
// byte on nothing can be retried, and a stream that runs for minutes must not
// hold its multi-MB request body for all that time (the retry state is
// dropped with the first payload).
func TestStartedStreamDoesNotKeepItsRequestBodyAlive(t *testing.T) {
	executor := &gatedStreamExecutor{provider: "codex", firstGate: make(chan struct{}), release: make(chan struct{})}
	manager := newFailoverTestManager(t, executor, "codex", "retain-a")
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	collected := make(chan struct{})

	dataChan, errChan := startStreamWithLargeBody(t, handler, collected)

	if bodyCollectedWithin(collected, 200*time.Millisecond) {
		t.Fatal("the request body was released before the stream started; a failure now could not be retried")
	}
	close(executor.firstGate)
	if got := string(<-dataChan); got != "first" {
		t.Fatalf("first payload = %q", got)
	}
	if !bodyCollectedWithin(collected, 10*time.Second) {
		t.Fatal("the request body is still reachable after the first payload was delivered")
	}

	close(executor.release)
	if out := drainStream(t, dataChan, errChan); out.err != nil || out.payload != "second" {
		t.Fatalf("rest of the stream = %+v, want the second payload", out)
	}
}
