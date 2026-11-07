package auth

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// requestGuardMetadataKey remembers, in the request's metadata, what the guard
// decided, so that a second call for the same request (the handler retries a
// stream that failed before sending anything) does not run the guard again
// over text it has already rewritten.
const requestGuardMetadataKey = "request_guard_outcome"

// RequestGuardResult is what a host-provided request guard decides about a
// request before any account is chosen.
type RequestGuardResult struct {
	// Blocked refuses the request; Message and HTTPStatus say what the client is
	// told, as for RequestModerationResult.
	Blocked    bool
	Message    string
	HTTPStatus int
	// OriginalRequest and Payload, when non-nil, replace opts.OriginalRequest and
	// req.Payload. They hold the same text with the guard's rewrites applied, and
	// are the same slice when the two were the same to begin with (the executors
	// rely on that, see samePayloadStorage in the executor package).
	OriginalRequest []byte
	Payload         []byte
	// Reply, when non-nil, filters what comes back to the client.
	Reply ReplyFilter
}

// RequestGuard inspects the inbound request once, before account selection,
// and may refuse it or rewrite it. It differs from RequestModerator, which is
// asked about every candidate account in turn and can only refuse: a rewrite
// has to be decided once for the request, so that every account tried (and
// every failover) receives the same text.
//
// A RequestModerator that also implements RequestGuard is used as both.
// Dependency failures must be handled fail-open by the host.
type RequestGuard interface {
	GuardRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) RequestGuardResult
}

// guardedRequest is what guardRequest keeps in the metadata.
type guardedRequest struct {
	// input is the OriginalRequest the outcome was computed for.
	input           []byte
	originalRequest []byte
	payload         []byte
	reply           ReplyFilter
}

// guardRequest runs the host's guard, if there is one, and returns the request
// to execute with its reply filter. A blocked request comes back as the same
// error a moderation block does.
func (m *Manager) guardRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Request, cliproxyexecutor.Options, ReplyFilter, error) {
	if m == nil {
		return req, opts, nil, nil
	}
	m.mu.RLock()
	guard, _ := m.requestModerator.(RequestGuard)
	m.mu.RUnlock()
	if guard == nil {
		return req, opts, nil, nil
	}
	if previous, ok := opts.Metadata[requestGuardMetadataKey].(*guardedRequest); ok && previous != nil && sameBytes(previous.input, opts.OriginalRequest) {
		req, opts = applyGuardedRequest(req, opts, previous)
		return req, opts, previous.reply, nil
	}
	if opts.Metadata == nil {
		// The outcome is remembered in the metadata; give the request a map to
		// remember it in (the execution would allocate one anyway).
		opts.Metadata = make(map[string]any)
	}
	result := runRequestGuard(guard, ctx, req, opts)
	if result.Blocked {
		status := result.HTTPStatus
		if status < 400 || status > 599 {
			status = http.StatusForbidden
		}
		message := strings.TrimSpace(result.Message)
		if message == "" {
			message = "Your request was blocked by the content moderation policy."
		}
		return req, opts, nil, &Error{Code: contentPolicyViolationCode, Message: message, HTTPStatus: status}
	}
	outcome := &guardedRequest{input: opts.OriginalRequest, originalRequest: result.OriginalRequest, payload: result.Payload, reply: result.Reply}
	opts.Metadata[requestGuardMetadataKey] = outcome
	req, opts = applyGuardedRequest(req, opts, outcome)
	return req, opts, outcome.reply, nil
}

// ShareRequestGuardOutcome copies what the request guard decided from the
// metadata of one execution of a request to the metadata of another execution of
// the same request (same OriginalRequest slice), so a handler that runs one
// client request as several executions has it guarded once, not once for each.
// src must be the metadata map the first execution was given; it holds the
// outcome after that execution has run.
func ShareRequestGuardOutcome(dst, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	if outcome, ok := src[requestGuardMetadataKey]; ok {
		dst[requestGuardMetadataKey] = outcome
	}
}

// guardPanicLogInterval spaces the log lines of a guard that panics on every
// request; the ones in between are only counted.
const guardPanicLogInterval = 10 * time.Second

// guardPanicLog is the limiter for those lines. A package variable so a test can
// start from a clean one.
var guardPanicLog guardPanicLimiter

type guardPanicLimiter struct {
	last       atomic.Int64
	suppressed atomic.Int64
}

// allow reports whether a line may be written now, and how many were dropped
// since the last one.
func (l *guardPanicLimiter) allow(now time.Time) (ok bool, dropped int64) {
	at := now.UnixNano()
	last := l.last.Load()
	if (last != 0 && at-last < int64(guardPanicLogInterval)) || !l.last.CompareAndSwap(last, at) {
		l.suppressed.Add(1)
		return false, 0
	}
	return true, l.suppressed.Swap(0)
}

// runRequestGuard calls the host's guard. The guard is a check in front of every
// request; a bug in it must not take the request down with it, nor the process
// (a panic in a goroutine that nothing recovers ends it). A panic leaves the
// request as it came, as any other failure of the guard's dependencies would,
// and is logged at error level: a guard that panics on every request is a guard
// that is switched off, and an operator has to hear of it.
func runRequestGuard(guard RequestGuard, ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (result RequestGuardResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logGuardPanic(guard, req.Model, recovered)
			result = RequestGuardResult{}
		}
	}()
	return guard.GuardRequest(ctx, req, opts)
}

func logGuardPanic(guard RequestGuard, model string, recovered any) {
	ok, dropped := guardPanicLog.allow(time.Now())
	if !ok {
		return
	}
	value := fmt.Sprint(recovered)
	if len(value) > 300 {
		value = value[:300] + "..."
	}
	log.WithFields(log.Fields{
		"guard":      fmt.Sprintf("%T", guard),
		"model":      model,
		"panic":      value,
		"suppressed": dropped,
	}).Errorf("request guard panicked; this request goes on unguarded\n%s", debug.Stack())
}

func applyGuardedRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, outcome *guardedRequest) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	if outcome.originalRequest != nil {
		opts.OriginalRequest = outcome.originalRequest
	}
	if outcome.payload != nil {
		req.Payload = outcome.payload
	}
	return req, opts
}

// sameBytes reports whether a and b are the same slice, which is how the same
// request is told from another that happens to read alike.
func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

// executeGuarded, executeCountGuarded and executeStreamGuarded are Execute,
// ExecuteCount and ExecuteStream with the host's request guard in front and its
// reply filter behind. Without a guard they are the plain executions.
func (m *Manager) executeGuarded(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	req, opts, reply, err := m.guardRequest(ctx, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	resp, err := newExecutionService(m).execute(ctx, providers, req, opts)
	if err == nil && reply != nil {
		resp = filterReplyResponse(reply, resp)
	}
	return resp, err
}

// executeCountGuarded guards the request but has no reply to filter: a token
// count carries no model text.
func (m *Manager) executeCountGuarded(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	req, opts, _, err := m.guardRequest(ctx, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return newExecutionService(m).executeCount(ctx, providers, req, opts)
}

func (m *Manager) executeStreamGuarded(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	req, opts, reply, err := m.guardRequest(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	result, err := newExecutionService(m).executeStream(ctx, providers, req, opts)
	if err == nil && reply != nil {
		result = filterReplyStream(ctx, newReplyStreamFilter(reply), result)
	}
	return result, err
}
