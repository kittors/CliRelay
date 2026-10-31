// Package routetrace records how the scheduler placed one request: each
// upstream attempt with its outcome and the state it left the account in, the
// accounts passed over for being at their concurrency limit, the waits for a
// slot or for a cooldown to end, and, when no account could take the request,
// why each candidate could not.
//
// A request carries one Recorder from the moment the gateway accepts it. The
// trace is written into the request log with the rest of the request's
// diagnostic (detail_content.diagnostic.route_trace), and the recorder's live
// state feeds the in-flight requests view.
//
// The request log has a row per upstream attempt, written by the executor that
// ran it, and the executor publishes that row before the scheduler has seen
// the attempt's result. Left alone, a failed attempt's row would know neither
// why the scheduler gave up on the account, nor how long the account cools
// down, nor what the request did next. An attempt therefore holds the records
// its executor publishes (DeferPublish) until the scheduler settles it: a
// success at once, a failure when the scheduler takes its next decision
// (another account, a queue or retry wait, or giving up). Every path settles,
// and Close settles whatever is left when the request ends, so a record is
// delayed by the scheduler's own bookkeeping, never lost.
package routetrace

import (
	"context"
	"sync"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
)

// Step kinds. The panel keys its wording on these values: add new ones, never
// rename.
const (
	// KindAttempt is a request sent to an upstream account.
	KindAttempt = "attempt"
	// KindSkip is an account picked but passed over without sending, for the
	// Reason given.
	KindSkip = "skip"
	// KindQueue is a wait for an account at its concurrency limit to free a
	// slot, once no idle account was left.
	KindQueue = "queue"
	// KindWait is a wait before the scheduler runs again (WaitKind says which).
	KindWait = "wait"
	// KindExhausted is a scheduling pass that found no account to take the
	// request; Candidates says why.
	KindExhausted = "exhausted"
)

// Outcomes of an attempt, a queue or a wait.
const (
	OutcomeSuccess  = "success"
	OutcomeFailed   = "failed"
	OutcomeCanceled = "canceled"
)

// Causes of a failed attempt the scheduler adds to its cooldown reasons (the
// CooldownReason* values of package auth: rate_limited, quota_exhausted,
// unauthorized, upstream_error and so on).
const (
	// CauseRequestInvalid is a request the upstream refused as malformed. It is
	// not the account's fault, so the request is not tried on another.
	CauseRequestInvalid = "request_invalid"
	// CauseContentPolicy is a request refused by the content moderation policy.
	CauseContentPolicy = "content_policy"
	// CauseFirstTokenTimeout is a stream that produced no data within the
	// channel group's first-token timeout.
	CauseFirstTokenTimeout = "first_token_timeout"
	// CauseStreamIncomplete is a stream that ended without its completion event.
	CauseStreamIncomplete = "stream_incomplete"
	// CauseNetwork is a failure with no HTTP status: the connection could not
	// be made or broke before an answer.
	CauseNetwork = "network_error"
)

// SkipSaturated is an account at its concurrency limit.
const SkipSaturated = "saturated"

// CandidateTried is an account this request already tried. Other candidate
// states are the SimulatedBlock* values of package auth (cooldown,
// unavailable, disabled, weight), or SkipSaturated.
const CandidateTried = "tried"

// Stages of a request in flight, as the in-flight view shows them.
const (
	// StagePreparing: accepted and not yet with the scheduler, or between two
	// of its decisions.
	StagePreparing = "preparing"
	// StageQueued: waiting for an account at its concurrency limit.
	StageQueued = "queued"
	// StageWaiting: waiting for a cooldown to end before trying again.
	StageWaiting = "waiting"
	// StageUpstream: sent, waiting for the upstream's answer.
	StageUpstream = "upstream"
	// StageFirstToken: the stream is open and nothing has arrived yet.
	StageFirstToken = "first_token"
	// StageStreaming: the stream is delivering.
	StageStreaming = "streaming"
	// StageIdle: a WebSocket connection between two of its requests. The
	// in-flight view leaves such connections out.
	StageIdle = "idle"
)

