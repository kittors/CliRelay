package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

// tracedContext is a request context carrying a route trace, as the request
// logging middleware gives every gateway request.
func tracedContext() (context.Context, *routetrace.Recorder) {
	rec := routetrace.New()
	return routetrace.WithRecorder(context.Background(), rec), rec
}

// tracedRow is a usage record as the request log keeps it: the account it is
// for and the route trace as it stood when the record went out.
type tracedRow struct {
	authID string
	trace  *routetrace.Trace
}

// publishingExecutor answers like scriptedExecutor and publishes a record for
// every call the way the usage reporters do: through routetrace.DeferPublish,
// under the context the call ran with. Its streams send their one chunk after
// firstToken; streamErrs[n], when set, is the n-th stream's first chunk
// instead, or its second, after the data, when failAfterData is set.
type publishingExecutor struct {
	*scriptedExecutor
	rec           *routetrace.Recorder
	firstToken    time.Duration
	streamErrs    []error
	failAfterData bool

	mu      sync.Mutex
	streams int
	rows    []tracedRow
}

func (e *publishingExecutor) publish(ctx context.Context, authID string) {
	publish := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.rows = append(e.rows, tracedRow{authID: authID, trace: e.rec.Snapshot()})
	}
	if !routetrace.DeferPublish(ctx, publish) {
		publish()
	}
}

func (e *publishingExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	defer e.publish(ctx, auth.ID)
	return e.scriptedExecutor.Execute(ctx, auth, req, opts)
}

func (e *publishingExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.next(auth); err != nil {
		e.publish(ctx, auth.ID)
		return nil, err
	}
	e.mu.Lock()
	var streamErr error
	if e.streams < len(e.streamErrs) {
		streamErr = e.streamErrs[e.streams]
	}
	e.streams++
	e.mu.Unlock()
	chunks := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		// A stream's reporter publishes as the stream ends.
		defer e.publish(ctx, auth.ID)
		time.Sleep(e.firstToken)
		if streamErr != nil && !e.failAfterData {
			chunks <- cliproxyexecutor.StreamChunk{Err: streamErr}
			return
		}
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
		if streamErr != nil {
			chunks <- cliproxyexecutor.StreamChunk{Err: streamErr}
		}
	}()
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *publishingExecutor) Rows() []tracedRow {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]tracedRow(nil), e.rows...)
}

func newPublishingManager(t *testing.T, exec *publishingExecutor, ids ...string) (*Manager, context.Context) {
	t.Helper()
	ctx, rec := tracedContext()
	exec.rec = rec
	return newPlainManager(t, exec, ids...), ctx
}

func drainStream(result *cliproxyexecutor.StreamResult) error {
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil && streamErr == nil {
			streamErr = chunk.Err
		}
	}
	return streamErr
}

func executeStream(mgr *Manager, ctx context.Context) (*cliproxyexecutor.StreamResult, error) {
	return mgr.ExecuteStream(ctx, []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{Stream: true})
}

// A request that fails over records both attempts. The failed attempt's usage
// record waits until the request moves on, so its request log row already says
// where the request went next.
func TestRouteTraceFollowsAFailover(t *testing.T) {
	exec := &publishingExecutor{scriptedExecutor: &scriptedExecutor{script: []error{throttled}}}
	mgr, ctx := newPublishingManager(t, exec, "tr-a", "tr-b")

	if err := execute(mgr, ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls := exec.Calls()
	if len(calls) != 2 || calls[0] == calls[1] {
		t.Fatalf("calls = %v, want a failover to the other account", calls)
	}
	steps := exec.rec.Snapshot().Steps
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want the two attempts", steps)
	}
	failed, served := steps[0], steps[1]
	if failed.Kind != routetrace.KindAttempt || failed.Attempt != 1 || failed.AuthID != calls[0] || failed.Provider != "flaky" || failed.Model != "test-model" {
		t.Fatalf("first attempt = %+v", failed)
	}
	if failed.Outcome != routetrace.OutcomeFailed || failed.Status != http.StatusTooManyRequests || failed.Cause != CooldownReasonRateLimited || failed.Error == "" {
		t.Fatalf("first attempt outcome = %+v", failed)
	}
	if failed.Blocked != SimulatedBlockUnavailable || failed.BlockedUntil == nil || !failed.BlockedUntil.After(time.Now()) {
		t.Fatalf("the first attempt must say how long its account is out: %+v", failed)
	}
	if failed.Optimistic || failed.CoolingUntil != nil {
		t.Fatalf("an attempt on a healthy account is not optimistic: %+v", failed)
	}
	if served.Kind != routetrace.KindAttempt || served.Attempt != 2 || served.AuthID != calls[1] || served.Outcome != routetrace.OutcomeSuccess || served.Blocked != "" {
		t.Fatalf("second attempt = %+v", served)
	}

	rows := exec.Rows()
	if len(rows) != 2 || rows[0].authID != calls[0] || rows[1].authID != calls[1] {
		t.Fatalf("rows = %+v, want one per attempt, in order", rows)
	}
	if held := rows[0].trace.Steps; len(held) != 2 || held[0].Outcome != routetrace.OutcomeFailed || held[1].AuthID != calls[1] || held[1].Outcome != "" {
		t.Fatalf("the failed attempt's row must show the request moving on: %+v", held)
	}
	if last := rows[1].trace.Steps; len(last) != 2 || last[1].Outcome != routetrace.OutcomeSuccess {
		t.Fatalf("the serving attempt's row must carry its success: %+v", last)
	}
}

