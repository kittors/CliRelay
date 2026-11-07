package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// guardingModerator is a RequestModerator that is also a RequestGuard.
type guardingModerator struct {
	mu     sync.Mutex
	guard  func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult
	guards int
}

func (g *guardingModerator) Moderate(context.Context, *Auth, cliproxyexecutor.Options) RequestModerationResult {
	return RequestModerationResult{}
}

func (g *guardingModerator) GuardRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) RequestGuardResult {
	g.mu.Lock()
	g.guards++
	g.mu.Unlock()
	return g.guard(ctx, req, opts)
}

func (g *guardingModerator) guardCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.guards
}

// seenRequest is what an executor was handed.
type seenRequest struct {
	payload  []byte
	original []byte
}

// recordingExecutor remembers every request it executes and answers with a
// canned reply. The first failFirst executions fail, to push a request on to
// its next candidate account.
type recordingExecutor struct {
	mu        sync.Mutex
	seen      []seenRequest
	reply     []byte
	chunks    [][]byte
	failFirst int
}

func (e *recordingExecutor) Identifier() string { return "guard-test" }

func (e *recordingExecutor) record(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
	e.mu.Lock()
	e.seen = append(e.seen, seenRequest{payload: req.Payload, original: opts.OriginalRequest})
	e.mu.Unlock()
}

func (e *recordingExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(req, opts)
	e.mu.Lock()
	failing := len(e.seen) <= e.failFirst
	e.mu.Unlock()
	if failing {
		return cliproxyexecutor.Response{}, errors.New("upstream unavailable")
	}
	return cliproxyexecutor.Response{Payload: e.reply, Headers: http.Header{"Content-Length": {"2"}}}, nil
}

func (e *recordingExecutor) ExecuteStream(_ context.Context, _ *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.record(req, opts)
	out := make(chan cliproxyexecutor.StreamChunk, len(e.chunks))
	for _, chunk := range e.chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Upstream": {"1"}}, Chunks: out}, nil
}

func (e *recordingExecutor) CountTokens(_ context.Context, _ *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(req, opts)
	return cliproxyexecutor.Response{Payload: []byte("count")}, nil
}

func (e *recordingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *recordingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *recordingExecutor) requests() []seenRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]seenRequest(nil), e.seen...)
}

func newGuardTestManager(t *testing.T, executor *recordingExecutor, guard RequestModerator, accounts int) *Manager {
	t.Helper()
	manager := NewManager(nil, orderedAuthSelector{}, nil)
	manager.RegisterExecutor(executor)
	for i := 0; i < accounts; i++ {
		auth := &Auth{ID: string(rune('a'+i)) + "-account", Provider: executor.Identifier(), Status: StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	manager.SetRequestModerator(guard)
	return manager
}

// sameStorage reports whether two slices share their first byte, which is how
// the executors tell that the payload and the original request are one body.
func sameStorage(a, b []byte) bool {
	return len(a) > 0 && len(a) == len(b) && &a[0] == &b[0]
}

func TestRequestGuardRewritesWhatEveryExecutorReceives(t *testing.T) {
	original := []byte(`{"messages":[{"content":"say the word"}]}`)
	rewritten := []byte(`{"messages":[{"content":"say the ***"}]}`)
	guard := &guardingModerator{guard: func(_ context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) RequestGuardResult {
		if !sameStorage(req.Payload, opts.OriginalRequest) {
			t.Errorf("the guard was handed two different bodies")
		}
		return RequestGuardResult{OriginalRequest: rewritten, Payload: rewritten}
	}}
	paths := map[string]func(*Manager, cliproxyexecutor.Request, cliproxyexecutor.Options) error{
		"execute": func(m *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
			_, err := m.Execute(context.Background(), []string{"guard-test"}, req, opts)
			return err
		},
		"stream": func(m *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
			_, err := m.ExecuteStream(context.Background(), []string{"guard-test"}, req, opts)
			return err
		},
		"count": func(m *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
			_, err := m.ExecuteCount(context.Background(), []string{"guard-test"}, req, opts)
			return err
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			executor := &recordingExecutor{reply: []byte("ok")}
			manager := newGuardTestManager(t, executor, guard, 1)
			if err := run(manager, cliproxyexecutor.Request{Model: "m", Payload: original}, cliproxyexecutor.Options{OriginalRequest: original}); err != nil {
				t.Fatal(err)
			}
			seen := executor.requests()
			if len(seen) != 1 {
				t.Fatalf("executor ran %d times, want 1", len(seen))
			}
			if string(seen[0].payload) != string(rewritten) || string(seen[0].original) != string(rewritten) {
				t.Fatalf("executor got payload %q original %q, want the rewritten body", seen[0].payload, seen[0].original)
			}
			// The executors compare the two bodies' storage to skip translating twice.
			if !sameStorage(seen[0].payload, seen[0].original) {
				t.Fatal("payload and original request no longer share storage")
			}
			if string(original) != `{"messages":[{"content":"say the word"}]}` {
				t.Fatalf("the caller's body was modified: %q", original)
			}
		})
	}
}

func TestRequestGuardLeavesTheRequestAloneWhenItChangesNothing(t *testing.T) {
	body := []byte(`{"a":1}`)
	executor := &recordingExecutor{reply: []byte("ok")}
	manager := newGuardTestManager(t, executor, &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{}
	}}, 1)
	if _, err := manager.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: body}, cliproxyexecutor.Options{OriginalRequest: body}); err != nil {
		t.Fatal(err)
	}
	seen := executor.requests()
	if len(seen) != 1 || !sameStorage(seen[0].payload, body) || !sameStorage(seen[0].original, body) {
		t.Fatalf("the executor did not receive the caller's own body: %+v", seen)
	}
}

