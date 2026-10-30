package auth

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// The first-token timeout of a channel group (routing_stream_guards.go in the
// config package) gives up on a streaming attempt whose upstream has produced
// nothing usable in time.
//
// "First token" means the first chunk the executor hands over with a
// non-whitespace payload, i.e. the first bytes that would be sent to the
// client. The clock starts before the upstream request is sent, so it also
// covers connecting and waiting for the response headers. For upstreams that
// open with a bookkeeping event this stops the clock early: Anthropic sends
// message_start (and Codex response.created) before any generated text, so on
// those the timeout catches a stalled connection or a request stuck in the
// upstream's queue, not slow generation after the stream has opened. A Gemini
// thinking model that does not stream its thoughts sends nothing until the
// thinking is done, so a timeout for such a group has to exceed that.
//
// A timed-out attempt is cancelled and recorded as a 504 with Code
// "first_token_timeout". The account sits out only briefly (the same back-off a
// provider load-shed gets) instead of the minute a generic 5xx costs: the miss
// says the upstream is slow right now, and an operator's tight timeout must not
// be able to park a whole small pool for a minute on a single slow response.
const firstTokenTimeoutCooldown = transientUpstreamOverloadCooldown

type firstTokenTimeoutError struct {
	err      *Error
	cooldown time.Duration
}

func newFirstTokenTimeoutError(timeout time.Duration) *firstTokenTimeoutError {
	return &firstTokenTimeoutError{
		err: &Error{
			Code:       "first_token_timeout",
			Message:    fmt.Sprintf("upstream sent no data within %s (channel group first-token timeout)", timeout),
			Retryable:  true,
			HTTPStatus: http.StatusGatewayTimeout,
		},
		cooldown: firstTokenTimeoutCooldown,
	}
}

func (e *firstTokenTimeoutError) Error() string   { return e.err.Error() }
func (e *firstTokenTimeoutError) StatusCode() int { return e.err.HTTPStatus }
func (e *firstTokenTimeoutError) Unwrap() error   { return e.err }

// RetryAfter is read by MarkResult to set the account's back-off.
func (e *firstTokenTimeoutError) RetryAfter() *time.Duration {
	cooldown := e.cooldown
	return &cooldown
}

// IsFirstTokenTimeout reports whether err is a first-token timeout.
func IsFirstTokenTimeout(err error) bool {
	_, ok := err.(*firstTokenTimeoutError)
	return ok
}

// executeStreamAttempt runs one upstream stream attempt, under the governing
// channel group's first-token timeout when it sets one. Without a timeout it
// is a plain ExecuteStream call.
func (s executionService) executeStreamAttempt(candidate *mixedExecutionCandidate, scope *mixedExecutionScope) (*cliproxyexecutor.StreamResult, error) {
	timeout := s.manager.streamGuardsFor(scope.opts.Metadata).firstTokenTimeout
	if timeout <= 0 {
		return candidate.executor.ExecuteStream(candidate.execCtx, candidate.auth, candidate.execReq, scope.opts)
	}
	watchdog := startFirstTokenWatchdog(candidate.execCtx, timeout)
	result, err := candidate.executor.ExecuteStream(watchdog.ctx, candidate.auth, candidate.execReq, scope.opts)
	if err != nil || result == nil {
		watchdog.release()
		if watchdog.timedOut() && candidate.execCtx.Err() == nil {
			// The upstream did not even answer with headers in time.
			return nil, watchdog.timeoutError()
		}
		return result, err
	}
	guarded := *result
	guarded.Chunks = watchdog.guard(result.Chunks)
	return &guarded, nil
}

const (
	watchdogArmed int32 = iota
	watchdogDisarmed
	watchdogFired
)

// firstTokenWatchdog cancels an attempt that has not produced its first token
// in time. Whichever comes first wins: the first token disarms it for good, or
// the timer fires and the attempt is abandoned even if a token arrives a moment
// later.
type firstTokenWatchdog struct {
	timeout time.Duration
	ctx     *attemptContext
	state   atomic.Int32
	fired   chan struct{}
	timer   *time.Timer
}

