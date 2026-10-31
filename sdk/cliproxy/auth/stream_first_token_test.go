package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// silentStreamExecutor accepts every stream and then sends nothing (or only
// its preamble) until its context ends.
type silentStreamExecutor struct {
	provider string
	preamble []byte
}

func (e silentStreamExecutor) Identifier() string { return e.provider }

func (e silentStreamExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not used")
}

func (e silentStreamExecutor) ExecuteStream(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(ch)
		if e.preamble != nil {
			ch <- cliproxyexecutor.StreamChunk{Payload: e.preamble}
		}
		<-ctx.Done()
		ch <- cliproxyexecutor.StreamChunk{Err: ctx.Err()}
	}()
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e silentStreamExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e silentStreamExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not used")
}

func (e silentStreamExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

// A missed first token is a 504 that parks the account for the short
// load-shed back-off, not the minute a generic 5xx costs: an operator's tight
// timeout must not empty a small pool for a minute on one slow response.
func TestFirstTokenTimeoutParksTheAccountBriefly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager(nil, nil, nil)
		mgr.RegisterExecutor(silentStreamExecutor{provider: "silent"})
		if _, err := mgr.Register(context.Background(), &Auth{ID: "ftt-park", Provider: "silent", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{
			ChannelGroups: []sdkconfig.RoutingChannelGroup{{Name: "default", FirstTokenTimeoutSeconds: 5}},
		}})

		result, err := mgr.ExecuteStream(context.Background(), []string{"silent"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Stream: true})
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		if result.AuthID != "ftt-park" {
			t.Fatalf("AuthID = %q, want the serving account", result.AuthID)
		}
		var streamErr error
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				streamErr = chunk.Err
			}
		}
		timedOutAt := time.Now()
		if !isFirstTokenTimeout(streamErr) || statusCodeFromError(streamErr) != http.StatusGatewayTimeout {
			t.Fatalf("stream error = %v, want a 504 first-token timeout", streamErr)
		}
		synctest.Wait()

		auth, ok := mgr.GetByID("ftt-park")
		if !ok {
			t.Fatal("account vanished")
		}
		state := auth.ModelStates["test-model"]
		if state == nil || state.LastError == nil || state.LastError.Code != FirstTokenTimeoutCode || state.LastError.HTTPStatus != http.StatusGatewayTimeout {
			t.Fatalf("model state = %+v, want the first-token timeout recorded", state)
		}
		if got := state.NextRetryAfter.Sub(timedOutAt); got != firstTokenTimeoutCooldown {
			t.Fatalf("back-off = %s, want %s", got, firstTokenTimeoutCooldown)
		}
	})
}

// Blank lines carry nothing a client can show, so an upstream that only sends
// those (some proxies pad an idle stream with them) is still stalled.
func TestFirstTokenTimeoutIgnoresBlankChunks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := NewManager(nil, nil, nil)
		mgr.RegisterExecutor(silentStreamExecutor{provider: "silent", preamble: []byte("\n\r\n \t")})
		if _, err := mgr.Register(context.Background(), &Auth{ID: "ftt-blank", Provider: "silent", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{
			ChannelGroups: []sdkconfig.RoutingChannelGroup{{Name: "default", FirstTokenTimeoutSeconds: 5}},
		}})

		result, err := mgr.ExecuteStream(context.Background(), []string{"silent"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Stream: true})
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		var streamErr error
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				streamErr = chunk.Err
			}
		}
		if !isFirstTokenTimeout(streamErr) {
			t.Fatalf("stream error = %v, want the first-token timeout despite the blank chunk", streamErr)
		}
	})
}

// The transports read ctx.Err() (HTTP/2) or context.Cause (HTTP/1); both must
// say the attempt timed out. Executors drop context.Canceled from the request
// log and the usage records, so a cancel would hide the failed attempt.
func TestAttemptContextReportsATimeoutNotACancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	attempt := newAttemptContext(parent)
	child, cancelChild := context.WithCancel(attempt)
	defer cancelChild()

	attempt.cancel(context.DeadlineExceeded)
	<-child.Done()
	if !errors.Is(attempt.Err(), context.DeadlineExceeded) || !errors.Is(context.Cause(attempt), context.DeadlineExceeded) {
		t.Fatalf("Err = %v, Cause = %v; want DeadlineExceeded", attempt.Err(), context.Cause(attempt))
	}
	if !errors.Is(child.Err(), context.DeadlineExceeded) {
		t.Fatalf("derived context ended with %v, want DeadlineExceeded", child.Err())
	}

	other := newAttemptContext(parent)
	cancelParent()
	<-other.Done()
	if !errors.Is(other.Err(), context.Canceled) {
		t.Fatalf("a client leaving must still read as a cancel, got %v", other.Err())
	}
}

func TestFirstTokenWatchdogFirstTokenWinsForGood(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		watchdog := startFirstTokenWatchdog(context.Background(), 5*time.Second)
		defer watchdog.release()
		if !watchdog.sawFirstToken() {
			t.Fatal("first token before the deadline must disarm the watchdog")
		}
		time.Sleep(time.Minute)
		if watchdog.timedOut() || watchdog.ctx.Err() != nil {
			t.Fatalf("a disarmed watchdog fired: timedOut=%v err=%v", watchdog.timedOut(), watchdog.ctx.Err())
		}
	})
	synctest.Test(t, func(t *testing.T) {
		watchdog := startFirstTokenWatchdog(context.Background(), 5*time.Second)
		defer watchdog.release()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if watchdog.sawFirstToken() {
			t.Fatal("a token after the deadline must not revive the attempt")
		}
		if !errors.Is(watchdog.ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("attempt context = %v, want DeadlineExceeded", watchdog.ctx.Err())
		}
	})
}

// isFirstTokenTimeout reports whether err is the error a missed first token
// produces: a 504 whose code says so.
func isFirstTokenTimeout(err error) bool {
	var authErr *Error
	return errors.As(err, &authErr) && authErr.Code == FirstTokenTimeoutCode
}