func TestRequestGuardBlocksBeforeAnyExecutorRuns(t *testing.T) {
	guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{Blocked: true, Message: "no", HTTPStatus: http.StatusUnavailableForLegalReasons}
	}}
	paths := map[string]func(*Manager) error{
		"execute": func(m *Manager) error {
			_, err := m.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m"}, cliproxyexecutor.Options{})
			return err
		},
		"stream": func(m *Manager) error {
			_, err := m.ExecuteStream(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m"}, cliproxyexecutor.Options{})
			return err
		},
		"count": func(m *Manager) error {
			_, err := m.ExecuteCount(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m"}, cliproxyexecutor.Options{})
			return err
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			executor := &recordingExecutor{reply: []byte("ok")}
			err := run(newGuardTestManager(t, executor, guard, 1))
			var authErr *Error
			if !errors.As(err, &authErr) || authErr.Code != contentPolicyViolationCode || authErr.HTTPStatus != http.StatusUnavailableForLegalReasons || authErr.Message != "no" {
				t.Fatalf("error = %#v, want a content policy violation with the guard's message and status", err)
			}
			if !isContentPolicyViolation(err) {
				t.Fatal("a guard block is not recognised as a content policy violation")
			}
			if n := len(executor.requests()); n != 0 {
				t.Fatalf("executor ran %d times for a blocked request", n)
			}
		})
	}
}

func TestRequestGuardBlockWithoutUsableStatusOrMessageGetsDefaults(t *testing.T) {
	guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{Blocked: true, HTTPStatus: 200, Message: "  "}
	}}
	manager := newGuardTestManager(t, &recordingExecutor{}, guard, 1)
	_, err := manager.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m"}, cliproxyexecutor.Options{})
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.HTTPStatus != http.StatusForbidden || authErr.Message == "" {
		t.Fatalf("error = %#v, want 403 with a default message", err)
	}
}

// A request that fails over to the next account must reach it with the same
// rewrite, and the guard must not run a second time over text it already
// rewrote.
func TestRequestGuardRunsOncePerRequestAcrossFailover(t *testing.T) {
	original := []byte(`{"q":"word"}`)
	rewritten := []byte(`{"q":"***"}`)
	guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{OriginalRequest: rewritten, Payload: rewritten}
	}}
	executor := &recordingExecutor{reply: []byte("ok"), failFirst: 1}
	manager := newGuardTestManager(t, executor, guard, 2)
	if _, err := manager.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: original}, cliproxyexecutor.Options{OriginalRequest: original}); err != nil {
		t.Fatal(err)
	}
	seen := executor.requests()
	if len(seen) != 2 {
		t.Fatalf("executor ran %d times, want a failure and a retry", len(seen))
	}
	for i, got := range seen {
		if string(got.payload) != string(rewritten) || string(got.original) != string(rewritten) {
			t.Fatalf("attempt %d got %q / %q, want the rewritten body", i, got.payload, got.original)
		}
	}
	if n := guard.guardCalls(); n != 1 {
		t.Fatalf("guard ran %d times for one request, want 1", n)
	}
}

