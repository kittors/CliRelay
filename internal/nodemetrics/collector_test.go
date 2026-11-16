package nodemetrics

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

var testStart = time.Date(2026, 11, 16, 22, 39, 0, 0, time.UTC)

func newTestCollector(t *testing.T, root string) (*Collector, *testClock) {
	t.Helper()
	clock := &testClock{now: testStart}
	return New(Options{Root: root, Now: clock.Now, StartedAt: testStart}), clock
}

const cgroupRel = "sys/fs/cgroup/system.slice/clirelay2-8319.service/"

// hostFixture is an 8-CPU host as hk-155 looked on 2026-11-16: over 10
// seconds (100 ticks a second, 8000 ticks in all) it ran 0.76 cores and lost
// 2.76 to steal, while the slot's cgroup recorded 2.55 cores.
func hostFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, "proc/stat", procStatLine(1000, 50, 400, 50000, 100, 10, 40, 20000))
	writeFixture(t, root, "proc/loadavg", "6.50 6.10 5.80 9/900 4242\n")
	writeFixture(t, root, "proc/meminfo", "MemTotal: 8131072 kB\nMemAvailable: 4065536 kB\n")
	writeFixture(t, root, "proc/self/cgroup", "0::/system.slice/clirelay2-8319.service\n")
	writeFixture(t, root, cgroupRel+"cpu.stat", "usage_usec 100000000\nnr_periods 1000\nnr_throttled 50\nthrottled_usec 2000000\n")
	writeFixture(t, root, cgroupRel+"cpu.max", "700000 100000\n")
	writeFixture(t, root, cgroupRel+"memory.current", "1610612736\n")
	writeFixture(t, root, cgroupRel+"memory.high", "3221225472\n")
	writeFixture(t, root, cgroupRel+"memory.max", "4294967296\n")
	return root
}

func procStatLine(user, nice, system, idle, iowait, irq, softirq, steal uint64) string {
	var b strings.Builder
	line := func(name string) {
		b.WriteString(name)
		for _, v := range []uint64{user, nice, system, idle, iowait, irq, softirq, steal, 0, 0} {
			b.WriteString(" ")
			b.WriteString(uintString(v))
		}
		b.WriteString("\n")
	}
	line("cpu ")
	for _, cpu := range []string{"cpu0", "cpu1", "cpu2", "cpu3", "cpu4", "cpu5", "cpu6", "cpu7"} {
		line(cpu)
	}
	return b.String()
}