const (
	// headSteps and tailSteps bound a trace: the first headSteps and the last
	// tailSteps are kept and Trace.Omitted counts those in between. The head
	// keeps how the request started, the tail how it ended. Even a request
	// that fails over across a pool of thirty accounts fits whole, and a full
	// trace stays around 10 KB of the request log row.
	headSteps = 8
	tailSteps = 24
	// maxErrorBytes bounds an attempt's error text. The upstream's full answer
	// stays in the request log's own sections; the trace only names it.
	maxErrorBytes = 240
	// MaxCandidates bounds the accounts an exhausted step lists. Callers sort
	// soonest recovery first, so the ones dropped are the furthest from serving.
	MaxCandidates = 12
)

// Trace is a recorded trace, as the request log stores it.
type Trace struct {
	// Pinned marks a request bound to one account (a pinned credential or a
	// sticky session): a failure is not retried on another account.
	Pinned bool   `json:"pinned,omitempty"`
	Steps  []Step `json:"steps"`
	// Omitted steps were recorded between Steps[OmittedAfter-1] and
	// Steps[OmittedAfter] and dropped to bound the trace.
	Omitted      int `json:"omitted,omitempty"`
	OmittedAfter int `json:"omitted_after,omitempty"`
}

// Step is one thing the scheduler did. Which fields are set depends on Kind.
type Step struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	// DurationMs is how long the step took; 0 while it runs.
	DurationMs int64 `json:"duration_ms,omitempty"`

	// The account of an attempt or a skip, or the one a queue was granted.
	// Attempt is the attempt's 1-based number within the request.
	Attempt   int    `json:"attempt,omitempty"`
	Group     string `json:"group,omitempty"`
	AuthID    string `json:"auth_id,omitempty"`
	AuthLabel string `json:"auth_label,omitempty"`
	Provider  string `json:"provider,omitempty"`
	// Model is the model sent upstream, after the account's aliases.
	Model string `json:"model,omitempty"`
	// Optimistic marks an attempt on an account still cooling, until
	// CoolingUntil: when every account is parked only for a moment, the
	// scheduler tries the one back soonest rather than refuse the request.
	Optimistic   bool       `json:"optimistic,omitempty"`
	CoolingUntil *time.Time `json:"cooling_until,omitempty"`

	// How an attempt, a queue or a wait ended; empty while it runs.
	Outcome string `json:"outcome,omitempty"`
	Status  int    `json:"status,omitempty"`
	Cause   string `json:"cause,omitempty"`
	Error   string `json:"error,omitempty"`
	// Blocked is what a failed attempt left its account in (a SimulatedBlock*
	// value of package auth), and BlockedUntil when that ends; a zero time
	// with Blocked set means the account waits for a probe or an operator.
	Blocked      string     `json:"blocked,omitempty"`
	BlockedUntil *time.Time `json:"blocked_until,omitempty"`
	// FirstTokenMs is when a stream's first data arrived, from the attempt's
	// start.
	FirstTokenMs int64 `json:"first_token_ms,omitempty"`
	// BeforeFirstByte marks a stream that failed before anything reached the
	// client, which a handler may still retry on another account.
	BeforeFirstByte bool `json:"before_first_byte,omitempty"`

	// Reason is why a skip passed the account over.
	Reason string `json:"reason,omitempty"`
	// Accounts is how many accounts a queue waited on.
	Accounts int `json:"accounts,omitempty"`
	// WaitKind and PlannedMs say which wait a wait step took and for how long
	// the scheduler meant it (the auth package's SimulatedRetryWait* values).
	WaitKind  string `json:"wait_kind,omitempty"`
	PlannedMs int64  `json:"planned_ms,omitempty"`

	// What an exhausted pass returns, and the state of its candidates.
	ErrorCode         string      `json:"error_code,omitempty"`
	RetryAfterMs      int64       `json:"retry_after_ms,omitempty"`
	Candidates        []Candidate `json:"candidates,omitempty"`
	CandidatesOmitted int         `json:"candidates_omitted,omitempty"`
}

