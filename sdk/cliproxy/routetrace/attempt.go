package routetrace

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type attemptState uint8

const (
	// attemptOpen: the executor is running, or a stream is delivering.
	attemptOpen attemptState = iota
	// attemptPending: failed, waiting for the scheduler's next decision.
	attemptPending
	// attemptSettled: its records are out; later ones go out at once.
	attemptSettled
)

// Attempt is one upstream attempt of a traced request. Its methods are no-ops
// on a nil Attempt.
type Attempt struct {
	rec   *Recorder
	step  *Step
	begun time.Time
	// Guarded by rec.mu.
	state      attemptState
	held       []func()
	firstToken bool
}

type attemptKey struct{}

// Context returns ctx carrying the attempt, for the executor to run under: the
// usage records it publishes under that context wait for the attempt to
// settle.
func (a *Attempt) Context(ctx context.Context) context.Context {
	if a == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, attemptKey{}, a)
}

// DeferPublish holds publish until the attempt ctx carries is settled and
// reports whether it did. Without an unsettled attempt in ctx it returns false,
// and the caller publishes at once.
func DeferPublish(ctx context.Context, publish func()) bool {
	if ctx == nil || publish == nil {
		return false
	}
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	if a == nil {
		return false
	}
	r := a.rec
	r.mu.Lock()
	defer r.mu.Unlock()
	// Close settles every attempt and BeginAttempt hands out settled ones
	// after it, so a closed recorder holds nothing either.
	if a.state == attemptSettled {
		return false
	}
	a.held = append(a.held, publish)
	return true
}

// MaxUpstreamRequestShapeBytes bounds the shape one attempt may carry. The trace
// holds up to headSteps+tailSteps attempts and is written into every request log
// row of the request, so a shape is a few hundred bytes of counts, not a copy of
// anything the client sent.
const MaxUpstreamRequestShapeBytes = 4 << 10

// AttachUpstreamRequestShape records the shape of the request the executor sent
// on the attempt ctx carries, and reports whether it did. An attempt that sent
// twice (an executor retrying on its own) keeps the last shape, the one whose
// answer the attempt ends with.
//
// build runs only when ctx carries an attempt, and outside the recorder's lock:
// building a shape reads the whole request body, which an executor with nowhere
// to put the result must not pay for. A shape that is empty or larger than
// MaxUpstreamRequestShapeBytes is dropped.
func AttachUpstreamRequestShape(ctx context.Context, build func() json.RawMessage) bool {
	if ctx == nil || build == nil {
		return false
	}
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	if a == nil {
		return false
	}
	shape := build()
	if len(shape) == 0 || len(shape) > MaxUpstreamRequestShapeBytes {
		return false
	}
	r := a.rec
	r.mu.Lock()
	a.step.UpstreamRequestShape = shape
	r.mu.Unlock()
	return true
}

// Finish records how the attempt ended; only the first outcome counts. A
// success settles the attempt at once. A failure waits for the scheduler's
// next decision, which settles it; a caller that makes no further decision (a
// stream ending on its own) settles it with Settle.
func (a *Attempt) Finish(o Outcome) {
	if a == nil {
		return
	}
	r := a.rec
	now := time.Now()
	r.mu.Lock()
	step := a.step
	if step.Outcome != "" {
		r.mu.Unlock()
		return
	}
	step.DurationMs = now.Sub(a.begun).Milliseconds()
	step.Outcome = o.Outcome
	step.Status = o.Status
	step.Cause = o.Cause
	step.Error = truncateError(o.Error)
	step.Blocked = o.Blocked
	if !o.BlockedUntil.IsZero() {
		until := stamp(o.BlockedUntil)
		step.BlockedUntil = &until
	}
	step.BeforeFirstByte = o.BeforeFirstByte
	var held []func()
	switch {
	case o.Outcome == OutcomeSuccess:
		held = r.settleLocked(a)
	case a.state == attemptOpen:
		a.state = attemptPending
	}
	if o.Outcome != OutcomeSuccess && r.current == step {
		r.setStageLocked(StagePreparing, now)
	}
	r.mu.Unlock()
	runHeld(held)
}

// Settle publishes what the attempt holds; anything published under its
// context afterwards goes out at once.
func (a *Attempt) Settle() {
	if a == nil {
		return
	}
	r := a.rec
	r.mu.Lock()
	held := r.settleLocked(a)
	r.mu.Unlock()
	runHeld(held)
}

// Streaming marks the attempt's stream as handed over and waiting for its
// first data.
func (a *Attempt) Streaming() {
	if a == nil {
		return
	}
	r := a.rec
	r.mu.Lock()
	if r.current == a.step && !a.firstToken {
		r.setStageLocked(StageFirstToken, time.Now())
	}
	r.mu.Unlock()
}

// FirstToken marks the arrival of the stream's first data.
func (a *Attempt) FirstToken() {
	if a == nil {
		return
	}
	r := a.rec
	now := time.Now()
	r.mu.Lock()
	if !a.firstToken {
		a.firstToken = true
		a.step.FirstTokenMs = now.Sub(a.begun).Milliseconds()
		if r.current == a.step {
			r.setStageLocked(StageStreaming, now)
		}
	}
	r.mu.Unlock()
}

// Span is a queue or wait step in progress. Its methods are no-ops on a nil
// Span.
type Span struct {
	rec   *Recorder
	step  *Step
	begun time.Time
}

// Granted names the account a queue got its slot on.
func (s *Span) Granted(info AttemptInfo) {
	if s == nil {
		return
	}
	s.rec.mu.Lock()
	info.applyTo(s.step)
	s.rec.mu.Unlock()
}

// End records how the queue or wait ended: err is nil when it ran its course.
func (s *Span) End(err error) {
	if s == nil {
		return
	}
	r := s.rec
	now := time.Now()
	r.mu.Lock()
	s.step.DurationMs = now.Sub(s.begun).Milliseconds()
	switch {
	case err == nil:
		s.step.Outcome = OutcomeSuccess
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		s.step.Outcome = OutcomeCanceled
	default:
		s.step.Outcome = OutcomeFailed
		s.step.Error = truncateError(err.Error())
	}
	r.setStageLocked(StagePreparing, now)
	r.mu.Unlock()
}
