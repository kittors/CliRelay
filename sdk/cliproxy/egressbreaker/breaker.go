package egressbreaker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Defaults for Config fields left at zero.
const (
	// DefaultRejectionThreshold is how many handshakes in a row the proxy must
	// reject before the breaker opens. A rejection is the proxy's own answer
	// and does not change on retry, so a second one is all the confirmation it
	// takes, and it comes within milliseconds: a request that fails on one
	// account moves on to the next. The second look is there because a proxy
	// gateway can fail a single authentication when its own backend hiccups,
	// and shutting out a tenant's only proxy for the base wait over one such
	// hiccup would turn one failed request into fifteen seconds of them.
	DefaultRejectionThreshold = 2
	// DefaultUnreachableThreshold is how many connects to the proxy must fail
	// in a row, with no request getting through in between, before the breaker
	// opens. One refused or timed-out connect is ordinary packet loss or a
	// proxy restarting; three with nothing succeeding is an outage. Requests
	// fail concurrently during one, so the count is reached within seconds.
	DefaultUnreachableThreshold = 3
	// DefaultBaseOpen is how long an opened breaker refuses requests before it
	// lets one through to try the proxy. It is short because a proxy restart
	// or a routing blip is over in seconds, and a recovered proxy should not
	// stay shut out for long.
	DefaultBaseOpen = 15 * time.Second
	// DefaultMaxOpen caps the wait between probes, which doubles each time a
	// probe fails. Two minutes keeps a long outage down to one handshake per
	// proxy every two minutes (an outage of half an hour costs about sixteen)
	// and bounds how long a proxy that came back stays unused.
	DefaultMaxOpen = 2 * time.Minute
	// DefaultProbeTimeout is how long one probe may hold the breaker before
	// the next request may probe instead. A handshake with a proxy that
	// accepts the connection and then says nothing has no timeout of its own,
	// and the breaker must not wait on it for ever.
	DefaultProbeTimeout = 30 * time.Second
)

// probeRetryAfter is what a request is told to wait when it is refused only
// because a probe is on its way. The probe settles as soon as its handshake
// does, so the answer is about a second away.
const probeRetryAfter = time.Second

// staleAfter is how long a proxy's state is kept once nothing asks about it
// any more. An operator who replaces a broken proxy leaves its URL behind with
// the breaker open and no request left to probe it.
const staleAfter = time.Hour

// Config tunes a Breaker. Zero values take the defaults.
type Config struct {
	RejectionThreshold   int
	UnreachableThreshold int
	BaseOpen             time.Duration
	MaxOpen              time.Duration
	ProbeTimeout         time.Duration
	// Now is the clock; nil means time.Now. Tests replace it.
	Now func() time.Time
}

// Breaker decides, per proxy URL, whether requests may go out through it.
//
// A proxy starts closed: requests pass and their outcomes are counted.
// RejectionThreshold rejected authentications in a row, or
// UnreachableThreshold failed connects in a row, open it: requests are refused
// without touching the network. Once the wait is over a single request is let
// through as the probe while the others keep being refused. A probe that gets
// past the proxy closes the breaker; one that meets a fault opens it again for
// twice as long, up to MaxOpen.
//
// State exists only for proxies that are failing, so the map stays as small as
// the number of broken proxies. All methods are safe for concurrent use, and
// safe on a nil *Breaker, which lets everything through.
type Breaker struct {
	cfg      Config
	disabled atomic.Bool

	mu     sync.Mutex
	epoch  uint64
	states map[string]*state
	// passed is when a request last got through each proxy. The state above
	// vanishes with the process, so after a restart "not open" only means
	// "not seen failing yet"; a request that got through is what confirms
	// that an outage reported earlier is over (see Observe). It holds one
	// time per proxy in use and forgets the ones unused for staleAfter.
	passed map[string]time.Time
}