// The cooldown a failure starts can name its cause better than the failure:
// the 429 of an exhausted quota is quota_exhausted, not a rate limit. A
// malformed request keeps its own cause, and does not fail over.
func TestRouteTraceNamesWhatTheFailureLeaves(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		blocked string
		cause   string
		calls   int
	}{
		{"exhausted quota", quotaGone, SimulatedBlockCooldown, CooldownReasonQuotaExhausted, 2},
		// The timeout parks the account as an upstream error; the trace keeps
		// the more telling cause.
		{"first token timeout", &Error{HTTPStatus: http.StatusGatewayTimeout, Code: FirstTokenTimeoutCode, Message: "no first token within 30s"}, SimulatedBlockUnavailable, routetrace.CauseFirstTokenTimeout, 2},
		{"revoked key", revoked, SimulatedBlockUnavailable, CooldownReasonUnauthorized, 2},
		{"malformed request", &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: messages: field required"}, "", routetrace.CauseRequestInvalid, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &publishingExecutor{scriptedExecutor: &scriptedExecutor{script: []error{tc.failure}}}
			mgr, ctx := newPublishingManager(t, exec, "tr-a", "tr-b")
			err := execute(mgr, ctx)
			if calls := exec.Calls(); len(calls) != tc.calls {
				t.Fatalf("calls = %v, want %d (err=%v)", calls, tc.calls, err)
			}
			first := exec.rec.Snapshot().Steps[0]
			if first.Outcome != routetrace.OutcomeFailed || first.Blocked != tc.blocked || first.Cause != tc.cause {
				t.Fatalf("first attempt = %+v, want blocked %q for %q", first, tc.blocked, tc.cause)
			}
			if (tc.blocked != "") != (first.BlockedUntil != nil) {
				t.Fatalf("blocked until = %v for blocked %q", first.BlockedUntil, tc.blocked)
			}
			// Every record is out once the request is over, the last failure's
			// included: no decision follows it but returning.
			if rows := exec.Rows(); len(rows) != tc.calls {
				t.Fatalf("published %d records, want %d", len(rows), tc.calls)
			}
		})
	}
}

// A request that finds its only account at its concurrency limit records the
// skip, the queue and the attempt the queue led to. While it waits it is
// queued.
func TestRouteTraceRecordsAQueueWait(t *testing.T) {
	auth := queueTestAuth("tr-queue", 1)
	mgr, exec := newQueueTestManager(t, 5*time.Second, auth)
	busy := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		busy <- err
	}()
	<-exec.started

	ctx, rec := tracedContext()
	queued := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(ctx, []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		queued <- err
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), auth.ID, 1)
	if stage := rec.Live().Stage; stage != routetrace.StageQueued {
		t.Fatalf("a request waiting for a slot is %q, want %q", stage, routetrace.StageQueued)
	}
	close(exec.release)
	for _, done := range []chan error{busy, queued} {
		if err := <-done; err != nil {
			t.Fatalf("request failed: %v", err)
		}
	}

	steps := rec.Snapshot().Steps
	if len(steps) != 3 {
		t.Fatalf("steps = %+v, want skip, queue and attempt", steps)
	}
	skip, queue, attempt := steps[0], steps[1], steps[2]
	if skip.Kind != routetrace.KindSkip || skip.Reason != routetrace.SkipSaturated || skip.AuthID != auth.ID {
		t.Fatalf("skip = %+v", skip)
	}
	if queue.Kind != routetrace.KindQueue || queue.Accounts != 1 || queue.AuthID != auth.ID || queue.Outcome != routetrace.OutcomeSuccess {
		t.Fatalf("queue = %+v", queue)
	}
	if attempt.Kind != routetrace.KindAttempt || attempt.Attempt != 1 || attempt.AuthID != auth.ID || attempt.Outcome != routetrace.OutcomeSuccess {
		t.Fatalf("attempt = %+v", attempt)
	}
}