// Candidate is one account of an exhausted pass and why it could not serve.
type Candidate struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label,omitempty"`
	Provider string `json:"provider,omitempty"`
	// State is CandidateTried, SkipSaturated or a SimulatedBlock* value of
	// package auth; empty for an account that could have served, which means
	// the pass ended for a reason of its own (the error says which).
	State  string     `json:"state,omitempty"`
	Until  *time.Time `json:"until,omitempty"`
	Cause  string     `json:"cause,omitempty"`
	Status int        `json:"status,omitempty"`
}

// AttemptInfo names the account of an attempt, a skip or a granted queue.
type AttemptInfo struct {
	Group     string
	AuthID    string
	AuthLabel string
	Provider  string
	Model     string
	// Optimistic and CoolingUntil: the account was still cooling as the
	// attempt began (see Step).
	Optimistic   bool
	CoolingUntil time.Time
}

// Outcome is how an attempt ended.
type Outcome struct {
	Outcome         string
	Status          int
	Cause           string
	Error           string
	Blocked         string
	BlockedUntil    time.Time
	BeforeFirstByte bool
}

// Exhaustion is a pass that found no account.
type Exhaustion struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	// Candidates are all the pass's accounts; Exhausted keeps the first
	// MaxCandidates.
	Candidates []Candidate
}

// Live is where a request stands right now.
type Live struct {
	StartedAt  time.Time
	Model      string
	Stream     bool
	Pinned     bool
	Stage      string
	StageSince time.Time
	// The account of the attempt the request is on or last made, if any.
	Attempt       int
	Group         string
	AuthID        string
	AuthLabel     string
	Provider      string
	UpstreamModel string
}

// Recorder collects the trace of one request. It is safe for concurrent use,
// and every method is a no-op on a nil Recorder, so callers never check
// whether a request is traced.
type Recorder struct {
	mu       sync.Mutex
	head     []*Step
	tail     []*Step
	tailPos  int
	omitted  int
	attempts int
	pinned   bool
	// live holds the attempts not settled yet, running or waiting for the
	// scheduler's next decision.
	live   []*Attempt
	closed bool

	startedAt  time.Time
	model      string
	stream     bool
	stage      string
	stageSince time.Time
	current    *Step
}

// New returns the recorder of a request that starts now.
func New() *Recorder {
	now := time.Now()
	return &Recorder{startedAt: now, stage: StagePreparing, stageSince: now}
}

type recorderKey struct{}

// WithRecorder returns ctx carrying r.
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, r)
}

// FromContext returns the recorder ctx carries, or nil.
func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}

// SetRequest records what the request asks for. The first non-empty model
// wins: a retry pass runs for the same request.
func (r *Recorder) SetRequest(model string, stream, pinned bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.model == "" {
		r.model = model
		r.stream = stream
	}
	r.pinned = r.pinned || pinned
}

// BeginAttempt records an attempt on info's account and settles the attempts
// that were waiting for this decision: their rows say the request moved on to
// this account.
func (r *Recorder) BeginAttempt(info AttemptInfo) *Attempt {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.mu.Lock()
	r.attempts++
	step := &Step{Kind: KindAttempt, At: stamp(now), Attempt: r.attempts}
	info.applyTo(step)
	r.appendLocked(step)
	a := &Attempt{rec: r, step: step, begun: now}
	held := r.settlePendingLocked()
	if !r.closed {
		r.live = append(r.live, a)
	} else {
		a.state = attemptSettled
	}
	r.current = step
	r.setStageLocked(StageUpstream, now)
	r.mu.Unlock()
	runHeld(held)
	return a
}

// Skip records an account passed over for reason, without sending to it.
func (r *Recorder) Skip(reason string, info AttemptInfo) {
	if r == nil {
		return
	}
	step := &Step{Kind: KindSkip, At: stamp(time.Now()), Reason: reason}
	info.applyTo(step)
	r.mu.Lock()
	r.appendLocked(step)
	r.mu.Unlock()
}