type state struct {
	// label names the proxy in logs: scheme, host and port, plus a fingerprint
	// of the credentials when there are any. Never the credentials.
	label string
	// lastSeen is when a request last asked about the proxy or reported on it.
	lastSeen time.Time
	// rejections and unreachable count the faults of each kind in a row while
	// closed; a request that gets through clears both.
	rejections  int
	unreachable int

	open  bool
	fault Fault
	// since is when this outage first opened the breaker.
	since time.Time
	// until is when the next probe may go out.
	until   time.Time
	backoff time.Duration
	// probing is set while the probe request is out; probeStarted bounds how
	// long it may hold the slot.
	probing      bool
	probeStarted time.Time
	// epoch changes each time the breaker opens or a probe starts, so that the
	// outcome of an earlier probe is not applied to a later round.
	epoch uint64
	// refused counts the attempts turned away during this outage. One request
	// that walks a pool of accounts is turned away once per account.
	refused int64
	// failedProbes counts the probes of this outage that met a fault: the
	// proof that the proxy was still failing after the breaker opened, not
	// just at the moment it did.
	failedProbes int
}

// New builds a Breaker.
func New(cfg Config) *Breaker {
	if cfg.RejectionThreshold <= 0 {
		cfg.RejectionThreshold = DefaultRejectionThreshold
	}
	if cfg.UnreachableThreshold <= 0 {
		cfg.UnreachableThreshold = DefaultUnreachableThreshold
	}
	if cfg.BaseOpen <= 0 {
		cfg.BaseOpen = DefaultBaseOpen
	}
	if cfg.MaxOpen < cfg.BaseOpen {
		cfg.MaxOpen = max(DefaultMaxOpen, cfg.BaseOpen)
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = DefaultProbeTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Breaker{cfg: cfg, states: map[string]*state{}, passed: map[string]time.Time{}}
}

var defaultBreaker = New(Config{})

// Default is the process-wide breaker. The scheduler and the upstream
// transports must share one: the scheduler skips an account whose proxy the
// transports found broken.
func Default() *Breaker { return defaultBreaker }

// EnvSwitch names the environment variable that turns the process-wide
// breaker off for a node, without a new build: set it to "off" (or "false",
// "0", "disabled") in the node's environment file and restart the process.
// Anything else, unset included, leaves the breaker on. The service reads it
// once when it starts.
const EnvSwitch = "CLIRELAY_EGRESS_BREAKER"

// EnabledByEnv reports whether value, the content of EnvSwitch, leaves the
// breaker on.
func EnabledByEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "false", "0", "disabled":
		return false
	default:
		return true
	}
}

// SetEnabled turns the breaker on or off. Off, every request is let through
// and nothing is recorded, which is the behaviour from before the breaker
// existed; what it had recorded is forgotten.
func (b *Breaker) SetEnabled(enabled bool) {
	if b == nil {
		return
	}
	b.disabled.Store(!enabled)
	if !enabled {
		b.Reset()
	}
}

// Reset forgets every proxy's state.
func (b *Breaker) Reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.states = map[string]*state{}
	b.passed = map[string]time.Time{}
	b.mu.Unlock()
}

// Check reports whether a request through proxyURL would be refused right
// now, without claiming the probe: a caller that has not decided to send yet
// (the scheduler choosing an account) must not use up the one request that is
// allowed to try the proxy. It returns nil, or the *Error to fail with.
func (b *Breaker) Check(proxyURL string) error {
	key := strings.TrimSpace(proxyURL)
	if b == nil || key == "" || b.disabled.Load() {
		return nil
	}
	now := b.cfg.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[key]
	if s == nil || !s.open {
		return nil
	}
	s.lastSeen = now
	if wait := b.waitLocked(s, now); wait > 0 {
		s.refused++
		return &Error{Fault: s.fault, Refused: true, RetryAfter: wait}
	}
	return nil
}

// Allow admits a request that is about to connect through proxyURL. It
// returns the *Error to fail with when the breaker refuses it; otherwise the
// caller must report how the connection went through the Permit. An empty
// proxyURL (no proxy) is always admitted with a nil Permit, whose methods do
// nothing.
func (b *Breaker) Allow(proxyURL string) (*Permit, error) {
	key := strings.TrimSpace(proxyURL)
	if b == nil || key == "" || b.disabled.Load() {
		return nil, nil
	}
	now := b.cfg.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[key]
	if s == nil || !s.open {
		return &Permit{breaker: b, key: key}, nil
	}
	s.lastSeen = now
	if wait := b.waitLocked(s, now); wait > 0 {
		s.refused++
		return nil, &Error{Fault: s.fault, Refused: true, RetryAfter: wait}
	}
	b.epoch++
	s.epoch, s.probing, s.probeStarted = b.epoch, true, now
	return &Permit{breaker: b, key: key, probe: true, epoch: s.epoch}, nil
}