// A queue that times out ends the request: the trace says the queue failed and
// what the request was answered.
func TestRouteTraceRecordsAQueueTimeout(t *testing.T) {
	auth := queueTestAuth("tr-queue-timeout", 1)
	mgr, exec := newQueueTestManager(t, 50*time.Millisecond, auth)
	defer close(exec.release)
	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
	}()
	<-exec.started

	ctx, rec := tracedContext()
	if _, err := mgr.Execute(ctx, []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("the queued request must fail once its wait times out")
	}
	steps := rec.Snapshot().Steps
	if len(steps) != 3 {
		t.Fatalf("steps = %+v, want skip, queue and the exhausted pass", steps)
	}
	if queue := steps[1]; queue.Kind != routetrace.KindQueue || queue.Outcome != routetrace.OutcomeFailed || queue.Error == "" || queue.AuthID != "" {
		t.Fatalf("queue = %+v", queue)
	}
	if exhausted := steps[2]; exhausted.Kind != routetrace.KindExhausted || exhausted.Status != http.StatusTooManyRequests {
		t.Fatalf("exhausted = %+v", exhausted)
	}
}

// A busy wait shows as the pass that found every account cooling, with each
// account's state soonest back first, the wait, and the attempt after it.
func TestRouteTraceRecordsABusyWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "bw-b", "bw-a")
		start := time.Now()
		park(mgr, "bw-b", throttled, 40*time.Second)
		park(mgr, "bw-a", upstreamDown, 15*time.Second)

		ctx, rec := tracedContext()
		if err := execute(mgr, ctx); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		steps := rec.Snapshot().Steps
		if len(steps) != 3 {
			t.Fatalf("steps = %+v, want the exhausted pass, the wait and the attempt", steps)
		}
		exhausted, wait, served := steps[0], steps[1], steps[2]
		if exhausted.Kind != routetrace.KindExhausted || exhausted.Status == 0 || exhausted.RetryAfterMs != 15000 || len(exhausted.Candidates) != 2 {
			t.Fatalf("exhausted = %+v", exhausted)
		}
		for i, want := range []struct {
			id     string
			after  time.Duration
			cause  string
			status int
		}{
			{"bw-a", 15 * time.Second, CooldownReasonUpstreamError, http.StatusServiceUnavailable},
			{"bw-b", 40 * time.Second, CooldownReasonRateLimited, http.StatusTooManyRequests},
		} {
			got := exhausted.Candidates[i]
			if got.AuthID != want.id || got.Provider != "flaky" || got.State != SimulatedBlockUnavailable || got.Cause != want.cause || got.Status != want.status {
				t.Fatalf("candidate %d = %+v, want %s cooling for %s", i, got, want.id, want.cause)
			}
			if got.Until == nil || !got.Until.Equal(start.Add(want.after)) {
				t.Fatalf("candidate %s back at %v, want %v", got.AuthID, got.Until, start.Add(want.after))
			}
		}
		if wait.Kind != routetrace.KindWait || wait.WaitKind != SimulatedRetryWaitBusyWait || wait.PlannedMs < 15000 || wait.DurationMs != wait.PlannedMs || wait.Outcome != routetrace.OutcomeSuccess {
			t.Fatalf("wait = %+v", wait)
		}
		if served.Kind != routetrace.KindAttempt || served.AuthID != "bw-a" || served.Outcome != routetrace.OutcomeSuccess {
			t.Fatalf("attempt = %+v", served)
		}
	})
}