func startFirstTokenWatchdog(parent context.Context, timeout time.Duration) *firstTokenWatchdog {
	w := &firstTokenWatchdog{
		timeout: timeout,
		ctx:     newAttemptContext(parent),
		fired:   make(chan struct{}),
	}
	w.timer = time.AfterFunc(timeout, w.fire)
	return w
}

func (w *firstTokenWatchdog) fire() {
	if !w.state.CompareAndSwap(watchdogArmed, watchdogFired) {
		return
	}
	// DeadlineExceeded rather than Canceled: executors treat a cancelled
	// context as the client going away and record nothing, while this attempt
	// failed and has to show up in the request log and the usage records.
	w.ctx.cancel(context.DeadlineExceeded)
	close(w.fired)
}

// sawFirstToken disarms the watchdog. It returns false when the timer already
// fired, in which case the attempt is lost even though data arrived.
func (w *firstTokenWatchdog) sawFirstToken() bool {
	if w.state.CompareAndSwap(watchdogArmed, watchdogDisarmed) {
		w.timer.Stop()
		return true
	}
	return w.state.Load() == watchdogDisarmed
}

func (w *firstTokenWatchdog) timedOut() bool { return w.state.Load() == watchdogFired }

func (w *firstTokenWatchdog) timeoutError() error { return newFirstTokenTimeoutError(w.timeout) }

// release stops the timer and the parent-cancellation hook once the attempt is
// over. The attempt context itself stays usable for an executor still winding
// down.
func (w *firstTokenWatchdog) release() {
	w.timer.Stop()
	w.ctx.detach()
}

// guard relays the executor's chunks until the first token, replacing the
// stream's end with a first-token timeout error when the watchdog fires.
func (w *firstTokenWatchdog) guard(in <-chan cliproxyexecutor.StreamChunk) <-chan cliproxyexecutor.StreamChunk {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer w.release()
		for {
			select {
			case chunk, ok := <-in:
				if !ok {
					if w.timedOut() {
						out <- cliproxyexecutor.StreamChunk{Err: w.timeoutError()}
					}
					return
				}
				if w.timedOut() || (chunk.Err == nil && hasFirstToken(chunk.Payload) && !w.sawFirstToken()) {
					w.abandon(out, in)
					return
				}
				out <- chunk
			case <-w.fired:
				w.abandon(out, in)
				return
			}
		}
	}()
	return out
}

// abandon reports the timeout and lets the executor finish on its own; its
// remaining chunks (typically the error the cancellation caused) are dropped.
func (w *firstTokenWatchdog) abandon(out chan<- cliproxyexecutor.StreamChunk, in <-chan cliproxyexecutor.StreamChunk) {
	out <- cliproxyexecutor.StreamChunk{Err: w.timeoutError()}
	go func() {
		for range in {
		}
	}()
}

func hasFirstToken(payload []byte) bool {
	return len(bytes.TrimSpace(payload)) > 0
}

// attemptContext is the context of one upstream attempt. It ends when its
// parent does or when the watchdog cancels it, and unlike a context.WithCancel
// child it can report DeadlineExceeded for the latter: the HTTP/2 transport
// returns ctx.Err() and the HTTP/1 transport context.Cause, and both have to
// say the attempt timed out rather than that the client left.
type attemptContext struct {
	context.Context
	done chan struct{}

	mu   sync.Mutex
	err  error
	stop func() bool
}

func newAttemptContext(parent context.Context) *attemptContext {
	c := &attemptContext{Context: parent, done: make(chan struct{})}
	c.stop = context.AfterFunc(parent, func() { c.cancel(parent.Err()) })
	return c
}

func (c *attemptContext) Done() <-chan struct{} { return c.done }

func (c *attemptContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *attemptContext) cancel(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
}

func (c *attemptContext) detach() {
	if c.stop != nil {
		c.stop()
	}
}