// waitLocked is how long a request has to stay away from an open breaker.
// Zero means the wait is over and no probe holds the slot.
func (b *Breaker) waitLocked(s *state, now time.Time) time.Duration {
	if s.probing && now.Sub(s.probeStarted) < b.cfg.ProbeTimeout {
		return probeRetryAfter
	}
	return max(s.until.Sub(now), 0)
}

// Permit is one admitted request. Connected and Done report how its
// connection went; Done must be called exactly once.
type Permit struct {
	breaker *Breaker
	key     string
	// probe marks the request an open breaker let through to try the proxy.
	probe  bool
	epoch  uint64
	passed atomic.Bool
}

// Connected reports that the request holds a connection through the proxy.
// Calling it as soon as that is known, rather than waiting for the response,
// matters for a probe: an upstream may take a minute to answer, and the proxy
// has proven itself long before that.
func (p *Permit) Connected() {
	if p == nil || !p.passed.CompareAndSwap(false, true) {
		return
	}
	p.breaker.pass(p)
}

// Done reports how the request ended and returns the error the caller should
// fail with: err itself, or an *Error in its place when err is a proxy fault.
func (p *Permit) Done(err error) error {
	if p == nil {
		return err
	}
	if fault := FaultOf(err); fault != FaultNone {
		wait := p.breaker.fail(p, fault, err)
		return &Error{Fault: fault, RetryAfter: wait, cause: err}
	}
	switch {
	case err == nil:
		p.Connected()
	case p.passed.Load():
		// It failed after getting through the proxy.
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller gave up before the connection settled, which says
		// nothing either way.
		p.breaker.abandon(p)
	default:
		// The request failed, but not on anything this package counts
		// against the proxy; see the package comment for why a handshake
		// that reports on the destination is not held against it.
		p.Connected()
	}
	return err
}

// pass records that a request got through the proxy.
func (b *Breaker) pass(p *Permit) {
	now := b.cfg.Now()
	b.mu.Lock()
	s := b.states[p.key]
	switch {
	case s == nil:
		b.notePassLocked(p.key, now)
		b.mu.Unlock()
		return
	case !s.open:
		// Closed: a request got through, so the run of faults ends. A probe
		// from a round that is over leaves the new count alone.
		if !p.probe {
			delete(b.states, p.key)
		}
		b.notePassLocked(p.key, now)
		b.mu.Unlock()
		return
	case !p.probe || s.epoch != p.epoch:
		// A request admitted before the breaker opened, or a probe of an
		// earlier round. It may have reused a connection that predates the
		// outage, which says nothing about whether new ones can be opened;
		// only the current probe closes the breaker. For the same reason it
		// is not recorded as a pass.
		b.mu.Unlock()
		return
	}
	delete(b.states, p.key)
	b.notePassLocked(p.key, now)
	label, outage, refused := s.label, now.Sub(s.since), s.refused
	b.mu.Unlock()
	log.Infof("egress-breaker: %s works again after %s; %d attempt(s) were refused instead of sent",
		label, outage.Round(time.Second), refused)
}

// fail records a proxy fault and returns how long requests through the proxy
// are refused from now, zero when the breaker stays closed.
func (b *Breaker) fail(p *Permit, fault Fault, cause error) time.Duration {
	now := b.cfg.Now()
	b.mu.Lock()
	s := b.states[p.key]
	if p.probe {
		if s == nil || !s.open || s.epoch != p.epoch {
			// A probe of an earlier round; the state has moved on without it.
			wait := b.remainingLocked(s, now)
			b.mu.Unlock()
			return wait
		}
		wait := b.openLocked(s, fault, now, true)
		s.failedProbes++
		label, outage, refused := s.label, now.Sub(s.since), s.refused
		b.mu.Unlock()
		log.Warnf("egress-breaker: %s is still unavailable after %s (%s); %d attempt(s) refused so far, next probe in %s: %v",
			label, outage.Round(time.Second), fault, refused, wait, cause)
		return wait
	}
	if s != nil && s.open {
		// Admitted before the breaker opened; its failure is already counted
		// by the one that opened it.
		wait := b.remainingLocked(s, now)
		b.mu.Unlock()
		return wait
	}
	if s == nil {
		b.forgetStaleLocked(now)
		s = &state{label: labelOf(p.key)}
		b.states[p.key] = s
	}
	s.lastSeen = now
	var count, threshold int
	if fault == FaultAuthRejected {
		s.rejections++
		count, threshold = s.rejections, b.cfg.RejectionThreshold
	} else {
		s.unreachable++
		count, threshold = s.unreachable, b.cfg.UnreachableThreshold
	}
	if count < threshold {
		label := s.label
		b.mu.Unlock()
		log.Debugf("egress-breaker: %s failed (%s, %d/%d in a row): %v", label, fault, count, threshold, cause)
		return 0
	}
	wait := b.openLocked(s, fault, now, false)
	label := s.label
	b.mu.Unlock()
	log.Warnf("egress-breaker: %s is unavailable (%s); requests through it are refused instead of sent, next probe in %s: %v",
		label, fault, wait, cause)
	return wait
}

