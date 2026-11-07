package routetrace

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func info(id string) AttemptInfo {
	return AttemptInfo{Group: "g", AuthID: id, AuthLabel: "label-" + id, Provider: "codex", Model: "gpt-x"}
}

// heldPublish is what a usage reporter hands DeferPublish: when it runs it
// takes the trace as it is then, the way the request log detail is built.
type heldPublish struct {
	mu    sync.Mutex
	rec   *Recorder
	ran   int
	trace *Trace
}

func (h *heldPublish) fn() func() {
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.ran++
		h.trace = h.rec.Snapshot()
	}
}

func (h *heldPublish) runs() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ran
}

func (h *heldPublish) seen() *Trace {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.trace
}

func TestNilRecorderAndAttemptAreNoOps(t *testing.T) {
	var r *Recorder
	r.SetRequest("m", true, true)
	a := r.BeginAttempt(info("a"))
	if a != nil {
		t.Fatal("nil recorder must not hand out attempts")
	}
	a.Finish(Outcome{Outcome: OutcomeFailed})
	a.Streaming()
	a.FirstToken()
	a.Settle()
	r.Skip(SkipSaturated, info("a"))
	r.BeginQueue(1).End(nil)
	r.BeginWait("busy-wait", time.Second).End(nil)
	r.Exhausted(Exhaustion{})
	r.SettlePending()
	r.Restart()
	r.Idle()
	r.Close()
	if r.Snapshot() != nil || r.Live() != (Live{}) {
		t.Fatal("nil recorder must report nothing")
	}
	ctx := a.Context(context.Background())
	if DeferPublish(ctx, func() {}) {
		t.Fatal("a context without an attempt must publish at once")
	}
	if FromContext(WithRecorder(context.Background(), nil)) != nil {
		t.Fatal("WithRecorder(nil) must not attach anything")
	}
}

// A failed attempt's records wait for the next decision, so the row says the
// request moved on to another account.
func TestFailedAttemptHoldsRecordsUntilTheNextAttemptBegins(t *testing.T) {
	r := New()
	held := &heldPublish{rec: r}
	first := r.BeginAttempt(info("a"))
	if !DeferPublish(first.Context(context.Background()), held.fn()) {
		t.Fatal("a running attempt must hold its records")
	}
	until := time.Now().Add(10 * time.Minute)
	first.Finish(Outcome{Outcome: OutcomeFailed, Status: 429, Cause: "rate_limited", Error: "slow down", Blocked: "unavailable", BlockedUntil: until})
	if held.runs() != 0 {
		t.Fatal("a failed attempt must wait for the scheduler's next decision")
	}
	second := r.BeginAttempt(info("b"))
	if held.runs() != 1 {
		t.Fatalf("records published %d times, want once when the next attempt begins", held.runs())
	}
	trace := held.seen()
	if len(trace.Steps) != 2 || trace.Steps[1].AuthID != "b" || trace.Steps[1].Outcome != "" {
		t.Fatalf("the held row must show the next attempt, still running: %+v", trace.Steps)
	}
	got := trace.Steps[0]
	if got.Attempt != 1 || got.Outcome != OutcomeFailed || got.Status != 429 || got.Cause != "rate_limited" || got.Blocked != "unavailable" {
		t.Fatalf("first attempt = %+v", got)
	}
	if got.BlockedUntil == nil || !got.BlockedUntil.Equal(until.Truncate(time.Millisecond)) {
		t.Fatalf("blocked until = %v, want %v", got.BlockedUntil, until)
	}
	if trace.Steps[1].Attempt != 2 {
		t.Fatalf("second attempt number = %d", trace.Steps[1].Attempt)
	}
	// Settled: what the first attempt's executor publishes now goes out at once.
	if DeferPublish(first.Context(context.Background()), func() {}) {
		t.Fatal("a settled attempt must not hold records any more")
	}
	second.Finish(Outcome{Outcome: OutcomeSuccess})
}

func TestSuccessSettlesAtOnce(t *testing.T) {
	r := New()
	held := &heldPublish{rec: r}
	a := r.BeginAttempt(info("a"))
	DeferPublish(a.Context(context.Background()), held.fn())
	a.Finish(Outcome{Outcome: OutcomeSuccess})
	if held.runs() != 1 {
		t.Fatal("a success must publish its records at once")
	}
	if step := held.seen().Steps[0]; step.Outcome != OutcomeSuccess {
		t.Fatalf("the row must carry its own outcome: %+v", step)
	}
	a.Finish(Outcome{Outcome: OutcomeFailed})
	if step := r.Snapshot().Steps[0]; step.Outcome != OutcomeSuccess {
		t.Fatal("only the first outcome counts")
	}
}