// The stream handler starts the stream again with the very same options when
// the first attempt fails before it sent anything. The second call must see the
// first call's outcome rather than guard the (unrewritten) request anew.
func TestRequestGuardOutcomeSurvivesAStreamRestart(t *testing.T) {
	original := []byte(`{"q":"word"}`)
	rewritten := []byte(`{"q":"***"}`)
	guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{OriginalRequest: rewritten, Payload: rewritten}
	}}
	executor := &recordingExecutor{}
	manager := newGuardTestManager(t, executor, guard, 1)
	req := cliproxyexecutor.Request{Model: "m", Payload: original}
	opts := cliproxyexecutor.Options{Stream: true, OriginalRequest: original, Metadata: map[string]any{}}
	for i := 0; i < 2; i++ {
		if _, err := manager.ExecuteStream(context.Background(), []string{"guard-test"}, req, opts); err != nil {
			t.Fatal(err)
		}
	}
	seen := executor.requests()
	if len(seen) != 2 || string(seen[1].payload) != string(rewritten) || string(seen[1].original) != string(rewritten) {
		t.Fatalf("restarted stream saw %+v, want the rewritten body", seen)
	}
	if n := guard.guardCalls(); n != 1 {
		t.Fatalf("guard ran %d times across a restart, want 1", n)
	}

	// Another request is another decision, even with the same text.
	other := []byte(`{"q":"word"}`)
	otherOpts := cliproxyexecutor.Options{Stream: true, OriginalRequest: other, Metadata: map[string]any{}}
	if _, err := manager.ExecuteStream(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: other}, otherOpts); err != nil {
		t.Fatal(err)
	}
	if n := guard.guardCalls(); n != 2 {
		t.Fatalf("guard ran %d times for two requests, want 2", n)
	}
}

func TestRequestGuardIsOptionalForModerators(t *testing.T) {
	body := []byte(`{"a":"word"}`)
	executor := &recordingExecutor{reply: []byte("ok")}
	manager := newGuardTestManager(t, executor, requestModeratorFunc(func(context.Context, *Auth, cliproxyexecutor.Options) RequestModerationResult {
		return RequestModerationResult{}
	}), 1)
	if _, err := manager.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: body}, cliproxyexecutor.Options{OriginalRequest: body}); err != nil {
		t.Fatal(err)
	}
	if seen := executor.requests(); len(seen) != 1 || string(seen[0].payload) != string(body) {
		t.Fatalf("executor saw %+v", seen)
	}
}

// replyUpper is a reply filter that upper-cases whatever it is given.
type replyUpper struct{}

func (replyUpper) FilterResponse(resp cliproxyexecutor.Response) cliproxyexecutor.Response {
	resp.Payload = []byte(strings.ToUpper(string(resp.Payload)))
	resp.Headers = resp.Headers.Clone()
	resp.Headers.Del("Content-Length")
	return resp
}

func (replyUpper) NewStream() StreamReplyFilter { return &upperStream{} }

type upperStream struct{}

func (*upperStream) Push(chunk []byte) [][]byte {
	return [][]byte{[]byte(strings.ToUpper(string(chunk)))}
}
func (*upperStream) Flush() [][]byte              { return nil }
func (*upperStream) HeldSince() (time.Time, bool) { return time.Time{}, false }

func TestRequestGuardReplyFilterIsAppliedToTheReply(t *testing.T) {
	guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{Reply: replyUpper{}}
	}}
	executor := &recordingExecutor{reply: []byte("hello"), chunks: [][]byte{[]byte("one"), []byte("two")}}
	manager := newGuardTestManager(t, executor, guard, 1)

	resp, err := manager.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Payload) != "HELLO" {
		t.Fatalf("payload = %q, want the filtered reply", resp.Payload)
	}
	if resp.Headers.Get("Content-Length") != "" {
		t.Fatal("the reply kept a Content-Length that no longer describes it")
	}

	stream, err := manager.ExecuteStream(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m"}, cliproxyexecutor.Options{Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if stream.Headers.Get("X-Upstream") != "1" {
		t.Fatalf("stream headers lost: %v", stream.Headers)
	}
	var got []string
	for chunk := range stream.Chunks {
		got = append(got, string(chunk.Payload))
	}
	if strings.Join(got, ",") != "ONE,TWO" {
		t.Fatalf("stream = %v, want the filtered chunks", got)
	}

	count, err := manager.ExecuteCount(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m"}, cliproxyexecutor.Options{})
	if err != nil || string(count.Payload) != "count" {
		t.Fatalf("a token count must not be filtered: %q, %v", count.Payload, err)
	}
}

func TestRequestGuardDoesNotFilterAFailedExecution(t *testing.T) {
	guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{Reply: replyUpper{}}
	}}
	executor := &recordingExecutor{failFirst: 10}
	manager := newGuardTestManager(t, executor, guard, 1)
	if _, err := manager.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m"}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("expected the upstream failure to be returned")
	}
}
