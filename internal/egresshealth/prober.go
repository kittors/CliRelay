// Package egresshealth checks that this node can still reach the proxies its
// upstream traffic leaves through.
//
// A node can pass every readiness check and still fail every request it
// serves: when its network loses the route to the proxy provider, each upstream
// call through the proxy pool times out, while /readyz, which looks at the
// process and its database, stays green. The prober connects to every
// configured proxy endpoint on a timer and /readyz/egress reports the outcome,
// so the arbiter's DNS watcher can prefer the nodes whose egress works.
//
// A check is a plain TCP connect to the proxy's host and port, closed at once.
// It sends no bytes, performs no proxy handshake and never uses credentials: a
// route blackhole shows up as a connect timeout, and nothing the check does
// can count against an account or a proxy subscription.
package egresshealth

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Defaults for Options left at zero.
const (
	DefaultInterval    = 15 * time.Second
	DefaultTimeout     = 3 * time.Second
	DefaultConcurrency = 16
)

// FailThreshold is how many consecutive failed connects mark an endpoint
// unreachable, so a single lost SYN is not reported as an outage.
const FailThreshold = 2

// Targets is what one round checks.
type Targets struct {
	// ProxyURLs lists every proxy upstream traffic may leave through.
	// Duplicates, blanks and unsupported entries are fine.
	ProxyURLs []string
	// IPv4Only connects over IPv4 only, as upstream traffic does under
	// prefer-ipv4.
	IPv4Only bool
}

// DialFunc matches net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Options configures a Prober. Zero values take the defaults.
type Options struct {
	// Targets is called at the start of every round.
	Targets func(context.Context) Targets
	// Interval spaces the rounds.
	Interval time.Duration
	// Timeout bounds each connect.
	Timeout time.Duration
	// Concurrency bounds the connects in flight.
	Concurrency int
	// Dial opens the test connections; nil uses a net.Dialer. Tests replace it.
	Dial DialFunc
}

// Status is the outcome of the latest round.
type Status struct {
	// Checked is false until the first round has finished.
	Checked bool
	// Total counts the distinct endpoints checked.
	Total int
	// Unreachable counts the endpoints that failed FailThreshold consecutive
	// checks and have not connected since.
	Unreachable int
}

// Prober checks the proxy endpoints on a timer. Its methods are safe on a nil
// *Prober, which reports an unchecked status with nothing unreachable.
type Prober struct {
	opts Options

	// endpoints is owned by the goroutine running rounds: the loop started by
	// Start, or a test calling RunOnce directly, never both.
	endpoints map[string]*endpointState

	mu     sync.RWMutex
	status Status

	lifecycle sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	stopped   bool
}

type endpointState struct {
	failures    int
	unreachable bool
}

// New builds a Prober. It does nothing until Start.
func New(opts Options) *Prober {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Dial == nil {
		opts.Dial = (&net.Dialer{Timeout: opts.Timeout}).DialContext
	}
	return &Prober{opts: opts, endpoints: map[string]*endpointState{}}
}

// Start runs the first round at once, so a restarted node reports real results
// within seconds, then one round per interval until ctx is cancelled or Stop
// is called. It returns immediately. Calling it again, or after Stop, does
// nothing.
func (p *Prober) Start(ctx context.Context) {
	if p == nil {
		return
	}
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	if p.done != nil || p.stopped {
		return
	}
	ctx, p.cancel = context.WithCancel(ctx)
	p.done = make(chan struct{})
	go p.loop(ctx, p.done)
}

// Stop ends the loop and waits for the round in progress to wind down, which
// the cancelled connects make quick.
func (p *Prober) Stop() {
	if p == nil {
		return
	}
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.stopped = true
	if p.cancel == nil {
		return
	}
	p.cancel()
	p.cancel = nil
	<-p.done
}

// Status returns the outcome of the latest round.
func (p *Prober) Status() Status {
	if p == nil {
		return Status{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status
}

func (p *Prober) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()
	for {
		p.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce performs one round: it collects the endpoints, connects to each and
// publishes the outcome. The loop started by Start calls it; tests call it
// directly instead of starting the loop.
func (p *Prober) RunOnce(ctx context.Context) {
	if p == nil {
		return
	}
	var targets Targets
	if p.opts.Targets != nil {
		targets = p.opts.Targets(ctx)
	}
	endpoints, skipped := Endpoints(targets.ProxyURLs)
	if skipped > 0 {
		// The URLs stay out of the log: they carry proxy credentials.
		log.Debugf("egress-health: skipped %d proxy URL(s) that name no supported proxy endpoint", skipped)
	}
	network := "tcp"
	if targets.IPv4Only {
		network = "tcp4"
	}
	errs := p.checkAll(ctx, network, endpoints)
	if ctx.Err() != nil {
		// Shutting down: cancelled connects say nothing about the endpoints.
		return
	}
	p.record(endpoints, errs)
}

// checkAll connects to every endpoint, at most Concurrency at a time, and
// returns the errors in endpoint order.
func (p *Prober) checkAll(ctx context.Context, network string, endpoints []string) []error {
	errs := make([]error, len(endpoints))
	slots := make(chan struct{}, p.opts.Concurrency)
	var wg sync.WaitGroup
	for i, endpoint := range endpoints {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			errs[i] = p.check(ctx, network, endpoint)
		})
	}
	wg.Wait()
	return errs
}

// check opens a TCP connection to endpoint and closes it straight away.
func (p *Prober) check(ctx context.Context, network, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()
	conn, err := p.opts.Dial(ctx, network, endpoint)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// record folds one round into the per-endpoint streaks, logs every endpoint
// that changes state and publishes the new status. Endpoints no longer
// configured are forgotten, so disabling a dead proxy clears its failure.
func (p *Prober) record(endpoints []string, errs []error) {
	next := make(map[string]*endpointState, len(endpoints))
	unreachable := 0
	for i, endpoint := range endpoints {
		state := p.endpoints[endpoint]
		if state == nil {
			state = &endpointState{}
		}
		next[endpoint] = state
		if errs[i] == nil {
			if state.unreachable {
				log.Infof("egress-health: proxy endpoint %s is reachable again", endpoint)
			}
			state.failures, state.unreachable = 0, false
			continue
		}
		state.failures++
		if !state.unreachable && state.failures >= FailThreshold {
			state.unreachable = true
			log.Warnf("egress-health: proxy endpoint %s is unreachable after %d consecutive failed connects: %s",
				endpoint, state.failures, describeDialError(errs[i]))
		} else if !state.unreachable {
			log.Debugf("egress-health: connect to proxy endpoint %s failed (%d/%d): %s",
				endpoint, state.failures, FailThreshold, describeDialError(errs[i]))
		}
		if state.unreachable {
			unreachable++
		}
	}
	for endpoint, state := range p.endpoints {
		if _, kept := next[endpoint]; !kept && state.unreachable {
			log.Infof("egress-health: proxy endpoint %s is no longer configured and stops counting as unreachable", endpoint)
		}
	}
	p.endpoints = next

	p.mu.Lock()
	p.status = Status{Checked: true, Total: len(endpoints), Unreachable: unreachable}
	p.mu.Unlock()
}

// describeDialError shortens a connect error for the log: the endpoint is
// already on the line, and the net.OpError wrapper would only repeat it.
func describeDialError(err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err.Error()
	}
	return err.Error()
}
