package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// A bug in the host's guard must cost the request nothing: it goes on as it
// came, and the log says what happened.
func TestRequestGuardPanicFailsOpen(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	body := []byte(`{"q":"hello"}`)
	paths := map[string]func(*Manager) error{
		"execute": func(m *Manager) error {
			_, err := m.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: body}, cliproxyexecutor.Options{OriginalRequest: body})
			return err
		},
		"stream": func(m *Manager) error {
			_, err := m.ExecuteStream(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: body}, cliproxyexecutor.Options{OriginalRequest: body})
			return err
		},
		"count": func(m *Manager) error {
			_, err := m.ExecuteCount(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: body}, cliproxyexecutor.Options{OriginalRequest: body})
			return err
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			guardPanicLog = guardPanicLimiter{}
			hook.Reset()
			guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
				items := []int{1}
				index := len(items) + 1
				_ = items[index] // an index out of range: a real panic, not a staged one
				return RequestGuardResult{}
			}}
			executor := &recordingExecutor{reply: []byte("ok")}
			if err := run(newGuardTestManager(t, executor, guard, 1)); err != nil {
				t.Fatalf("a guard that panics must not fail the request: %v", err)
			}
			seen := executor.requests()
			if len(seen) != 1 || !sameStorage(seen[0].payload, body) || !sameStorage(seen[0].original, body) {
				t.Fatalf("the executor must get the caller's own body: %+v", seen)
			}
			var logged *log.Entry
			for _, entry := range hook.AllEntries() {
				if strings.Contains(entry.Message, "request guard panicked") {
					logged = entry
				}
			}
			if logged == nil || logged.Level != log.ErrorLevel {
				t.Fatalf("the panic must be logged at error level: %+v", hook.AllEntries())
			}
			if !strings.Contains(logged.Data["panic"].(string), "index out of range") || !strings.Contains(logged.Data["guard"].(string), "guardingModerator") || logged.Data["model"] != "m" {
				t.Fatalf("log fields = %v", logged.Data)
			}
			if !strings.Contains(logged.Message, "request_guard.go") {
				t.Fatalf("the log line must carry the stack: %s", logged.Message)
			}
		})
	}
}

// A request that fails over to the next account must not run the broken guard
// again for each account, nor fill the log with one panic per attempt.
func TestRequestGuardPanicIsRememberedForTheRequest(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()
	guardPanicLog = guardPanicLimiter{}

	guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		panic("guard bug")
	}}
	executor := &recordingExecutor{reply: []byte("ok"), failFirst: 1}
	manager := newGuardTestManager(t, executor, guard, 2)
	body := []byte(`{"q":"hello"}`)
	if _, err := manager.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: body}, cliproxyexecutor.Options{OriginalRequest: body}); err != nil {
		t.Fatal(err)
	}
	if n := len(executor.requests()); n != 2 {
		t.Fatalf("executor ran %d times, want a failure and a retry", n)
	}
	if n := guard.guardCalls(); n != 1 {
		t.Fatalf("the guard ran %d times for one request, want 1", n)
	}
}

func TestRequestGuardPanicLogsAreSpaced(t *testing.T) {
	var limiter guardPanicLimiter
	start := time.Unix(5000, 0)
	if ok, dropped := limiter.allow(start); !ok || dropped != 0 {
		t.Fatalf("the first panic is always logged: %v, %d", ok, dropped)
	}
	for i := 1; i <= 5; i++ {
		if ok, _ := limiter.allow(start.Add(time.Duration(i) * time.Second)); ok {
			t.Fatalf("panic %d came within the interval but was logged", i)
		}
	}
	ok, dropped := limiter.allow(start.Add(guardPanicLogInterval))
	if !ok || dropped != 5 {
		t.Fatalf("after the interval: ok=%v dropped=%d, want the next line to stand for 5", ok, dropped)
	}
}

// One client request that a handler runs as several executions is guarded
// once: the outcome travels in the metadata, and the handler passes it on.
func TestShareRequestGuardOutcomeGuardsARequestOnce(t *testing.T) {
	original := []byte(`{"q":"word","n":3}`)
	rewritten := []byte(`{"q":"***","n":3}`)
	guard := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{OriginalRequest: rewritten, Payload: rewritten}
	}}
	executor := &recordingExecutor{reply: []byte("ok")}
	manager := newGuardTestManager(t, executor, guard, 1)

	var first map[string]any
	for i := 0; i < 3; i++ {
		meta := map[string]any{"single_pick": true}
		if first != nil {
			ShareRequestGuardOutcome(meta, first)
		}
		if _, err := manager.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: original}, cliproxyexecutor.Options{OriginalRequest: original, Metadata: meta}); err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = meta
		}
	}
	if n := guard.guardCalls(); n != 1 {
		t.Fatalf("the guard ran %d times for one client request, want 1", n)
	}
	for i, got := range executor.requests() {
		if string(got.payload) != string(rewritten) || string(got.original) != string(rewritten) {
			t.Fatalf("execution %d got %q / %q, want the rewritten body", i, got.payload, got.original)
		}
	}

	// Without the hand-over each execution is guarded on its own, which is what
	// the images handler used to do for n > 1.
	guard2 := &guardingModerator{guard: func(context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) RequestGuardResult {
		return RequestGuardResult{}
	}}
	manager2 := newGuardTestManager(t, &recordingExecutor{reply: []byte("ok")}, guard2, 1)
	for i := 0; i < 3; i++ {
		if _, err := manager2.Execute(context.Background(), []string{"guard-test"}, cliproxyexecutor.Request{Model: "m", Payload: original}, cliproxyexecutor.Options{OriginalRequest: original, Metadata: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := guard2.guardCalls(); n != 3 {
		t.Fatalf("without sharing, the guard ran %d times, want 3", n)
	}
}

func TestShareRequestGuardOutcomeIsSafeWithoutMetadata(t *testing.T) {
	ShareRequestGuardOutcome(nil, map[string]any{requestGuardMetadataKey: &guardedRequest{}})
	ShareRequestGuardOutcome(map[string]any{}, nil)
	dst := map[string]any{}
	ShareRequestGuardOutcome(dst, map[string]any{"other": 1})
	if len(dst) != 0 {
		t.Fatalf("only the guard's outcome is passed on: %v", dst)
	}
}