// Every decision that may block publishes what waits first: a long queue or
// cooldown wait must not hold a finished attempt's row.
func TestQueueWaitAndSettlePendingPublishWaitingRecords(t *testing.T) {
	for name, decide := range map[string]func(*Recorder){
		"queue":   func(r *Recorder) { r.BeginQueue(2) },
		"wait":    func(r *Recorder) { r.BeginWait("busy-wait", time.Minute) },
		"settle":  func(r *Recorder) { r.SettlePending() },
		"close":   func(r *Recorder) { r.Close() },
		"restart": func(r *Recorder) { r.Restart() },
		"idle":    func(r *Recorder) { r.Idle() },
	} {
		t.Run(name, func(t *testing.T) {
			r := New()
			held := &heldPublish{rec: r}
			a := r.BeginAttempt(info("a"))
			DeferPublish(a.Context(context.Background()), held.fn())
			a.Finish(Outcome{Outcome: OutcomeFailed, Status: 503})
			r.Skip(SkipSaturated, info("b"))
			r.Exhausted(Exhaustion{Status: 429})
			if held.runs() != 0 {
				t.Fatal("a skip or an exhausted pass is not the decision after a failure")
			}
			decide(r)
			if held.runs() != 1 {
				t.Fatalf("records published %d times, want 1", held.runs())
			}
		})
	}
}

func TestCloseSettlesRunningAttemptsAndRefusesLaterHolds(t *testing.T) {
	r := New()
	held := &heldPublish{rec: r}
	a := r.BeginAttempt(info("a"))
	ctx := a.Context(context.Background())
	DeferPublish(ctx, held.fn())
	r.Close()
	if held.runs() != 1 {
		t.Fatal("Close must publish what a running attempt holds")
	}
	if DeferPublish(ctx, func() {}) {
		t.Fatal("after Close records must go out at once")
	}
	late := r.BeginAttempt(info("b"))
	if DeferPublish(late.Context(context.Background()), func() {}) {
		t.Fatal("an attempt begun after Close must not hold records")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.live) != 0 {
		t.Fatal("a closed recorder keeps no attempt to settle")
	}
}

func TestHeldPublishPanicDoesNotStopTheOthers(t *testing.T) {
	r := New()
	a := r.BeginAttempt(info("a"))
	ctx := a.Context(context.Background())
	ran := false
	DeferPublish(ctx, func() { panic("boom") })
	DeferPublish(ctx, func() { ran = true })
	a.Finish(Outcome{Outcome: OutcomeSuccess})
	if !ran {
		t.Fatal("a panicking record must not keep the next one from going out")
	}
}

func TestTraceKeepsHeadAndTail(t *testing.T) {
	r := New()
	total := headSteps + tailSteps + 5
	for i := 0; i < total; i++ {
		a := r.BeginAttempt(info(strings.Repeat("x", i%3+1)))
		a.Finish(Outcome{Outcome: OutcomeFailed})
	}
	trace := r.Snapshot()
	if len(trace.Steps) != headSteps+tailSteps {
		t.Fatalf("kept %d steps, want %d", len(trace.Steps), headSteps+tailSteps)
	}
	if trace.Omitted != 5 || trace.OmittedAfter != headSteps {
		t.Fatalf("omitted = %d after %d, want 5 after %d", trace.Omitted, trace.OmittedAfter, headSteps)
	}
	for i := 0; i < headSteps; i++ {
		if trace.Steps[i].Attempt != i+1 {
			t.Fatalf("head step %d is attempt %d", i, trace.Steps[i].Attempt)
		}
	}
	for i := 0; i < tailSteps; i++ {
		want := total - tailSteps + i + 1
		if got := trace.Steps[headSteps+i].Attempt; got != want {
			t.Fatalf("tail step %d is attempt %d, want %d", i, got, want)
		}
	}
}

func TestErrorTextIsBoundedOnARuneBoundary(t *testing.T) {
	// One ASCII byte first, so the byte limit falls inside a three-byte rune.
	message := "a" + strings.Repeat("限", maxErrorBytes)
	got := truncateError(message)
	if !utf8.ValidString(got) || len(got) > maxErrorBytes+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated to %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
	if truncateError("short") != "short" {
		t.Fatal("a short message must stay as it is")
	}
}

func TestExhaustedKeepsTheFirstCandidates(t *testing.T) {
	r := New()
	candidates := make([]Candidate, MaxCandidates+3)
	for i := range candidates {
		candidates[i] = Candidate{AuthID: string(rune('a' + i)), State: "cooldown"}
	}
	r.Exhausted(Exhaustion{Status: 429, Code: "model_cooldown", Message: "all cooling", RetryAfter: 90 * time.Second, Candidates: candidates})
	step := r.Snapshot().Steps[0]
	if step.Kind != KindExhausted || len(step.Candidates) != MaxCandidates || step.CandidatesOmitted != 3 {
		t.Fatalf("exhausted = %+v", step)
	}
	if step.Status != 429 || step.ErrorCode != "model_cooldown" || step.RetryAfterMs != 90000 || step.Candidates[0].AuthID != "a" {
		t.Fatalf("exhausted fields = %+v", step)
	}
	candidates[0].AuthID = "changed"
	if r.Snapshot().Steps[0].Candidates[0].AuthID != "a" {
		t.Fatal("the trace must keep its own copy of the candidates")
	}
}

