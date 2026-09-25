package configsync

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// DefaultWindow is how long the dispatcher waits after the first event of a
// burst before reloading. A panel save often writes several keys or rows in a
// row; reloading once for all of them keeps a burst from turning into a burst
// of reloads, while staying well inside the one-to-two-second budget for a
// change to reach every node.
const DefaultWindow = 300 * time.Millisecond

// Handler reloads one domain after other nodes changed it. events holds every
// distinct (tenant, key) announced for the domain during the coalescing
// window, with the highest version seen for each.
type Handler func(ctx context.Context, events []cluster.ConfigEvent) error

// Options configures a Dispatcher.
type Options struct {
	// Window coalesces bursts; zero means DefaultWindow.
	Window time.Duration
	// Handlers maps a domain to its reload function.
	Handlers map[string]Handler
	// FullReload rebuilds every configuration cache from the database. It runs
	// for Resync events, because a reconnected listener may have missed any
	// number of changes, and for domains without a handler, which a newer node
	// may announce during a rolling upgrade.
	FullReload func(ctx context.Context) error
}

type pendingKey struct {
	domain string
	tenant string
	key    string
}

// Dispatcher applies configuration events from other nodes. Events are only
// queued on the bus goroutine; a single worker drains the queue, so reloads
// never run concurrently with each other and never block the bus.
type Dispatcher struct {
	window   time.Duration
	handlers map[string]Handler
	full     func(ctx context.Context) error

	mu      sync.Mutex
	pending map[pendingKey]cluster.ConfigEvent
	order   []pendingKey
	resync  bool
	busy    bool
	started bool

	wake      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	unsub     func()
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewDispatcher builds a dispatcher; Start attaches it to a coordinator.
func NewDispatcher(opts Options) *Dispatcher {
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	handlers := make(map[string]Handler, len(opts.Handlers))
	for domain, handler := range opts.Handlers {
		if handler != nil {
			handlers[strings.TrimSpace(domain)] = handler
		}
	}
	return &Dispatcher{
		window:   window,
		handlers: handlers,
		full:     opts.FullReload,
		pending:  make(map[pendingKey]cluster.ConfigEvent),
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start subscribes to cluster.TopicConfig on coord and starts the worker. It
// is a no-op after the first call. A nil coord starts only the worker, for
// callers that feed events through Enqueue themselves.
func (d *Dispatcher) Start(coord *cluster.Coordinator) {
	if d == nil {
		return
	}
	d.startOnce.Do(func() {
		d.mu.Lock()
		d.started = true
		d.mu.Unlock()
		if coord != nil {
			d.unsub = coord.Subscribe(cluster.TopicConfig, d.Enqueue)
		}
		go d.run()
	})
}

// Stop unsubscribes and waits for the worker to finish the reload it is
// running, if any. Queued events are dropped.
func (d *Dispatcher) Stop() {
	if d == nil {
		return
	}
	d.stopOnce.Do(func() {
		d.startOnce.Do(func() {}) // a later Start must not launch a worker
		if d.unsub != nil {
			d.unsub()
		}
		close(d.stop)
		d.mu.Lock()
		started := d.started
		d.mu.Unlock()
		if started {
			<-d.done
		}
	})
}

// Enqueue records an event for the worker. It never blocks, which is the
// contract of a cluster subscriber.
func (d *Dispatcher) Enqueue(ev cluster.Event) {
	if d == nil {
		return
	}
	if ev.Resync {
		d.mu.Lock()
		d.resync = true
		d.mu.Unlock()
		d.signal()
		return
	}
	var payload cluster.ConfigEvent
	if err := ev.Decode(&payload); err != nil || strings.TrimSpace(payload.Domain) == "" {
		// An event we cannot read still says "something changed"; a full
		// reload is the only safe interpretation.
		log.WithError(err).Warnf("configsync: unreadable config event from %s, scheduling a full reload", ev.Origin)
		d.mu.Lock()
		d.resync = true
		d.mu.Unlock()
		d.signal()
		return
	}
	payload.Domain = strings.TrimSpace(payload.Domain)
	payload.TenantID = NormalizeTenantID(payload.TenantID)
	key := pendingKey{domain: payload.Domain, tenant: payload.TenantID, key: payload.Key}
	d.mu.Lock()
	if existing, ok := d.pending[key]; ok {
		if payload.Version > existing.Version {
			d.pending[key] = payload
		}
	} else {
		d.pending[key] = payload
		d.order = append(d.order, key)
	}
	d.mu.Unlock()
	d.signal()
}

func (d *Dispatcher) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) run() {
	defer close(d.done)
	for {
		select {
		case <-d.stop:
			return
		case <-d.wake:
		}
		timer := time.NewTimer(d.window)
		select {
		case <-d.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		d.drainAndProcess()
	}
}

func (d *Dispatcher) drainAndProcess() {
	d.mu.Lock()
	batch := make([]cluster.ConfigEvent, 0, len(d.order))
	for _, key := range d.order {
		batch = append(batch, d.pending[key])
	}
	resync := d.resync
	d.pending = make(map[pendingKey]cluster.ConfigEvent)
	d.order = nil
	d.resync = false
	d.busy = true
	d.mu.Unlock()

	d.process(batch, resync)

	d.mu.Lock()
	d.busy = false
	d.mu.Unlock()
}

func (d *Dispatcher) process(batch []cluster.ConfigEvent, resync bool) {
	ctx := context.Background()
	if !resync {
		for _, ev := range batch {
			if _, ok := d.handlers[ev.Domain]; !ok {
				log.Warnf("configsync: no reload handler for domain %q, running a full reload", ev.Domain)
				resync = true
				break
			}
		}
	}
	if resync {
		// A full reload rebuilds every domain, so the queued events are covered.
		d.call("full reload", func() error {
			if d.full == nil {
				return nil
			}
			return d.full(ctx)
		})
		return
	}
	byDomain := make(map[string][]cluster.ConfigEvent)
	domains := make([]string, 0)
	for _, ev := range batch {
		if _, seen := byDomain[ev.Domain]; !seen {
			domains = append(domains, ev.Domain)
		}
		byDomain[ev.Domain] = append(byDomain[ev.Domain], ev)
	}
	for _, domain := range domains {
		handler := d.handlers[domain]
		events := byDomain[domain]
		d.call("reload "+domain, func() error { return handler(ctx, events) })
	}
}

// call runs one reload and keeps the worker alive whatever it does: a panic
// in one domain must not stop every later event from being applied.
func (d *Dispatcher) call(label string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("configsync: %s panicked: %v", label, r)
		}
	}()
	started := time.Now()
	if err := fn(); err != nil {
		log.WithError(err).Warnf("configsync: %s failed", label)
		return
	}
	log.Debugf("configsync: %s done in %s", label, time.Since(started).Round(time.Millisecond))
}

// WaitIdle blocks until nothing is queued or running, or ctx ends. Tests use
// it to wait for a peer to apply a change.
func (d *Dispatcher) WaitIdle(ctx context.Context) error {
	if d == nil {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		d.mu.Lock()
		idle := !d.busy && len(d.order) == 0 && !d.resync && len(d.wake) == 0
		d.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("configsync: dispatcher still busy: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