func uintString(v uint64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestCollectorSplitsHostCPUFromSteal(t *testing.T) {
	root := hostFixture(t)
	c, clock := newTestCollector(t, root)
	ctx := context.Background()

	c.sample(ctx)
	first, ok := c.Latest()
	if !ok || first.Host == nil || first.Host.CPUs != 8 || first.Host.CPU != nil {
		t.Fatalf("the first sample has counters but no window yet: %+v", first.Host)
	}
	if first.Cgroup == nil || first.Cgroup.CPU == nil || first.Cgroup.CPU.Window != nil {
		t.Fatalf("the first cgroup sample has no window yet: %+v", first.Cgroup)
	}

	// Ten seconds later: +760 busy (500 user, 30 nice, 200 system, 10 irq,
	// 20 softirq), +2760 steal, +20 iowait, +4460 idle.
	clock.Advance(10 * time.Second)
	writeFixture(t, root, "proc/stat", procStatLine(1500, 80, 600, 54460, 120, 20, 60, 22760))
	writeFixture(t, root, cgroupRel+"cpu.stat", "usage_usec 125500000\nnr_periods 1100\nnr_throttled 62\nthrottled_usec 2300000\n")
	c.sample(ctx)
	snap, _ := c.Latest()

	want := HostCPU{WindowSeconds: 10, BusyCores: 0.76, StealCores: 2.76, IOWaitCores: 0.02, IdleCores: 4.46}
	if snap.Host.CPU == nil || *snap.Host.CPU != want {
		t.Fatalf("host cpu = %+v, want %+v", snap.Host.CPU, want)
	}
	if *snap.Host.Load != (Load{Load1: 6.5, Load5: 6.1, Load15: 5.8}) || snap.Host.Memory.TotalBytes != 8131072*1024 {
		t.Fatalf("load/memory = %+v %+v", snap.Host.Load, snap.Host.Memory)
	}

	cpu := snap.Cgroup.CPU
	if snap.Cgroup.Path != "/system.slice/clirelay2-8319.service" || cpu.QuotaCores == nil || *cpu.QuotaCores != 7 {
		t.Fatalf("cgroup = %+v", snap.Cgroup)
	}
	wantWindow := CgroupCPUWindow{Seconds: 10, UsageCores: 2.55, ThrottledRatio: 0.12, ThrottledSeconds: 0.3}
	if cpu.Window == nil || *cpu.Window != wantWindow {
		t.Fatalf("cgroup window = %+v, want %+v", cpu.Window, wantWindow)
	}
	if cpu.UsageUsec != 125500000 || cpu.NrPeriods != 1100 || cpu.NrThrottled != 62 || cpu.ThrottledUsec != 2300000 {
		t.Fatalf("cgroup counters = %+v", cpu)
	}
	mem := snap.Cgroup.Memory
	if mem == nil || mem.CurrentBytes != 1610612736 || *mem.HighBytes != 3221225472 || *mem.MaxBytes != 4294967296 {
		t.Fatalf("cgroup memory = %+v", mem)
	}
}

func TestCollectorLeavesOutWhatItCannotRead(t *testing.T) {
	c, _ := newTestCollector(t, t.TempDir())
	c.sample(context.Background())
	snap, ok := c.Latest()
	if !ok {
		t.Fatal("a snapshot must be published even with nothing to read")
	}
	if snap.Host != nil || snap.Cgroup != nil || snap.DB != nil || snap.Redis != nil || snap.Egress != nil {
		t.Fatalf("unreadable parts must be left out: %+v", snap)
	}
	raw, at := c.LatestJSON()
	if !at.Equal(testStart) {
		t.Fatalf("collected at %s, want %s", at, testStart)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"host", "cgroup", "db", "redis", "egress"} {
		if _, present := decoded[key]; present {
			t.Fatalf("%s should be absent from %s", key, raw)
		}
	}
	if decoded["schema"] != float64(SchemaVersion) || decoded["gomaxprocs"] == float64(0) {
		t.Fatalf("unexpected snapshot %s", raw)
	}
}

func TestCollectorDropsTheWindowWhenTheCgroupChanges(t *testing.T) {
	root := hostFixture(t)
	c, clock := newTestCollector(t, root)
	c.sample(context.Background())
	clock.Advance(5 * time.Second)
	other := "sys/fs/cgroup/system.slice/clirelay2-8318.service/"
	writeFixture(t, root, "proc/self/cgroup", "0::/system.slice/clirelay2-8318.service\n")
	writeFixture(t, root, other+"cpu.stat", "usage_usec 5\n")
	c.sample(context.Background())
	snap, _ := c.Latest()
	if snap.Cgroup.CPU.Window != nil || snap.Cgroup.Memory != nil {
		t.Fatalf("counters of another cgroup must not be subtracted: %+v", snap.Cgroup)
	}
	if snap.Cgroup.CPU.QuotaCores != nil {
		t.Fatalf("no cpu.max there, so no quota: %v", *snap.Cgroup.CPU.QuotaCores)
	}
}

func TestCollectorTrafficWindowCoversTheLastMinute(t *testing.T) {
	c, clock := newTestCollector(t, t.TempDir())
	var inFlight int64 = 7
	c.SetSources(Sources{Version: "dev-test", InFlight: func() int64 { return inFlight }})
	ctx := context.Background()

	// Young process: the window starts with the process.
	clock.Advance(time.Second)
	for range 3 {
		c.CountRequest()
	}
	c.traffic.bytesIn.Add(300)
	c.traffic.bytesOut.Add(9000)
	clock.Advance(4 * time.Second)
	c.sample(ctx)
	snap, _ := c.Latest()
	if got := snap.Traffic; got != (Traffic{InFlight: 7, WindowSeconds: 5, Requests: 3, BytesIn: 300, BytesOut: 9000}) {
		t.Fatalf("traffic = %+v", got)
	}
	if snap.Version != "dev-test" || !snap.StartedAt.Equal(testStart) {
		t.Fatalf("version/start = %q %s", snap.Version, snap.StartedAt)
	}

	// Two requests per five-second sample for well over a minute: the count
	// settles on the window's share and no longer grows.
	for range 20 {
		c.CountRequest()
		c.CountRequest()
		clock.Advance(5 * time.Second)
		c.sample(ctx)
	}
	snap, _ = c.Latest()
	traffic := snap.Traffic
	if traffic.WindowSeconds < 60 || traffic.WindowSeconds > 65 {
		t.Fatalf("window = %v s, want about a minute", traffic.WindowSeconds)
	}
	if want := uint64(traffic.WindowSeconds / 5 * 2); traffic.Requests != want {
		t.Fatalf("requests = %d over %v s, want %d", traffic.Requests, traffic.WindowSeconds, want)
	}
	if traffic.BytesIn != 0 || traffic.BytesOut != 0 {
		t.Fatalf("the early bytes fell out of the window, got %+v", traffic)
	}
	if len(c.history) > 16 {
		t.Fatalf("history must stay bounded, has %d samples", len(c.history))
	}
}

func TestRoundTripReportsFailuresByKind(t *testing.T) {
	ctx := context.Background()
	if got := roundTrip(ctx, "", func(context.Context) (string, error) { return "", ErrNotConfigured }); got != nil {
		t.Fatalf("a dependency that is not configured is left out, got %+v", got)
	}
	ok := roundTrip(ctx, "", func(context.Context) (string, error) { return "cluster", nil })
	if ok.Role != "cluster" || ok.RTTMillis == nil || ok.Error != "" {
		t.Fatalf("success = %+v", ok)
	}
	slow := roundTrip(ctx, "", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "usage", ctx.Err()
	})
	if slow.Role != "usage" || slow.Error != "timeout" || slow.RTTMillis != nil {
		t.Fatalf("timeout = %+v", slow)
	}
	failed := roundTrip(ctx, "", func(context.Context) (string, error) {
		return "", errors.New("dial tcp 198.51.100.9:55432: connect: connection refused (user=cliproxy)")
	})
	if failed.Error != "error" {
		t.Fatalf("failure = %+v", failed)
	}
	if raw, _ := json.Marshal(failed); strings.Contains(string(raw), "198.51.100.9") || strings.Contains(string(raw), "cliproxy") {
		t.Fatalf("the cause must stay out of the snapshot: %s", raw)
	}
}

