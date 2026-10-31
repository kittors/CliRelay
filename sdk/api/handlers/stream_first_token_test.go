package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type stallMode int

const (
	answerNow           stallMode = iota // stream "ok" at once
	stallAfterHeaders                    // accept the stream, then send nothing
	stallBeforeHeaders                   // never return from ExecuteStream on its own
	firstTokenThenPause                  // one chunk, a long pause, another chunk
)

// stallingStreamExecutor plays one stallMode per attempt and records how each
// attempt's context ended, so a test can tell a timeout from a client leaving.
type stallingStreamExecutor struct {
	modes []stallMode
	pause time.Duration

	mu      sync.Mutex
	authIDs []string
	ctxErrs []error
}

func (e *stallingStreamExecutor) Identifier() string { return "codex" }

func (e *stallingStreamExecutor) Execute(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	// A slow non-streaming answer: the first-token timeout must not touch it.
	select {
	case <-time.After(e.pause):
		return coreexecutor.Response{Payload: []byte("done")}, nil
	case <-ctx.Done():
		return coreexecutor.Response{}, ctx.Err()
	}
}

func (e *stallingStreamExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.authIDs = append(e.authIDs, auth.ID)
	call := len(e.authIDs)
	e.ctxErrs = append(e.ctxErrs, nil)
	e.mu.Unlock()

	mode := answerNow
	if call <= len(e.modes) {
		mode = e.modes[call-1]
	}
	record := func() {
		e.mu.Lock()
		e.ctxErrs[call-1] = ctx.Err()
		e.mu.Unlock()
	}
	if mode == stallBeforeHeaders {
		<-ctx.Done()
		record()
		return nil, ctx.Err()
	}
	ch := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(ch)
		switch mode {
		case answerNow:
			ch <- coreexecutor.StreamChunk{Payload: []byte("ok")}
		case stallAfterHeaders:
			<-ctx.Done()
			record()
			ch <- coreexecutor.StreamChunk{Err: ctx.Err()}
		case firstTokenThenPause:
			ch <- coreexecutor.StreamChunk{Payload: []byte("first ")}
			select {
			case <-time.After(e.pause):
				ch <- coreexecutor.StreamChunk{Payload: []byte("second")}
			case <-ctx.Done():
				record()
				ch <- coreexecutor.StreamChunk{Err: ctx.Err()}
			}
		}
	}()
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *stallingStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *stallingStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *stallingStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *stallingStreamExecutor) attempts() ([]string, []error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...), append([]error(nil), e.ctxErrs...)
}

// withFirstTokenTimeout gives the default group (which governs requests that
// no single group does) a first-token timeout. busyWait, when given, sets the
// group's busy-wait cap too.
func withFirstTokenTimeout(manager *coreauth.Manager, seconds int, busyWait ...int) {
	group := sdkconfig.RoutingChannelGroup{Name: "default", FirstTokenTimeoutSeconds: seconds}
	if len(busyWait) > 0 {
		group.BusyWaitMaxSeconds = new(busyWait[0])
	}
	manager.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{ChannelGroups: []sdkconfig.RoutingChannelGroup{group}}})
}

// isFirstTokenTimeout reports whether err is the error a missed first token
// produces: a 504 whose code says so.
func isFirstTokenTimeout(err error) bool {
	var authErr *coreauth.Error
	return errors.As(err, &authErr) && authErr.Code == coreauth.FirstTokenTimeoutCode
}

func streamOnce(t *testing.T, handler *BaseAPIHandler) streamOutcome {
	t.Helper()
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "test-model", []byte(`{}`), "")
	return drainStream(t, dataChan, errChan)
}

func TestFirstTokenTimeoutMovesAStalledStreamToAnotherAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &stallingStreamExecutor{modes: []stallMode{stallAfterHeaders, answerNow}}
		manager := newFailoverTestManager(t, executor, "codex", "ftt-move-a", "ftt-move-b")
		withFirstTokenTimeout(manager, 5)
		handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

		start := time.Now()
		out := streamOnce(t, handler)

		if out.err != nil || out.payload != "ok" {
			t.Fatalf("outcome = %+v, want ok from the second account", out)
		}
		if waited := time.Since(start); waited != 5*time.Second {
			t.Fatalf("switched after %s, want exactly the 5s timeout", waited)
		}
		ids, ctxErrs := executor.attempts()
		if len(ids) != 2 || !distinct(ids) {
			t.Fatalf("attempts = %v, want two different accounts", ids)
		}
		// The abandoned attempt must look like a timeout, not a client that left:
		// executors drop context.Canceled from the request log and usage records.
		if !errors.Is(ctxErrs[0], context.DeadlineExceeded) {
			t.Fatalf("stalled attempt ended with %v, want context.DeadlineExceeded", ctxErrs[0])
		}
	})
}

