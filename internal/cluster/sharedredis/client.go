package sharedredis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// Defaults for Options.
const (
	DefaultOpTimeout      = 200 * time.Millisecond
	DefaultHealthInterval = 2 * time.Second
	DefaultMinBackoff     = time.Second
	DefaultMaxBackoff     = 30 * time.Second
	DefaultStableAfter    = time.Minute

	DefaultFailureWindow    = 10 * time.Second
	DefaultFailureThreshold = 3
	DefaultFailureRatio     = 0.5
	// DefaultOutageAfter is half again the health interval, so that every
	// streak long enough to count spans a whole probe: a link that only
	// drops packets lets that probe through and ends the streak.
	DefaultOutageAfter = 3 * time.Second

	// BackgroundTimeout bounds commands issued off the request path: counter
	// flushes, lease renewals and health probes.
	BackgroundTimeout = 2 * time.Second

	// downReminderInterval spaces the warning repeated while the server stays
	// unreachable, so an outage is visible in the log without flooding it.
	downReminderInterval = 5 * time.Minute

	// failureLogInterval spaces the log of commands that failed without
	// marking the client down, so the loss on the link shows in the log
	// without one line per lost command.
	failureLogInterval = time.Minute
)

// ErrUnavailable is returned without touching the network while the client is
// marked down. Callers treat it like any other failure: fall back to local.
var ErrUnavailable = errors.New("sharedredis: cluster redis unavailable")

// Options tunes a Client. Zero values take the defaults above.
type Options struct {
	// NodeID names this process in the health key and in log lines.
	NodeID string
	// OpTimeout bounds every request-path command.
	OpTimeout time.Duration
	// HealthInterval is how often a healthy client probes the server.
	HealthInterval time.Duration
	// MinBackoff and MaxBackoff bound the probe interval while the server is
	// down; it doubles after every failed probe.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// StableAfter is how long the server must stay healthy before a new
	// failure is treated as a fresh outage rather than as flapping.
	StableAfter time.Duration
	// FailureWindow, FailureThreshold and FailureRatio decide when commands
	// lost to timeouts or broken connections mark the server down: at least
	// FailureThreshold of them within FailureWindow, making up at least
	// FailureRatio of the request-path commands and probes in it. Until then
	// each lost command falls back to node-local state on its own and the
	// next one tries the server again.
	FailureWindow    time.Duration
	FailureThreshold int
	FailureRatio     float64
	// OutageAfter marks the server down without waiting for the ratio when
	// nothing succeeds: FailureThreshold failures in a row spanning at least
	// this long. Keep it longer than HealthInterval, so that a probe falls
	// inside every such streak.
	OutageAfter time.Duration
}

func (o Options) withDefaults() Options {
	if o.OpTimeout <= 0 {
		o.OpTimeout = DefaultOpTimeout
	}
	if o.HealthInterval <= 0 {
		o.HealthInterval = DefaultHealthInterval
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff < o.MinBackoff {
		o.MaxBackoff = DefaultMaxBackoff
		if o.MaxBackoff < o.MinBackoff {
			o.MaxBackoff = o.MinBackoff
		}
	}
	if o.StableAfter <= 0 {
		o.StableAfter = DefaultStableAfter
	}
	if o.FailureWindow <= 0 {
		o.FailureWindow = DefaultFailureWindow
	}
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = DefaultFailureThreshold
	}
	// Written so that NaN takes the default too.
	if !(o.FailureRatio > 0 && o.FailureRatio <= 1) {
		o.FailureRatio = DefaultFailureRatio
	}
	if o.OutageAfter <= 0 {
		o.OutageAfter = DefaultOutageAfter
	}
	if strings.TrimSpace(o.NodeID) == "" {
		o.NodeID = "local"
	}
	return o
}

// Status is a point-in-time view of the client for status pages and logs.
type Status struct {
	Addr      string    `json:"addr"`
	Available bool      `json:"available"`
	Since     time.Time `json:"since"`
	LastError string    `json:"last_error,omitempty"`
}