// The request-retry wait is told apart from a busy wait. (The account is out
// for longer than optimisticRetryWindow, or the request would try it at once.)
func TestRouteTraceRecordsARequestRetryWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newPlainManager(t, exec, "rr-a")
		mgr.SetRetryConfig(1, 30*time.Second)
		park(mgr, "rr-a", upstreamDown, 15*time.Second)

		ctx, rec := tracedContext()
		if err := execute(mgr, ctx); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		steps := rec.Snapshot().Steps
		if len(steps) != 3 || steps[0].Kind != routetrace.KindExhausted || steps[2].Outcome != routetrace.OutcomeSuccess {
			t.Fatalf("steps = %+v", steps)
		}
		if wait := steps[1]; wait.Kind != routetrace.KindWait || wait.WaitKind != SimulatedRetryWaitRequestRetry || wait.PlannedMs != 15000 {
			t.Fatalf("wait = %+v", wait)
		}
	})
}

// When every account is parked only for a moment the scheduler tries the one
// back soonest; the trace says the attempt went through a cooldown, and until
// when that cooldown ran.
func TestRouteTraceMarksAnOptimisticAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newPlainManager(t, exec, "op-a", "op-b")
		start := time.Now()
		park(mgr, "op-a", upstreamDown, 5*time.Second)
		park(mgr, "op-b", upstreamDown, 3*time.Second)

		ctx, rec := tracedContext()
		if err := execute(mgr, ctx); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		steps := rec.Snapshot().Steps
		if len(steps) != 1 {
			t.Fatalf("steps = %+v, want the one attempt", steps)
		}
		attempt := steps[0]
		if attempt.AuthID != "op-b" || !attempt.Optimistic || attempt.CoolingUntil == nil || !attempt.CoolingUntil.Equal(start.Add(3*time.Second)) {
			t.Fatalf("attempt = %+v, want an optimistic attempt on op-b, cooling until %v", attempt, start.Add(3*time.Second))
		}
	})
}

// When the request has failed on every account, its last pass lists them as
// tried, with when each is back.
func TestRouteTraceListsTheTriedAccountsWhenThePoolRunsOut(t *testing.T) {
	exec := &scriptedExecutor{script: []error{upstreamDown, upstreamDown}}
	mgr := newPlainManager(t, exec, "tr-a", "tr-b")
	ctx, rec := tracedContext()
	if err := execute(mgr, ctx); err == nil {
		t.Fatal("Execute succeeded, want the last failure")
	}
	steps := rec.Snapshot().Steps
	if len(steps) != 3 || steps[2].Kind != routetrace.KindExhausted {
		t.Fatalf("steps = %+v, want two attempts and the exhausted pass", steps)
	}
	exhausted := steps[2]
	if exhausted.Status != http.StatusServiceUnavailable || len(exhausted.Candidates) != 2 {
		t.Fatalf("exhausted = %+v", exhausted)
	}
	for _, candidate := range exhausted.Candidates {
		if candidate.State != routetrace.CandidateTried || candidate.Until == nil || candidate.Cause != CooldownReasonUpstreamError {
			t.Fatalf("candidate = %+v, want tried and cooling", candidate)
		}
	}
}

// A request whose client has left lists nothing: describing the pool would
// cost the scheduler for a trace nobody waits for.
func TestRouteTraceSkipsTheCandidatesOfACanceledRequest(t *testing.T) {
	exec := &scriptedExecutor{}
	mgr := newPlainManager(t, exec, "tr-a")
	park(mgr, "tr-a", upstreamDown, time.Minute)
	ctx, rec := tracedContext()
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_ = execute(mgr, ctx)
	if trace := rec.Snapshot(); trace != nil {
		t.Fatalf("trace = %+v, want nothing recorded", trace)
	}
}