func TestLiveFollowsTheStages(t *testing.T) {
	r := New()
	r.SetRequest("claude-x", true, false)
	r.SetRequest("other", false, true)
	live := r.Live()
	if live.Stage != StagePreparing || live.Model != "claude-x" || !live.Stream || !live.Pinned {
		t.Fatalf("live = %+v", live)
	}
	queue := r.BeginQueue(1)
	if r.Live().Stage != StageQueued {
		t.Fatal("a queue wait is queued")
	}
	queue.Granted(info("a"))
	queue.End(nil)
	a := r.BeginAttempt(info("a"))
	if live := r.Live(); live.Stage != StageUpstream || live.AuthID != "a" || live.Attempt != 1 || live.UpstreamModel != "gpt-x" {
		t.Fatalf("live = %+v", live)
	}
	a.Streaming()
	if r.Live().Stage != StageFirstToken {
		t.Fatal("an open stream waits for its first token")
	}
	a.FirstToken()
	if r.Live().Stage != StageStreaming {
		t.Fatal("after its first token a stream is streaming")
	}
	a.Finish(Outcome{Outcome: OutcomeFailed})
	if r.Live().Stage != StagePreparing {
		t.Fatal("after a failure the scheduler is deciding again")
	}
	wait := r.BeginWait("request-retry", 2*time.Second)
	if r.Live().Stage != StageWaiting {
		t.Fatal("a cooldown wait is waiting")
	}
	wait.End(context.Canceled)
	steps := r.Snapshot().Steps
	if steps[0].Kind != KindQueue || steps[0].AuthID != "a" || steps[0].Accounts != 1 || steps[0].Outcome != OutcomeSuccess {
		t.Fatalf("queue step = %+v", steps[0])
	}
	if steps[1].FirstTokenMs < 0 || steps[2].Kind != KindWait || steps[2].Outcome != OutcomeCanceled || steps[2].WaitKind != "request-retry" || steps[2].PlannedMs != 2000 {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestSpanEndRecordsAFailure(t *testing.T) {
	r := New()
	r.BeginQueue(3).End(errors.New("queue closed"))
	step := r.Snapshot().Steps[0]
	if step.Outcome != OutcomeFailed || step.Error != "queue closed" {
		t.Fatalf("queue step = %+v", step)
	}
}

// Restart starts a WebSocket connection's next request on an empty trace;
// what the previous request still held goes out with the previous trace.
func TestRestartAndIdle(t *testing.T) {
	r := New()
	r.SetRequest("m1", true, true)
	r.BeginAttempt(info("a")).Finish(Outcome{Outcome: OutcomeSuccess})
	lingering := r.BeginAttempt(info("b"))
	held := &heldPublish{rec: r}
	DeferPublish(lingering.Context(context.Background()), held.fn())
	r.Restart()
	if held.runs() != 1 || held.seen() == nil || len(held.seen().Steps) != 2 {
		t.Fatalf("a record held at Restart must go out with its own request's trace, saw %+v", held.seen())
	}
	r.BeginAttempt(info("c")).Finish(Outcome{Outcome: OutcomeFailed})
	r.Idle()
	if r.Live().Stage != StageIdle {
		t.Fatal("Idle must mark the connection idle")
	}
	r.Restart()
	if r.Snapshot() != nil {
		t.Fatal("Restart must start an empty trace")
	}
	live := r.Live()
	if live.Stage != StagePreparing || live.Model != "" || live.Pinned || live.AuthID != "" {
		t.Fatalf("live after Restart = %+v", live)
	}
	if a := r.BeginAttempt(info("b")); a.step.Attempt != 1 {
		t.Fatal("attempts count from 1 again after Restart")
	}
}

// The scheduler, stream goroutines and the request log all touch one
// recorder; run with -race.
func TestRecorderIsSafeForConcurrentUse(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				a := r.BeginAttempt(info("a"))
				ctx := a.Context(context.Background())
				DeferPublish(ctx, func() { _ = r.Snapshot() })
				a.Streaming()
				a.FirstToken()
				_ = r.Live()
				if i%2 == 0 {
					a.Finish(Outcome{Outcome: OutcomeSuccess})
				} else {
					a.Finish(Outcome{Outcome: OutcomeFailed})
					r.SettlePending()
				}
			}
		}()
	}
	wg.Wait()
	r.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.live) != 0 {
		t.Fatalf("%d attempts left unsettled", len(r.live))
	}
}

// The per-request cost on the hot path: a recorder, one attempt that holds
// its usage record and succeeds, and the request's end.
func BenchmarkTracedRequest(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	publish := func() {}
	for b.Loop() {
		r := New()
		r.SetRequest("gpt-x", true, false)
		a := r.BeginAttempt(info("a"))
		DeferPublish(a.Context(ctx), publish)
		a.Streaming()
		a.FirstToken()
		a.Finish(Outcome{Outcome: OutcomeSuccess})
		r.Close()
	}
}