// BeginQueue records that the request waits on accounts at their concurrency
// limit, and settles the attempts waiting for this decision first: the wait
// may be long, and their rows must not wait with it.
func (r *Recorder) BeginQueue(accounts int) *Span {
	return r.beginSpan(&Step{Kind: KindQueue, Accounts: accounts}, StageQueued)
}

// BeginWait records a wait of kind, planned for planned, before the scheduler
// runs again. Like BeginQueue it settles the waiting attempts first.
func (r *Recorder) BeginWait(kind string, planned time.Duration) *Span {
	return r.beginSpan(&Step{Kind: KindWait, WaitKind: kind, PlannedMs: planned.Milliseconds()}, StageWaiting)
}

func (r *Recorder) beginSpan(step *Step, stage string) *Span {
	if r == nil {
		return nil
	}
	now := time.Now()
	step.At = stamp(now)
	r.mu.Lock()
	r.appendLocked(step)
	held := r.settlePendingLocked()
	r.setStageLocked(stage, now)
	r.mu.Unlock()
	runHeld(held)
	return &Span{rec: r, step: step, begun: now}
}

// Exhausted records a pass that found no account. It settles nothing: what
// the scheduler does about it (wait, or give up) is the next decision.
func (r *Recorder) Exhausted(e Exhaustion) {
	if r == nil {
		return
	}
	step := &Step{
		Kind:         KindExhausted,
		At:           stamp(time.Now()),
		Status:       e.Status,
		ErrorCode:    e.Code,
		Error:        truncateError(e.Message),
		RetryAfterMs: e.RetryAfter.Milliseconds(),
	}
	candidates := e.Candidates
	if len(candidates) > MaxCandidates {
		step.CandidatesOmitted = len(candidates) - MaxCandidates
		candidates = candidates[:MaxCandidates]
	}
	step.Candidates = append([]Candidate(nil), candidates...)
	r.mu.Lock()
	r.appendLocked(step)
	r.mu.Unlock()
}

// SettlePending settles the attempts that finished and were waiting for the
// scheduler's next decision. The scheduler calls it on its way out.
func (r *Recorder) SettlePending() {
	if r == nil {
		return
	}
	r.mu.Lock()
	held := r.settlePendingLocked()
	r.mu.Unlock()
	runHeld(held)
}

// Close settles every attempt, finished or not; records published afterwards
// go out at once. The owner of the request calls it when the request ends.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	held := r.settleAllLocked()
	r.mu.Unlock()
	runHeld(held)
}

// Restart begins the trace of the next request served on the same
// connection (a WebSocket turn): what is held is published with the previous
// trace, and the trace starts over.
func (r *Recorder) Restart() {
	if r == nil {
		return
	}
	// A held record takes the trace when it runs, so it runs before the reset.
	// A connection serves its requests one at a time: nothing records between
	// the two steps.
	r.mu.Lock()
	held := r.settleAllLocked()
	r.mu.Unlock()
	runHeld(held)

	now := time.Now()
	r.mu.Lock()
	r.head, r.tail, r.tailPos, r.omitted, r.attempts = nil, nil, 0, 0, 0
	r.pinned, r.model, r.stream, r.current = false, "", false, nil
	r.startedAt = now
	r.setStageLocked(StagePreparing, now)
	r.mu.Unlock()
}

// Idle marks a WebSocket connection between two of its requests. The request
// that ran is over, so what it still holds goes out now rather than when the
// client next speaks, which may be much later.
func (r *Recorder) Idle() {
	if r == nil {
		return
	}
	r.mu.Lock()
	held := r.settleAllLocked()
	r.setStageLocked(StageIdle, time.Now())
	r.mu.Unlock()
	runHeld(held)
}

