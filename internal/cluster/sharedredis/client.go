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

	// BackgroundTimeout bounds commands issued off the request path: counter
	// flushes, lease renewals and health probes.
	BackgroundTimeout = 2 * time.Second

	// downReminderInterval spaces the warning repeated while the server stays
	// unreachable, so an outage is visible in the log without flooding it.
	downReminderInterval = 5 * time.Minute
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

func contextDialer(timeout time.Duration, tlsCfg *tls.Config) func(context.Context, string, string) (net.Conn, error) {
	netDialer := &net.Dialer{Timeout: timeout, KeepAlive: 5 * time.Minute}
	if tlsCfg == nil {
		return netDialer.DialContext
	}
	tlsDialer := &tls.Dialer{NetDialer: netDialer, Config: tlsCfg}
	return tlsDialer.DialContext
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
// in-flight probe, which is bounded by BackgroundTimeout plus the op timeout.
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
// while the client is down; otherwise the call is bounded by the op timeout.
//
// The caller's cancellation is deliberately not propagated: a command that
// takes a slot or mints an id must either complete or time out, because a
// reply lost to a cancelled client would leave state behind that nobody
// releases until its TTL.
func (c *Client) Eval(ctx context.Context, script *redis.Script, keys []string, args ...any) (any, error) {
	if !c.Available() {
		return nil, ErrUnavailable
	}
	opCtx, cancel := c.opContext(ctx, c.opts.OpTimeout)
	defer cancel()
	res, err := script.Run(opCtx, c.rdb, keys, args...).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		c.Observe(err)
	}
	return res, err
}

// EvalBackground runs script off the request path with BackgroundTimeout.
func (c *Client) EvalBackground(ctx context.Context, script *redis.Script, keys []string, args ...any) (any, error) {
	if !c.Available() {
		return nil, ErrUnavailable
	}
	opCtx, cancel := c.opContext(ctx, BackgroundTimeout)
	defer cancel()
	res, err := script.Run(opCtx, c.rdb, keys, args...).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		c.Observe(err)
	}
	return res, err
}

// PipelineBackground runs fn as one pipeline off the request path.
func (c *Client) PipelineBackground(ctx context.Context, fn func(redis.Pipeliner) error) error {
	if !c.Available() {
		return ErrUnavailable
	}
	opCtx, cancel := c.opContext(ctx, BackgroundTimeout)
	defer cancel()
	_, err := c.rdb.Pipelined(opCtx, fn)
	if err != nil && !errors.Is(err, redis.Nil) {
		c.Observe(err)
		return err
	}
	return nil
}

func (c *Client) opContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

// Observe classifies a command error. A connectivity failure marks the client
// down so that later requests skip Redis; an error the server returned for
// that one command (a script bug, a wrong type) only fails that command.
func (c *Client) Observe(err error) {
	if c == nil || err == nil {
		return
	}
	if IsConnectivityError(err) {
		c.markDown(err)
		return
	}
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

// unusableServerPrefixes are error replies that mean the server as a whole
// cannot serve us right now, as opposed to rejecting one command.
var unusableServerPrefixes = []string{
	"LOADING", "READONLY", "MASTERDOWN", "BUSY", "NOAUTH", "WRONGPASS",
	"NOPERM", "CLUSTERDOWN", "TRYAGAIN", "MISCONF",
}

// IsConnectivityError reports whether err means the server cannot be used,
// rather than that one command was rejected. The caller's own cancellation is
// not the server's fault and does not count.
func IsConnectivityError(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) || errors.Is(err, context.Canceled) {
		return false
	}
	var replyErr redis.Error
	if errors.As(err, &replyErr) {
		msg := strings.TrimSpace(replyErr.Error())
		for _, prefix := range unusableServerPrefixes {
			if strings.HasPrefix(msg, prefix) {
				return true
			}
		}
		return false
	}
	return true
}

func (c *Client) markDown(err error) {
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
	c.mu.Unlock()
	log.WithError(err).WithField("addr", c.addr).Warn("cluster redis: marked unavailable; limits, slots and affinity fall back to node-local state")
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
// The first attempt may have to open a connection, so it gets the background
// timeout. The server only counts as available when a command on a warm
// connection completes within the request-path budget: a Redis that answers,
// but slower than requests may wait, must stay out of the request path.
func (c *Client) probe() {
	value := strconv.FormatInt(time.Now().UnixMilli(), 10)
	err := c.probeOnce(BackgroundTimeout, value)
	if err == nil {
		err = c.probeOnce(c.opts.OpTimeout, value)
	}
	if err != nil {
		c.probeFailed(err)
		return
	}
	c.probeSucceeded()
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
	now := time.Now()
	if c.available.Load() {
		return
	}
	c.mu.Lock()
	downSince := c.changedAt
	c.healthySince = now
	c.changedAt = now
	c.lastErr = nil
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
		c.markDown(err)
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