// A stream that fails before its first byte fails over like any request, and
// a stream records when its first token came.
func TestRouteTraceFollowsAStreamFailover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &publishingExecutor{scriptedExecutor: &scriptedExecutor{script: []error{upstreamDown}}, firstToken: 250 * time.Millisecond}
		mgr, ctx := newPublishingManager(t, exec, "st-a", "st-b")

		result, err := executeStream(mgr, ctx)
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		synctest.Wait()
		if stage := exec.rec.Live().Stage; stage != routetrace.StageFirstToken {
			t.Fatalf("an open stream waiting for data is %q, want %q", stage, routetrace.StageFirstToken)
		}
		if err := drainStream(result); err != nil {
			t.Fatalf("stream: %v", err)
		}
		if stage := exec.rec.Live().Stage; stage != routetrace.StageStreaming {
			t.Fatalf("after its first token a stream is %q, want %q", stage, routetrace.StageStreaming)
		}
		steps := exec.rec.Snapshot().Steps
		if len(steps) != 2 {
			t.Fatalf("steps = %+v", steps)
		}
		failed, served := steps[0], steps[1]
		if failed.Outcome != routetrace.OutcomeFailed || !failed.BeforeFirstByte || failed.Cause != CooldownReasonUpstreamError {
			t.Fatalf("failed stream = %+v", failed)
		}
		if served.Outcome != routetrace.OutcomeSuccess || served.BeforeFirstByte || served.FirstTokenMs != 250 || served.DurationMs != 250 {
			t.Fatalf("served stream = %+v", served)
		}
		rows := exec.Rows()
		if len(rows) != 2 || len(rows[0].trace.Steps) != 2 || rows[1].trace.Steps[1].Outcome != routetrace.OutcomeSuccess {
			t.Fatalf("rows = %+v", rows)
		}
	})
}

// A stream whose first chunk is an error has delivered nothing, so the
// handler may still move it to another account (its bootstrap retry calls
// ExecuteStream again). The failed attempt says so, and its record waits for
// that call, so the row shows where the request went.
func TestRouteTraceMarksAStreamThatFailedBeforeItsFirstByte(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &publishingExecutor{scriptedExecutor: &scriptedExecutor{}, streamErrs: []error{upstreamDown}}
		mgr, ctx := newPublishingManager(t, exec, "st-a", "st-b")

		for call := 0; call < 2; call++ {
			result, err := executeStream(mgr, ctx)
			if err != nil {
				t.Fatalf("ExecuteStream %d: %v", call, err)
			}
			if streamErr := drainStream(result); (streamErr != nil) != (call == 0) {
				t.Fatalf("stream %d ended with %v", call, streamErr)
			}
			synctest.Wait()
			if rows := exec.Rows(); call == 0 && len(rows) != 0 {
				t.Fatalf("the failed stream's record must wait for the handler's decision: %+v", rows)
			}
		}
		steps := exec.rec.Snapshot().Steps
		if len(steps) != 2 {
			t.Fatalf("steps = %+v", steps)
		}
		if failed := steps[0]; failed.Outcome != routetrace.OutcomeFailed || !failed.BeforeFirstByte || failed.Status != http.StatusServiceUnavailable {
			t.Fatalf("failed stream = %+v", failed)
		}
		if served := steps[1]; served.Attempt != 2 || served.AuthID == steps[0].AuthID || served.Outcome != routetrace.OutcomeSuccess {
			t.Fatalf("served stream = %+v", served)
		}
		rows := exec.Rows()
		if len(rows) != 2 || rows[0].authID != steps[0].AuthID || len(rows[0].trace.Steps) != 2 || rows[0].trace.Steps[1].AuthID != steps[1].AuthID {
			t.Fatalf("the failed stream's row must show the retry: %+v", rows)
		}
	})
}

// A stream that fails after its first data cannot move to another account, so
// it settles as it ends.
func TestRouteTraceSettlesAStreamThatFailedMidway(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &publishingExecutor{scriptedExecutor: &scriptedExecutor{}, streamErrs: []error{upstreamDown}, failAfterData: true}
		mgr, ctx := newPublishingManager(t, exec, "st-a", "st-b")
		result, err := executeStream(mgr, ctx)
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		if streamErr := drainStream(result); streamErr == nil {
			t.Fatal("the stream must fail")
		}
		synctest.Wait()
		rows := exec.Rows()
		if len(rows) != 1 || len(rows[0].trace.Steps) != 1 {
			t.Fatalf("rows = %+v, want the record out as the stream ended", rows)
		}
		if step := rows[0].trace.Steps[0]; step.Outcome != routetrace.OutcomeFailed || step.BeforeFirstByte {
			t.Fatalf("step = %+v, want a failure after the first byte", step)
		}
	})
}

