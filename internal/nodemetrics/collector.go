package nodemetrics

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultInterval matches the cluster heartbeat, which publishes the
	// latest snapshot, so every heartbeat carries a sample at most one
	// interval old.
	DefaultInterval = 5 * time.Second
	// trafficSpan is the window of the request and byte counts.
	trafficSpan = time.Minute
	// pingTimeout bounds each round trip; a dependency slower than this is
	// reported as a timeout.
	pingTimeout = 2 * time.Second
)

// ErrNotConfigured is what a ping source returns for a dependency this node
// does not use; the snapshot then leaves the dependency out.
var ErrNotConfigured = errors.New("nodemetrics: not configured")

// Sources is what the collector asks the rest of the process for. Every field
// is optional.
type Sources struct {
	Version string
	// InFlight returns the number of HTTP requests being handled.
	InFlight func() int64
	// PingDB makes one round trip to the runtime database.
	PingDB func(ctx context.Context) error
	// PingRedis makes one round trip to the Redis this node relies on and
	// names it ("cluster" or "usage"); ErrNotConfigured when there is none.
	PingRedis func(ctx context.Context) (role string, err error)
	// Egress reports the egress breaker and connect check.
	Egress func() *Egress
}

// Options configures a Collector. Zero values take the defaults; tests set
// Root to a fixture directory and Now to a fake clock.
type Options struct {
	Root      string
	Interval  time.Duration
	Now       func() time.Time
	StartedAt time.Time
}