// Client wraps a go-redis client with availability tracking. The zero value is
// not usable; use New. A nil *Client is valid and permanently unavailable, so
// callers can hold one unconditionally.
type Client struct {
	rdb    *redis.Client
	addr   string
	opts   Options
	health string

	available atomic.Bool
	started   atomic.Bool
	probeNow  chan struct{}
	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once

	mu           sync.Mutex
	lastErr      error
	changedAt    time.Time
	healthySince time.Time
	backoff      time.Duration
	lastReminder time.Time
	lastOpErrLog time.Time
	lastLossLog  time.Time
	failures     failureTracker
}

// New builds a client for cfg without connecting. Call Start to probe the
// server and begin health tracking; until the first probe succeeds the client
// reports unavailable.
func New(cfg config.ClusterRedisConfig, opts Options) (*Client, error) {
	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		return nil, errors.New("sharedredis: cluster redis addr is empty")
	}
	tlsCfg, err := BuildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	opts = opts.withDefaults()

	const dialTimeout = 2 * time.Second
	ro := &redis.Options{
		Addr:     addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		// The dialer honours the caller's context, so a request-path command
		// that has to open a connection (TCP plus TLS handshake over the public
		// network) still gives up at the op timeout. go-redis' default TLS
		// dialer ignores the context and would wait for the full dial timeout.
		Dialer:        contextDialer(dialTimeout, tlsCfg),
		TLSConfig:     tlsCfg,
		DialTimeout:   dialTimeout,
		DialerRetries: 1,
		ReadTimeout:   BackgroundTimeout,
		WriteTimeout:  BackgroundTimeout,
		// Socket deadlines follow the per-command context, which is how the op
		// timeout reaches the wire. Retries would multiply it, so none.
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
		// Warm connections keep the TLS handshake off the request path.
		MinIdleConns:    4,
		DisableIdentity: true,
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: maintnotifications.ModeDisabled,
		},
	}
	c := &Client{
		rdb:      redis.NewClient(ro),
		addr:     addr,
		opts:     opts,
		health:   KeyPrefix + "health:" + opts.NodeID,
		probeNow: make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		backoff:  opts.MinBackoff,
	}
	return c, nil
}

// contextDialer opens connections, wrapping every failure in dialError so
// that classifyError can tell a refused dial from a lost reply.
func contextDialer(timeout time.Duration, tlsCfg *tls.Config) func(context.Context, string, string) (net.Conn, error) {
	netDialer := &net.Dialer{Timeout: timeout, KeepAlive: 5 * time.Minute}
	dial := netDialer.DialContext
	if tlsCfg != nil {
		dial = (&tls.Dialer{NetDialer: netDialer, Config: tlsCfg}).DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, &dialError{err: err}
		}
		return conn, nil
	}
}

// Start probes the server once and starts the background health loop. It is
// safe to call more than once. It does not block for longer than one probe.
func (c *Client) Start() {
	if c == nil {
		return
	}
	c.startOnce.Do(func() {
		select {
		case <-c.stop:
			return
		default:
		}
		c.started.Store(true)
		c.probe()
		go c.healthLoop()
	})
}

// Close stops the health loop and closes the connection pool. It waits for an
// in-flight probe, which is bounded by twice BackgroundTimeout plus the op
// timeout: a probe retries once, and not after Close has begun.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.available.Store(false)
		close(c.stop)
		if c.started.Load() {
			<-c.done
		}
		_ = c.rdb.Close()
	})
}

// Available reports whether request-path commands should be attempted.
func (c *Client) Available() bool {
	return c != nil && c.available.Load()
}

// OpTimeout returns the request-path command timeout.
func (c *Client) OpTimeout() time.Duration {
	if c == nil {
		return DefaultOpTimeout
	}
	return c.opts.OpTimeout
}

// NodeID returns the node name this client was built with.
func (c *Client) NodeID() string {
	if c == nil {
		return ""
	}
	return c.opts.NodeID
}