func TestFirstTokenTimeoutCoversWaitingForResponseHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &stallingStreamExecutor{modes: []stallMode{stallBeforeHeaders, answerNow}}
		manager := newFailoverTestManager(t, executor, "codex", "ftt-hdr-a", "ftt-hdr-b")
		withFirstTokenTimeout(manager, 5)
		handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

		out := streamOnce(t, handler)

		if out.err != nil || out.payload != "ok" {
			t.Fatalf("outcome = %+v, want ok from the second account", out)
		}
		if ids, _ := executor.attempts(); len(ids) != 2 || !distinct(ids) {
			t.Fatalf("attempts = %v, want two different accounts", ids)
		}
	})
}

// With nowhere to go, the client gets a 504 naming the timeout, whether the
// upstream stalled before or after its response headers.
func TestFirstTokenTimeoutWithoutAnotherAccountReturns504(t *testing.T) {
	for _, mode := range []stallMode{stallAfterHeaders, stallBeforeHeaders} {
		synctest.Test(t, func(t *testing.T) {
			executor := &stallingStreamExecutor{modes: []stallMode{mode}}
			manager := newFailoverTestManager(t, executor, "codex", "ftt-alone")
			withFirstTokenTimeout(manager, 5)
			handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

			out := streamOnce(t, handler)

			if out.status != http.StatusGatewayTimeout || !isFirstTokenTimeout(out.err) {
				t.Fatalf("mode %d: outcome = %+v, want a 504 first-token timeout", mode, out)
			}
			if ids, _ := executor.attempts(); len(ids) != 1 {
				t.Fatalf("mode %d: attempts = %v, want one", mode, ids)
			}
		})
	}
}

// A group that turned busy waiting on still does not wait out a first-token
// timeout: the account sits out five seconds, the provider is slow right now,
// and sending the same request back after the wait would only repeat the stall
// (and make the client wait through another timeout). One attempt, one 504.
func TestFirstTokenTimeoutIsNotBusyWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &stallingStreamExecutor{modes: []stallMode{stallBeforeHeaders, stallBeforeHeaders, stallBeforeHeaders}}
		manager := newFailoverTestManager(t, executor, "codex", "ftt-no-wait")
		withFirstTokenTimeout(manager, 5, 60)
		handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

		start := time.Now()
		out := streamOnce(t, handler)

		if out.status != http.StatusGatewayTimeout || !isFirstTokenTimeout(out.err) {
			t.Fatalf("outcome = %+v, want a 504 first-token timeout", out)
		}
		if ids, _ := executor.attempts(); len(ids) != 1 {
			t.Fatalf("attempts = %v, want the one that timed out", ids)
		}
		if took := time.Since(start); took != 5*time.Second {
			t.Fatalf("took %s, want the 5s of the one attempt and no wait after it", took)
		}
	})
}

// The timeout only covers the first token: a long pause after it is the
// client's business, and nothing is configured unless an operator sets it.
func TestFirstTokenTimeoutLeavesStartedAndUnconfiguredStreamsAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &stallingStreamExecutor{modes: []stallMode{firstTokenThenPause}, pause: time.Minute}
		manager := newFailoverTestManager(t, executor, "codex", "ftt-started")
		withFirstTokenTimeout(manager, 5)
		handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

		if out := streamOnce(t, handler); out.err != nil || out.payload != "first second" {
			t.Fatalf("outcome = %+v, want the whole stream despite the pause", out)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		executor := &stallingStreamExecutor{modes: []stallMode{stallAfterHeaders}}
		manager := newFailoverTestManager(t, executor, "codex", "ftt-off")
		handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		start := time.Now()
		dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "test-model", []byte(`{}`), "")
		drainStream(t, dataChan, errChan)
		synctest.Wait()
		// Only the caller's own 10-minute deadline ended it.
		if waited := time.Since(start); waited != 10*time.Minute {
			t.Fatalf("the stream ended after %s, want the caller's 10m deadline", waited)
		}
		if ids, _ := executor.attempts(); len(ids) != 1 {
			t.Fatalf("attempts = %v, want one", ids)
		}
	})
}

func TestFirstTokenTimeoutDoesNotApplyToNonStreamingRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &stallingStreamExecutor{pause: time.Minute}
		manager := newFailoverTestManager(t, executor, "codex", "ftt-nonstream")
		withFirstTokenTimeout(manager, 5)
		handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

		resp, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "test-model", []byte(`{}`), "")
		if errMsg != nil || string(resp) != "done" {
			t.Fatalf("non-streaming request = %q, %+v; want the slow answer", resp, errMsg)
		}
	})
}