// Collector samples on a timer and keeps the latest snapshot. Its methods
// are safe for concurrent use.
type Collector struct {
	root      string
	interval  time.Duration
	now       func() time.Time
	startedAt time.Time
	traffic   trafficCounters

	sourcesMu sync.RWMutex
	sources   Sources

	// The sampling state below belongs to whoever holds sampleMu: the loop,
	// or a test calling sample directly.
	sampleMu   sync.Mutex
	prevCPU    *cpuSample
	prevCgroup *cgroupSample
	// history holds traffic counts by time; history[0] is the newest sample
	// at least trafficSpan old, or the start of the process while it is
	// younger than that.
	history []trafficSample

	latest atomic.Pointer[published]

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

type cpuSample struct {
	at    time.Time
	times cpuTimes
}

type cgroupSample struct {
	at   time.Time
	path string
	stat cgroupCPUStat
}

type trafficSample struct {
	at     time.Time
	counts trafficCounts
}

type published struct {
	snapshot Snapshot
	raw      json.RawMessage
}

// processStartedAt is when this package was initialised, within moments of
// the process starting.
var processStartedAt = time.Now()

var defaultCollector = New(Options{})

// Default is the process-wide collector. The API server feeds and runs it;
// the cluster heartbeat and the console read it.
func Default() *Collector { return defaultCollector }

// New builds a collector. It samples nothing until Start, or a direct call
// in tests.
func New(opts Options) *Collector {
	if opts.Root == "" {
		opts.Root = "/"
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = processStartedAt
	}
	return &Collector{
		root:      opts.Root,
		interval:  opts.Interval,
		now:       opts.Now,
		startedAt: opts.StartedAt,
		// The counters start at zero with the process, which makes its start
		// the first point of the traffic window.
		history: []trafficSample{{at: opts.StartedAt}},
	}
}

// SetSources replaces what the collector asks the process for.
func (c *Collector) SetSources(sources Sources) {
	c.sourcesMu.Lock()
	c.sources = sources
	c.sourcesMu.Unlock()
}

func (c *Collector) currentSources() Sources {
	c.sourcesMu.RLock()
	defer c.sourcesMu.RUnlock()
	return c.sources
}

// WrapListener counts the bytes of every connection l accepts.
func (c *Collector) WrapListener(l net.Listener) net.Listener {
	return countingListener{Listener: l, counters: &c.traffic}
}

// CountRequest records one HTTP request; the server's outermost middleware
// calls it as the request comes in.
func (c *Collector) CountRequest() { c.traffic.requests.Add(1) }

// Totals returns the request and byte counters since the process started,
// which the snapshot's traffic window is computed from.
func (c *Collector) Totals() (requests, bytesIn, bytesOut uint64) {
	counts := c.traffic.load()
	return counts.requests, counts.bytesIn, counts.bytesOut
}

// Start samples at once and then every interval until Stop. Calling it again
// while it runs does nothing.
func (c *Collector) Start() {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.cancel, c.done = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			c.sample(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop ends sampling and waits for a sample in progress, which its round
// trip timeouts bound. The latest snapshot stays readable.
func (c *Collector) Stop() {
	c.runMu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel, c.done = nil, nil
	c.runMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Latest returns the most recent snapshot; ok is false before the first.
func (c *Collector) Latest() (snapshot Snapshot, ok bool) {
	if p := c.latest.Load(); p != nil {
		return p.snapshot, true
	}
	return Snapshot{}, false
}

// LatestJSON returns the most recent snapshot encoded, and when it was taken;
// raw is nil before the first.
func (c *Collector) LatestJSON() (raw json.RawMessage, collectedAt time.Time) {
	if p := c.latest.Load(); p != nil {
		return p.raw, p.snapshot.CollectedAt
	}
	return nil, time.Time{}
}

// SnapshotJSON is LatestJSON without the time, the shape the cluster
// heartbeat takes.
func (c *Collector) SnapshotJSON() json.RawMessage {
	raw, _ := c.LatestJSON()
	return raw
}

// sample takes one snapshot and publishes it.
func (c *Collector) sample(ctx context.Context) {
	c.sampleMu.Lock()
	defer c.sampleMu.Unlock()

	sources := c.currentSources()
	now := c.now()
	snap := Snapshot{
		Schema:      SchemaVersion,
		CollectedAt: now.UTC(),
		Version:     sources.Version,
		StartedAt:   c.startedAt.UTC(),
		GOMAXPROCS:  runtime.GOMAXPROCS(0),
		Goroutines:  runtime.NumGoroutine(),
	}
	if limit := debug.SetMemoryLimit(-1); limit != math.MaxInt64 {
		snap.MemoryLimitBytes = &limit
	}
	snap.Host = c.sampleHost(now)
	snap.Cgroup = c.sampleCgroup(now)
	snap.Traffic = c.sampleTraffic(now, sources)
	if sources.Egress != nil {
		snap.Egress = sources.Egress()
	}
	// The round trips go last: they are the only part that can take a while.
	if sources.PingDB != nil {
		snap.DB = roundTrip(ctx, "", func(ctx context.Context) (string, error) { return "", sources.PingDB(ctx) })
	}
	if sources.PingRedis != nil {
		snap.Redis = roundTrip(ctx, "", sources.PingRedis)
	}

	if ctx.Err() != nil {
		// Stopped mid-sample: the cut round trips would read as timeouts.
		return
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		// Every field is a plain value; this cannot fail, and if it did the
		// previous snapshot would simply stay.
		return
	}
	c.latest.Store(&published{snapshot: snap, raw: raw})
}

func (c *Collector) sampleHost(now time.Time) *Host {
	host := &Host{}
	if times, cpus, ok := readProcStat(c.root); ok {
		host.CPUs = cpus
		if prev := c.prevCPU; prev != nil {
			host.CPU = cpuWindow(prev.times, times, cpus, now.Sub(prev.at))
		}
		c.prevCPU = &cpuSample{at: now, times: times}
	} else {
		c.prevCPU = nil
	}
	if load, ok := readLoadAvg(c.root); ok {
		host.Load = load
	}
	if mem, ok := readMemInfo(c.root); ok {
		host.Memory = mem
	}
	if host.CPUs == 0 && host.Load == nil && host.Memory == nil {
		return nil
	}
	return host
}

// cpuWindow splits the CPU time between two /proc/stat reads into cores. It
// works from shares of the total, so it needs neither the clock tick rate
// nor exact timing; nil when the counters did not move or went backwards.
func cpuWindow(prev, cur cpuTimes, cpus int, elapsed time.Duration) *HostCPU {
	if cpus <= 0 || elapsed <= 0 || cur.total() <= prev.total() {
		return nil
	}
	delta := func(a, b uint64) float64 {
		if a < b {
			return 0
		}
		return float64(a - b)
	}
	total := delta(cur.total(), prev.total())
	cores := func(part float64) float64 { return round3(part / total * float64(cpus)) }
	return &HostCPU{
		WindowSeconds: round3(elapsed.Seconds()),
		BusyCores:     cores(delta(cur.busy(), prev.busy())),
		StealCores:    cores(delta(cur.steal, prev.steal)),
		IOWaitCores:   cores(delta(cur.iowait, prev.iowait)),
		IdleCores:     cores(delta(cur.idle, prev.idle)),
	}
}

func (c *Collector) sampleCgroup(now time.Time) *Cgroup {
	path, ok := cgroupPath(c.root)
	if !ok {
		c.prevCgroup = nil
		return nil
	}
	dir := cgroupDir(c.root, path)
	group := &Cgroup{Path: path}
	if stat, ok := readCgroupCPUStat(dir); ok {
		cpu := &CgroupCPU{
			UsageUsec:     stat.usageUsec,
			NrPeriods:     stat.nrPeriods,
			NrThrottled:   stat.nrThrottled,
			ThrottledUsec: stat.throttledUsec,
			QuotaCores:    readCPUMax(dir),
		}
		if prev := c.prevCgroup; prev != nil && prev.path == path {
			cpu.Window = cgroupWindow(prev.stat, stat, now.Sub(prev.at))
		}
		c.prevCgroup = &cgroupSample{at: now, path: path, stat: stat}
		group.CPU = cpu
	} else {
		c.prevCgroup = nil
	}
	if mem, ok := readCgroupMemory(dir); ok {
		group.Memory = mem
	}
	if group.CPU == nil && group.Memory == nil {
		// Not mounted here, as in a container without cgroup access.
		return nil
	}
	return group
}

func cgroupWindow(prev, cur cgroupCPUStat, elapsed time.Duration) *CgroupCPUWindow {
	if elapsed <= 0 || cur.usageUsec < prev.usageUsec || cur.nrPeriods < prev.nrPeriods ||
		cur.nrThrottled < prev.nrThrottled || cur.throttledUsec < prev.throttledUsec {
		return nil
	}
	window := &CgroupCPUWindow{
		Seconds:          round3(elapsed.Seconds()),
		UsageCores:       round3(float64(cur.usageUsec-prev.usageUsec) / 1e6 / elapsed.Seconds()),
		ThrottledSeconds: round3(float64(cur.throttledUsec-prev.throttledUsec) / 1e6),
	}
	if periods := cur.nrPeriods - prev.nrPeriods; periods > 0 {
		window.ThrottledRatio = round3(float64(cur.nrThrottled-prev.nrThrottled) / float64(periods))
	}
	return window
}

func (c *Collector) sampleTraffic(now time.Time, sources Sources) Traffic {
	counts := c.traffic.load()
	c.history = append(c.history, trafficSample{at: now, counts: counts})
	cut := now.Add(-trafficSpan)
	// Keep the newest sample at or before the cut as the window's start.
	for len(c.history) > 2 && !c.history[1].at.After(cut) {
		c.history = c.history[1:]
	}
	base := c.history[0]
	delta := counts.minus(base.counts)
	traffic := Traffic{
		WindowSeconds: round3(max(now.Sub(base.at), 0).Seconds()),
		Requests:      delta.requests,
		BytesIn:       delta.bytesIn,
		BytesOut:      delta.bytesOut,
	}
	if sources.InFlight != nil {
		traffic.InFlight = sources.InFlight()
	}
	return traffic
}

// roundTrip times ping, reporting failures by kind only.
func roundTrip(ctx context.Context, role string, ping func(ctx context.Context) (string, error)) *RoundTrip {
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	started := time.Now()
	gotRole, err := ping(pingCtx)
	elapsed := time.Since(started)
	if errors.Is(err, ErrNotConfigured) {
		return nil
	}
	if gotRole != "" {
		role = gotRole
	}
	result := &RoundTrip{Role: role}
	switch {
	case err == nil:
		ms := round3(float64(elapsed) / float64(time.Millisecond))
		result.RTTMillis = &ms
	case errors.Is(err, context.DeadlineExceeded) || pingCtx.Err() != nil:
		result.Error = "timeout"
	default:
		result.Error = "error"
	}
	return result
}

// round3 keeps three decimals, which is plenty for the console and keeps the
// stored JSON short.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