func TestCollectorReportsDependenciesAndEgress(t *testing.T) {
	c, _ := newTestCollector(t, t.TempDir())
	since := testStart.Add(-time.Minute)
	c.SetSources(Sources{
		PingDB:    func(context.Context) error { return nil },
		PingRedis: func(context.Context) (string, error) { return "", ErrNotConfigured },
		Egress: func() *Egress {
			return &Egress{BreakerEnabled: true, BreakerOpen: 1, Unreachable: 1, OpenSince: &since, Check: &EgressCheck{Total: 4, Unreachable: 1}}
		},
	})
	c.sample(context.Background())
	snap, _ := c.Latest()
	if snap.DB == nil || snap.DB.RTTMillis == nil || snap.DB.Role != "" {
		t.Fatalf("db = %+v", snap.DB)
	}
	if snap.Redis != nil {
		t.Fatalf("no Redis is configured, got %+v", snap.Redis)
	}
	if snap.Egress == nil || snap.Egress.BreakerOpen != 1 || snap.Egress.Check.Total != 4 {
		t.Fatalf("egress = %+v", snap.Egress)
	}
}

func TestCollectorStartSamplesAtOnceAndStops(t *testing.T) {
	c := New(Options{Root: t.TempDir(), Interval: time.Hour})
	if _, ok := c.Latest(); ok {
		t.Fatal("nothing is published before the first sample")
	}
	c.Start()
	c.Start() // a second Start must not start a second loop
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := c.Latest(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start should sample at once")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.Stop()
	c.Stop()
	if raw := c.SnapshotJSON(); len(raw) == 0 {
		t.Fatal("the latest snapshot stays readable after Stop")
	}
}
