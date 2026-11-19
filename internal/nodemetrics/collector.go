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
	// cpuSpan is the window of the CPU figures, host and cgroup: long enough
	// to smooth out the jumps between two 5-second samples, short enough to
	// show a change within a minute.
	cpuSpan = 30 * time.Second
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
	// SharedRedis reports the cluster Redis; nil, or a nil result, on a
	// single node.
	SharedRedis func() *SharedRedis
	// DiskPaths lists the directories whose filesystems the snapshot
	// reports, besides the root filesystem, which is always reported.
	DiskPaths func() []DiskPath
	// Database returns the latest sample of the PostgreSQL server
	// (DatabaseProbe.Latest); nil, or a nil result, on nodes that do not
	// sample it.
	Database func() *Database
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
	// or a test calling sample directly. Each history is ordered by time and
	// its first entry is where the window starts (see slide).
	sampleMu      sync.Mutex
	cpuHistory    []cpuSample
	cgroupHistory []cgroupSample
	// history holds traffic counts; it starts at the process start, when the
	// counters were zero.
	history []trafficSample
	// diskHistory holds one point a minute of each filesystem's used space,
	// keyed by mount point and device, for DiskGrowth.
	diskHistory map[string][]diskSample
	// statPath reads a filesystem; tests replace it.
	statPath func(path string) (fsStat, error)

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
		history:  []trafficSample{{at: opts.StartedAt}},
		statPath: platformStatPath,
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

// Sample takes one snapshot now, publishes it and returns it; ok is false
// when ctx ended during the sample. The loop started by Start calls the same
// thing; this is for one-off reads such as clirelay-hostagent -once.
func (c *Collector) Sample(ctx context.Context) (snapshot Snapshot, ok bool) {
	c.sample(ctx)
	if ctx.Err() != nil {
		return Snapshot{}, false
	}
	return c.Latest()
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
	var diskPaths []DiskPath
	if sources.DiskPaths != nil {
		diskPaths = sources.DiskPaths()
	}
	snap.Disks = c.sampleDisks(now, diskPaths)
	snap.Traffic = c.sampleTraffic(now, sources)
	if sources.Egress != nil {
		snap.Egress = sources.Egress()
	}
	if sources.SharedRedis != nil {
		snap.SharedRedis = sources.SharedRedis()
	}
	if sources.Database != nil {
		// Sampled on its own timer by the leader; this only reads the latest.
		snap.Database = sources.Database()
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

// slide appends cur to history and drops the samples the window no longer
// needs. The window starts at the newest sample at least span older than
// cur, or at the oldest one kept while the history is younger than span; ok
// is false while there is nothing before cur.
func slide[T any](history []T, cur T, at func(T) time.Time, span time.Duration) (kept []T, start T, ok bool) {
	history = append(history, cur)
	cut := at(cur).Add(-span)
	for len(history) > 2 && !at(history[1]).After(cut) {
		history = history[1:]
	}
	if len(history) < 2 {
		return history, start, false
	}
	return history, history[0], true
}

func (c *Collector) sampleHost(now time.Time) *Host {
	host := &Host{}
	if times, cpus, ok := readProcStat(c.root); ok {
		host.CPUs = cpus
		var start cpuSample
		c.cpuHistory, start, ok = slide(c.cpuHistory, cpuSample{at: now, times: times}, func(s cpuSample) time.Time { return s.at }, cpuSpan)
		if ok {
			host.CPU = cpuWindow(start.times, times, cpus, now.Sub(start.at))
		}
	} else {
		c.cpuHistory = nil
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
		c.cgroupHistory = nil
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
		if len(c.cgroupHistory) > 0 && c.cgroupHistory[0].path != path {
			// Counters of another cgroup cannot be subtracted from these.
			c.cgroupHistory = nil
		}
		var start cgroupSample
		c.cgroupHistory, start, ok = slide(c.cgroupHistory, cgroupSample{at: now, path: path, stat: stat}, func(s cgroupSample) time.Time { return s.at }, cpuSpan)
		if ok {
			cpu.Window = cgroupWindow(start.stat, stat, now.Sub(start.at))
		}
		group.CPU = cpu
	} else {
		c.cgroupHistory = nil
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
	var base trafficSample
	// history begins with the process start, so a window always exists.
	c.history, base, _ = slide(c.history, trafficSample{at: now, counts: counts}, func(s trafficSample) time.Time { return s.at }, trafficSpan)
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