// openLocked opens the breaker, or opens it again after a failed probe with
// twice the previous wait, and returns the wait.
func (b *Breaker) openLocked(s *state, fault Fault, now time.Time, reopen bool) time.Duration {
	if reopen {
		s.backoff = min(s.backoff*2, b.cfg.MaxOpen)
	} else {
		s.backoff = b.cfg.BaseOpen
		s.since = now
		s.refused = 0
		s.failedProbes = 0
	}
	s.open, s.fault, s.probing = true, fault, false
	s.rejections, s.unreachable = 0, 0
	s.until = now.Add(s.backoff)
	b.epoch++
	s.epoch = b.epoch
	return s.backoff
}

func (b *Breaker) remainingLocked(s *state, now time.Time) time.Duration {
	if s == nil || !s.open {
		return 0
	}
	return b.waitLocked(s, now)
}

// forgetStaleLocked drops the state of proxies nothing has asked about for
// staleAfter, and the passes older than that.
func (b *Breaker) forgetStaleLocked(now time.Time) {
	for key, s := range b.states {
		if now.Sub(s.lastSeen) > staleAfter {
			delete(b.states, key)
		}
	}
	for key, at := range b.passed {
		if now.Sub(at) > staleAfter {
			delete(b.passed, key)
		}
	}
}

// notePassLocked records that a request got through the proxy. A proxy seen
// for the first time is the moment to drop the stale ones, as fail does for
// states, so the map stays as small as the set of proxies in use.
func (b *Breaker) notePassLocked(key string, now time.Time) {
	if _, known := b.passed[key]; !known {
		b.forgetStaleLocked(now)
	}
	b.passed[key] = now
}

// abandon gives the probe slot back when the probe ended without an answer,
// so the next request probes instead of waiting out ProbeTimeout.
func (b *Breaker) abandon(p *Permit) {
	if !p.probe {
		return
	}
	b.mu.Lock()
	if s := b.states[p.key]; s != nil && s.open && s.probing && s.epoch == p.epoch {
		s.probing = false
	}
	b.mu.Unlock()
}

// Status describes one proxy the breaker currently refuses requests for.
type Status struct {
	// Label names the proxy without its credentials.
	Label string
	Fault Fault
	// Since is when the outage opened the breaker.
	Since time.Time
	// NextProbe is when a request is next let through to try the proxy.
	NextProbe time.Time
	// Refused counts the attempts turned away during this outage; a request
	// that walks a pool of accounts counts once per account.
	Refused int64
	// FailedProbes counts the probes of this outage that met a fault.
	FailedProbes int
}

// Open lists the proxies requests are refused for, ordered by label.
func (b *Breaker) Open() []Status {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	b.forgetStaleLocked(b.cfg.Now())
	out := make([]Status, 0, len(b.states))
	for _, s := range b.states {
		if s.open {
			out = append(out, s.status())
		}
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// labelOf names a proxy for the log: scheme, host and port. Two pool entries
// may share those and differ only in credentials, so a short fingerprint of
// the credentials tells them apart without revealing them.
func labelOf(proxyURL string) string {
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return "proxy (unparsable URL)"
	}
	label := strings.ToLower(parsed.Scheme) + "://" + parsed.Host
	if parsed.User != nil {
		sum := sha256.Sum256([]byte(parsed.User.String()))
		label += " (credentials #" + hex.EncodeToString(sum[:4]) + ")"
	}
	return label
}