// Snapshot returns the trace recorded so far, nil when there is none.
func (r *Recorder) Snapshot() *Trace {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.head) == 0 {
		return nil
	}
	trace := &Trace{Pinned: r.pinned, Steps: make([]Step, 0, len(r.head)+len(r.tail))}
	for _, step := range r.head {
		trace.Steps = append(trace.Steps, *step)
	}
	for i := range r.tail {
		trace.Steps = append(trace.Steps, *r.tail[(r.tailPos+i)%len(r.tail)])
	}
	if r.omitted > 0 {
		trace.Omitted = r.omitted
		trace.OmittedAfter = len(r.head)
	}
	return trace
}

// Live returns where the request stands.
func (r *Recorder) Live() Live {
	if r == nil {
		return Live{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	live := Live{
		StartedAt:  r.startedAt,
		Model:      r.model,
		Stream:     r.stream,
		Pinned:     r.pinned,
		Stage:      r.stage,
		StageSince: r.stageSince,
	}
	if step := r.current; step != nil {
		live.Attempt = step.Attempt
		live.Group = step.Group
		live.AuthID = step.AuthID
		live.AuthLabel = step.AuthLabel
		live.Provider = step.Provider
		live.UpstreamModel = step.Model
	}
	return live
}

// appendLocked adds step, dropping the oldest tail step once both the head and
// the tail are full.
func (r *Recorder) appendLocked(step *Step) {
	switch {
	case len(r.head) < headSteps:
		r.head = append(r.head, step)
	case len(r.tail) < tailSteps:
		r.tail = append(r.tail, step)
	default:
		r.tail[r.tailPos] = step
		r.tailPos = (r.tailPos + 1) % tailSteps
		r.omitted++
	}
}

func (r *Recorder) setStageLocked(stage string, now time.Time) {
	if r.stage == stage {
		return
	}
	r.stage = stage
	r.stageSince = now
}

func (r *Recorder) settlePendingLocked() []func() {
	var held []func()
	kept := r.live[:0]
	for _, a := range r.live {
		if a.state == attemptPending {
			a.state = attemptSettled
			held = append(held, a.held...)
			a.held = nil
			continue
		}
		kept = append(kept, a)
	}
	clear(r.live[len(kept):])
	r.live = kept
	return held
}

func (r *Recorder) settleAllLocked() []func() {
	var held []func()
	for _, a := range r.live {
		a.state = attemptSettled
		held = append(held, a.held...)
		a.held = nil
	}
	clear(r.live)
	r.live = r.live[:0]
	return held
}

func (r *Recorder) settleLocked(a *Attempt) []func() {
	if a.state == attemptSettled {
		return nil
	}
	a.state = attemptSettled
	held := a.held
	a.held = nil
	for i, other := range r.live {
		if other == a {
			r.live = append(r.live[:i], r.live[i+1:]...)
			break
		}
	}
	return held
}

// runHeld publishes held records, outside the recorder's lock: building a
// record reads the trace.
func runHeld(held []func()) {
	for _, publish := range held {
		runPublish(publish)
	}
}

// runPublish keeps one failing record from taking the others down with it, or
// a scheduler that publishes on its way out from losing its own result.
func runPublish(publish func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Errorf("routetrace: publishing a held usage record panicked: %v", recovered)
		}
	}()
	publish()
}

func (info AttemptInfo) applyTo(step *Step) {
	step.Group = info.Group
	step.AuthID = info.AuthID
	step.AuthLabel = info.AuthLabel
	step.Provider = info.Provider
	step.Model = info.Model
	step.Optimistic = info.Optimistic
	if !info.CoolingUntil.IsZero() {
		until := stamp(info.CoolingUntil)
		step.CoolingUntil = &until
	}
}

// stamp keeps a step's time to the millisecond: the trace is read by people
// and stored per row.
func stamp(t time.Time) time.Time { return t.Truncate(time.Millisecond) }

func truncateError(message string) string {
	if len(message) <= maxErrorBytes {
		return message
	}
	cut := maxErrorBytes
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + "…"
}
