package routetrace

import (
	"context"
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