// Redis exposes the underlying client for tests and for helpers in this
// module that need commands the wrappers do not cover. Callers own their
// timeouts and must report failures through Observe.
func (c *Client) Redis() *redis.Client {
	if c == nil {
		return nil
	}
	return c.rdb
}

// Status returns the current availability view.
func (c *Client) Status() Status {
	if c == nil {
		return Status{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{Addr: c.addr, Available: c.available.Load(), Since: c.changedAt}
	if c.lastErr != nil {
		st.LastError = c.lastErr.Error()
	}
	return st
}

// Eval runs script on the request path. It returns ErrUnavailable at once
// while the client is down; otherwise the call is bounded by the op timeout
// (or an earlier deadline on ctx).
//
// The caller's cancellation is deliberately not propagated: a command that
// takes a slot or mints an id must either complete or time out, because a
// reply lost to a cancelled client would leave state behind that nobody
// releases until its TTL.
func (c *Client) Eval(ctx context.Context, script *redis.Script, keys []string, args ...any) (any, error) {
	if !c.Available() {
		return nil, ErrUnavailable
	}
	opCtx, cancel, callerBound := c.opContext(ctx, c.opts.OpTimeout)
	defer cancel()
	res, err := script.Run(opCtx, c.rdb, keys, args...).Result()
	c.observe(err, true, callerBound)
	return res, err
}

// EvalBackground runs script off the request path with BackgroundTimeout.
func (c *Client) EvalBackground(ctx context.Context, script *redis.Script, keys []string, args ...any) (any, error) {
	if !c.Available() {
		return nil, ErrUnavailable
	}
	opCtx, cancel, callerBound := c.opContext(ctx, BackgroundTimeout)
	defer cancel()
	res, err := script.Run(opCtx, c.rdb, keys, args...).Result()
	c.observe(err, false, callerBound)
	return res, err
}

// PipelineBackground runs fn as one pipeline off the request path.
func (c *Client) PipelineBackground(ctx context.Context, fn func(redis.Pipeliner) error) error {
	if !c.Available() {
		return ErrUnavailable
	}
	opCtx, cancel, callerBound := c.opContext(ctx, BackgroundTimeout)
	defer cancel()
	_, err := c.rdb.Pipelined(opCtx, fn)
	c.observe(err, false, callerBound)
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

// TxPipelineBackground runs fn as one MULTI/EXEC transaction off the request
// path. Use it whenever the batch creates keys: a key and the PEXPIRE that
// bounds it are then applied together or not at all. The production instance
// evicts with volatile-lru, so a key left without a TTL could never be evicted
// and would eventually make every write fail once memory is full.
func (c *Client) TxPipelineBackground(ctx context.Context, fn func(redis.Pipeliner) error) error {
	if !c.Available() {
		return ErrUnavailable
	}
	opCtx, cancel, callerBound := c.opContext(ctx, BackgroundTimeout)
	defer cancel()
	_, err := c.rdb.TxPipelined(opCtx, fn)
	c.observe(err, false, callerBound)
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

// opContext bounds one command by timeout, or by the caller's deadline when
// that is earlier, which callerBound reports. The caller's cancellation is
// not inherited (see Eval).
func (c *Client) opContext(parent context.Context, timeout time.Duration) (ctx context.Context, cancel context.CancelFunc, callerBound bool) {
	if parent == nil {
		parent = context.Background()
	}
	deadline := time.Now().Add(timeout)
	if d, ok := parent.Deadline(); ok && d.Before(deadline) {
		deadline = d
		callerBound = true
	}
	ctx, cancel = context.WithDeadline(context.WithoutCancel(parent), deadline)
	return ctx, cancel, callerBound
}

// observe records the outcome of one command issued through the wrappers.
//
// countSuccess is set for request-path commands: their successes show the
// server answering within the request-path budget, which is what availability
// means here. A background command succeeding within its longer timeout shows
// no such thing, though its failures still count.
//
// callerBound is set when the caller's deadline, earlier than ours, bounded
// the command. A timeout is then the caller's budget running out and says
// nothing about the server.
func (c *Client) observe(err error, countSuccess, callerBound bool) {
	if err == nil || errors.Is(err, redis.Nil) {
		if countSuccess {
			c.recordSuccess()
		}
		return
	}
	if callerBound && isTimeout(err) {
		return
	}
	c.Observe(err)
}

// Observe classifies a command error. A fatal one (a refused dial, a reply
// such as LOADING or NOAUTH) marks the client down so that later requests
// skip Redis. A transient one (a timeout, a broken connection) fails only
// that command and counts towards the failure window, which marks the client
// down once enough of them add up. An error the server returned for that one
// command (a script bug, a wrong type) only fails that command.
func (c *Client) Observe(err error) {
	if c == nil || err == nil {
		return
	}
	switch classifyError(err) {
	case failureFatal:
		c.markDown(err, "")
	case failureTransient:
		c.recordTransient(err)
	case failureCommand:
		c.mu.Lock()
		logIt := time.Since(c.lastOpErrLog) >= time.Minute
		if logIt {
			c.lastOpErrLog = time.Now()
		}
		c.mu.Unlock()
		if logIt {
			log.WithError(err).Warn("cluster redis: command failed; falling back to node-local state for it")
		}
	}
}

// recordSuccess counts a success held to the request-path budget.
func (c *Client) recordSuccess() {
	if !c.available.Load() {
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.failures.success(now, c.opts)
	c.mu.Unlock()
}

// recordTransient counts a transient failure, and marks the client down once
// the failures add up to an outage (see failureTracker.failure).
func (c *Client) recordTransient(err error) {
	if !c.available.Load() {
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return
	}
	now := time.Now()
	c.mu.Lock()
	outage, reason := c.failures.failure(now, c.opts)
	logIt := !outage && now.Sub(c.lastLossLog) >= failureLogInterval
	var ok, failed int
	if logIt {
		c.lastLossLog = now
		ok, failed = c.failures.totals(now, c.opts)
	}
	c.mu.Unlock()
	if outage {
		c.markDown(err, reason)
		return
	}
	if logIt {
		log.WithError(err).WithFields(log.Fields{
			"addr":   c.addr,
			"failed": failed,
			"total":  ok + failed,
			"window": c.opts.FailureWindow.String(),
		}).Info("cluster redis: command failed and fell back to node-local state; the client stays available")
	}
}

// markDown takes the client out of the request path. reason says why
// transient failures added up to an outage; it is empty when err alone is
// fatal.
func (c *Client) markDown(err error, reason string) {
	if !c.available.CompareAndSwap(true, false) {
		c.mu.Lock()
		c.lastErr = err
		c.mu.Unlock()
		return
	}
	now := time.Now()
	c.mu.Lock()
	// Failing again soon after recovering is flapping, typically a server that
	// answers probes but not real traffic in time. Probing again right away
	// would only flip it back on, so the backoff keeps growing instead.
	flapping := !c.healthySince.IsZero() && now.Sub(c.healthySince) < c.opts.StableAfter
	if flapping {
		c.backoff = c.nextBackoffLocked()
	} else {
		c.backoff = c.opts.MinBackoff
	}
	c.lastErr = err
	c.changedAt = now
	c.lastReminder = now
	c.failures.reset()
	c.mu.Unlock()
	entry := log.WithError(err).WithField("addr", c.addr)
	if reason != "" {
		entry = entry.WithField("reason", reason)
	}
	entry.Warn("cluster redis: marked unavailable; limits, slots and affinity fall back to node-local state")
	if !flapping {
		select {
		case c.probeNow <- struct{}{}:
		default:
		}
	}
}

func (c *Client) nextBackoffLocked() time.Duration {
	next := c.backoff * 2
	if next < c.opts.MinBackoff {
		next = c.opts.MinBackoff
	}
	if next > c.opts.MaxBackoff {
		next = c.opts.MaxBackoff
	}
	return next
}

func (c *Client) healthLoop() {
	defer close(c.done)
	for {
		wait := c.opts.HealthInterval
		if !c.available.Load() {
			c.mu.Lock()
			wait = c.backoff
			c.mu.Unlock()
		}
		timer := time.NewTimer(wait)
		select {
		case <-c.stop:
			timer.Stop()
			return
		case <-c.probeNow:
			timer.Stop()
		case <-timer.C:
		}
		select {
		case <-c.stop:
			return
		default:
		}
		c.probe()
	}
}

// probe writes the node's health key. A write rather than PING, because a
// server that answers PING but refuses writes (a replica after failover, a
// full disk) is just as useless to the counters.
//
// Each attempt first writes with the background timeout, since it may have to
// open a connection, then again on that now warm connection within the
// request-path budget: a Redis that answers, but slower than requests may
// wait, must stay out of the request path.
//
// An attempt that loses a command is made once more. One dropped packet costs
// a retransmission timeout, at least 200ms on Linux, which alone exceeds the
// request-path budget; on a link losing one packet in ten a single attempt
// would fail one probe in five, and keep a recovered server out for a whole
// backoff. Two lost attempts in a row are rare on a link that merely drops
// packets, and common on one that is down.
func (c *Client) probe() {
	value := strconv.FormatInt(time.Now().UnixMilli(), 10)
	err := c.probeAttempt(value)
	if err != nil && classifyError(err) == failureTransient {
		select {
		case <-c.stop:
			return
		default:
		}
		err = c.probeAttempt(value)
	}
	if err != nil {
		c.probeFailed(err)
		return
	}
	c.probeSucceeded()
}

func (c *Client) probeAttempt(value string) error {
	err := c.probeOnce(BackgroundTimeout, value)
	if err == nil {
		err = c.probeOnce(c.opts.OpTimeout, value)
	}
	return err
}

func (c *Client) probeOnce(timeout time.Duration, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.rdb.Set(ctx, c.health, value, 30*time.Second).Err(); err != nil {
		return fmt.Errorf("health probe: %w", err)
	}
	return nil
}

func (c *Client) probeSucceeded() {
	if c.available.Load() {
		c.recordSuccess()
		return
	}
	now := time.Now()
	c.mu.Lock()
	downSince := c.changedAt
	c.healthySince = now
	c.changedAt = now
	c.lastErr = nil
	// Failures from before the outage say nothing about the server now; the
	// probe that just got through does.
	c.failures.reset()
	c.failures.success(now, c.opts)
	c.mu.Unlock()
	c.available.Store(true)
	entry := log.WithField("addr", c.addr)
	if !downSince.IsZero() {
		entry = entry.WithField("down_for", now.Sub(downSince).Round(time.Millisecond).String())
	}
	entry.Info("cluster redis: available; limits, slots and affinity are cluster-wide")
}

func (c *Client) probeFailed(err error) {
	if c.available.Load() {
		// A probe that lost both attempts counts like a lost request-path
		// command. Any other failure is a server that refuses the health write,
		// which is as useless to the counters as one that does not answer.
		switch classifyError(err) {
		case failureTransient:
			c.recordTransient(err)
		case failureNone:
		default:
			c.markDown(err, "")
		}
		return
	}
	now := time.Now()
	c.mu.Lock()
	first := c.changedAt.IsZero()
	if first {
		c.changedAt = now
		c.lastReminder = now
	}
	c.lastErr = err
	c.backoff = c.nextBackoffLocked()
	remind := !first && now.Sub(c.lastReminder) >= downReminderInterval
	if remind {
		c.lastReminder = now
	}
	since := c.changedAt
	c.mu.Unlock()
	switch {
	case first:
		log.WithError(err).WithField("addr", c.addr).Warn("cluster redis: unreachable at startup; limits, slots and affinity stay node-local until it answers")
	case remind:
		log.WithError(err).WithField("addr", c.addr).WithField("down_for", now.Sub(since).Round(time.Second).String()).Warn("cluster redis: still unavailable")
	default:
		log.WithError(err).WithField("addr", c.addr).Debug("cluster redis: probe failed")
	}
}