// A handler that does not retry such a stream ends the request instead, which
// settles it.
func TestRouteTraceSettlesAFailedStreamWhenTheRequestEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &publishingExecutor{scriptedExecutor: &scriptedExecutor{}, streamErrs: []error{upstreamDown}}
		mgr, ctx := newPublishingManager(t, exec, "st-a")
		result, err := executeStream(mgr, ctx)
		if err != nil {
			t.Fatalf("ExecuteStream: %v", err)
		}
		if streamErr := drainStream(result); streamErr == nil {
			t.Fatal("the stream must fail")
		}
		synctest.Wait()
		if rows := exec.Rows(); len(rows) != 0 {
			t.Fatalf("rows = %+v, want the record held", rows)
		}
		exec.rec.Close()
		if rows := exec.Rows(); len(rows) != 1 || len(rows[0].trace.Steps) != 1 {
			t.Fatalf("rows = %+v, want the record out with the request's trace", rows)
		}
	})
}

func TestRouteTraceMarksAPinnedRequest(t *testing.T) {
	exec := &scriptedExecutor{}
	mgr := newPlainManager(t, exec, "tr-a", "tr-b")
	ctx, rec := tracedContext()
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "tr-b"}}
	if _, err := mgr.Execute(ctx, []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, opts); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	trace := rec.Snapshot()
	if !trace.Pinned || len(trace.Steps) != 1 || trace.Steps[0].AuthID != "tr-b" {
		t.Fatalf("trace = %+v, want one attempt on the pinned account", trace)
	}
}

func TestRouteTraceRecordsAnAttemptTheClientLeft(t *testing.T) {
	auth := queueTestAuth("tr-cancel", 1)
	mgr, exec := newQueueTestManager(t, time.Second, auth)
	defer close(exec.release)
	ctx, rec := tracedContext()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(ctx, []string{"blocking"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
		done <- err
	}()
	<-exec.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v, want the cancellation", err)
	}
	steps := rec.Snapshot().Steps
	if len(steps) != 1 || steps[0].Outcome != routetrace.OutcomeCanceled {
		t.Fatalf("steps = %+v, want one canceled attempt", steps)
	}
}

// A token count leaves the account's state alone, so its failure is described
// from the error alone.
func TestRouteTraceDescribesATokenCountFailureFromItsError(t *testing.T) {
	exec := &scriptedExecutor{script: []error{throttled}}
	mgr := newPlainManager(t, exec, "tr-a", "tr-b")
	ctx, rec := tracedContext()
	if _, err := mgr.ExecuteCount(ctx, []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("ExecuteCount: %v", err)
	}
	steps := rec.Snapshot().Steps
	if len(steps) != 2 || steps[1].Outcome != routetrace.OutcomeSuccess {
		t.Fatalf("steps = %+v", steps)
	}
	if failed := steps[0]; failed.Outcome != routetrace.OutcomeFailed || failed.Cause != CooldownReasonRateLimited || failed.Blocked != "" || failed.BlockedUntil != nil {
		t.Fatalf("failed count = %+v", failed)
	}
}

func TestFailureCauseNamesWhatWentWrong(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"malformed request":   {&Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: bad"}, routetrace.CauseRequestInvalid},
		"content policy":      {&Error{HTTPStatus: http.StatusBadRequest, Code: contentPolicyViolationCode, Message: "blocked"}, routetrace.CauseContentPolicy},
		"first token timeout": {&Error{HTTPStatus: http.StatusGatewayTimeout, Code: FirstTokenTimeoutCode, Message: "no first token"}, routetrace.CauseFirstTokenTimeout},
		"stream incomplete":   {&Error{HTTPStatus: http.StatusBadGateway, Code: "response_stream_incomplete", Message: "cut"}, routetrace.CauseStreamIncomplete},
		"rate limit":          {throttled, CooldownReasonRateLimited},
		"server error":        {upstreamDown, CooldownReasonUpstreamError},
		"revoked key":         {revoked, CooldownReasonUnauthorized},
		"bare status":         {&retryAfterError{status: http.StatusTooManyRequests, message: "slow down", retryAfter: time.Second}, CooldownReasonRateLimited},
		"no answer":           {errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), routetrace.CauseNetwork},
		"unclassified":        {&Error{HTTPStatus: http.StatusTeapot, Message: "teapot"}, ""},
	} {
		if got := failureCause(tc.err); got != tc.want {
			t.Errorf("%s: failureCause = %q, want %q", name, got, tc.want)
		}
	}
	if got := tracedFailure(context.Canceled); got.Outcome != routetrace.OutcomeCanceled || got.Cause != "" {
		t.Errorf("a canceled attempt = %+v", got)
	}
}
