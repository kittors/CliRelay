package egresshealth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// fakeDialer answers connects from a table: listed endpoints fail with their
// error, everything else connects.
type fakeDialer struct {
	mu       sync.Mutex
	failures map[string]error
	calls    []string
	networks []string
}

func newFakeDialer() *fakeDialer {
	return &fakeDialer{failures: map[string]error{}}
}

func (d *fakeDialer) fail(endpoint string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failures[endpoint] = err
}

func (d *fakeDialer) heal(endpoint string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.failures, endpoint)
}

func (d *fakeDialer) dial(_ context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, address)
	d.networks = append(d.networks, network)
	if err := d.failures[address]; err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

func (d *fakeDialer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func staticTargets(urls ...string) func(context.Context) Targets {
	return func(context.Context) Targets { return Targets{ProxyURLs: urls} }
}

func TestProberNeedsTwoConsecutiveFailures(t *testing.T) {
	dialer := newFakeDialer()
	p := New(Options{
		Targets: staticTargets("socks5://user:secret@203.0.113.9:1080", "http://proxy.example.test:8080"),
		Dial:    dialer.dial,
	})
	ctx := context.Background()
	if got := p.Status(); got.Checked || got.Unreachable != 0 {
		t.Fatalf("status before the first round = %+v, want unchecked", got)
	}

	dialer.fail("203.0.113.9:1080", syscall.ETIMEDOUT)
	p.RunOnce(ctx)
	if got := p.Status(); got != (Status{Checked: true, Total: 2}) {
		t.Fatalf("one failed connect must not count, got %+v", got)
	}
	p.RunOnce(ctx)
	if got := p.Status(); got != (Status{Checked: true, Total: 2, Unreachable: 1}) {
		t.Fatalf("two consecutive failures should mark the endpoint unreachable, got %+v", got)
	}
	p.RunOnce(ctx)
	if got := p.Status(); got.Unreachable != 1 {
		t.Fatalf("the endpoint should stay unreachable while it keeps failing, got %+v", got)
	}

	dialer.heal("203.0.113.9:1080")
	p.RunOnce(ctx)
	if got := p.Status(); got != (Status{Checked: true, Total: 2}) {
		t.Fatalf("one successful connect should clear the endpoint, got %+v", got)
	}
}

func TestProberStreakMustBeConsecutive(t *testing.T) {
	dialer := newFakeDialer()
	p := New(Options{Targets: staticTargets("socks5://203.0.113.9:1080"), Dial: dialer.dial})
	for i, broken := range []bool{true, false, true, false, true} {
		if broken {
			dialer.fail("203.0.113.9:1080", syscall.ECONNREFUSED)
		} else {
			dialer.heal("203.0.113.9:1080")
		}
		p.RunOnce(context.Background())
		if got := p.Status(); got.Unreachable != 0 {
			t.Fatalf("round %d: isolated failures must be ignored, got %+v", i, got)
		}
	}
}

func TestProberLogsTransitionsWithoutCredentials(t *testing.T) {
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()

	dialer := newFakeDialer()
	urls := []string{"socks5://alice:hunter2@203.0.113.9:1080"}
	p := New(Options{
		Targets: func(context.Context) Targets { return Targets{ProxyURLs: urls} },
		Dial:    dialer.dial,
	})
	dialer.fail("203.0.113.9:1080", syscall.ECONNREFUSED)
	p.RunOnce(context.Background())
	p.RunOnce(context.Background())
	p.RunOnce(context.Background())
	dialer.heal("203.0.113.9:1080")
	p.RunOnce(context.Background())
	// Disabling the proxy while it is down clears it too.
	dialer.fail("203.0.113.9:1080", syscall.ECONNREFUSED)
	p.RunOnce(context.Background())
	p.RunOnce(context.Background())
	urls = nil
	p.RunOnce(context.Background())

	var warns, infos []string
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "hunter2") || strings.Contains(entry.Message, "alice") {
			t.Fatalf("log line leaks proxy credentials: %q", entry.Message)
		}
		if !strings.HasPrefix(entry.Message, "egress-health:") {
			continue
		}
		switch entry.Level {
		case log.WarnLevel:
			warns = append(warns, entry.Message)
		case log.InfoLevel:
			infos = append(infos, entry.Message)
		}
	}
	if len(warns) != 2 || !strings.Contains(warns[0], "203.0.113.9:1080 is unreachable after 2 consecutive failed connects") ||
		!strings.Contains(warns[0], "connection refused") {
		t.Fatalf("expected one warning per outage naming host:port and the reason, got %q", warns)
	}
	if len(infos) != 2 || !strings.Contains(infos[0], "203.0.113.9:1080 is reachable again") ||
		!strings.Contains(infos[1], "no longer configured") {
		t.Fatalf("expected a recovery line and a removal line, got %q", infos)
	}
	if got := p.Status(); got != (Status{Checked: true}) {
		t.Fatalf("a removed endpoint must stop counting, got %+v", got)
	}
}

func TestProberWithoutEndpointsDialsNothing(t *testing.T) {
	dialer := newFakeDialer()
	p := New(Options{Targets: staticTargets("", "  ", "direct"), Dial: dialer.dial})
	p.RunOnce(context.Background())
	if n := dialer.callCount(); n != 0 {
		t.Fatalf("no endpoint means no connect, got %d", n)
	}
	if got := p.Status(); got != (Status{Checked: true}) {
		t.Fatalf("status = %+v, want checked with nothing to check", got)
	}
	// A prober without a target source behaves the same.
	New(Options{Dial: dialer.dial}).RunOnce(context.Background())
	if n := dialer.callCount(); n != 0 {
		t.Fatalf("no target source means no connect, got %d", n)
	}
}

func TestProberDialsEachEndpointOnceWithTheConfiguredNetwork(t *testing.T) {
	dialer := newFakeDialer()
	ipv4 := false
	p := New(Options{
		Targets: func(context.Context) Targets {
			return Targets{
				ProxyURLs: []string{"socks5://a:1@203.0.113.9:1080", "socks5h://b:2@203.0.113.9:1080", "http://proxy.example.test"},
				IPv4Only:  ipv4,
			}
		},
		Dial: dialer.dial,
	})
	p.RunOnce(context.Background())
	ipv4 = true
	p.RunOnce(context.Background())

	dialer.mu.Lock()
	calls, networks := slices.Clone(dialer.calls), slices.Clone(dialer.networks)
	dialer.mu.Unlock()
	slices.Sort(calls)
	want := []string{"203.0.113.9:1080", "203.0.113.9:1080", "proxy.example.test:80", "proxy.example.test:80"}
	if !slices.Equal(calls, want) {
		t.Fatalf("connects = %v, want %v", calls, want)
	}
	if !slices.Equal(networks, []string{"tcp", "tcp", "tcp4", "tcp4"}) {
		t.Fatalf("networks = %v; prefer-ipv4 must restrict connects to IPv4", networks)
	}
}

func TestProberBoundsConcurrentConnects(t *testing.T) {
	var inFlight, peak atomic.Int32
	var urls []string
	for i := range 40 {
		urls = append(urls, fmt.Sprintf("socks5://203.0.113.%d:1080", i+1))
	}
	var dialed atomic.Int32
	p := New(Options{
		Targets:     staticTargets(urls...),
		Concurrency: 4,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			n := inFlight.Add(1)
			for {
				seen := peak.Load()
				if n <= seen || peak.CompareAndSwap(seen, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			dialed.Add(1)
			return nil, errors.New("refused")
		},
	})
	p.RunOnce(context.Background())
	if dialed.Load() != 40 {
		t.Fatalf("dialed %d endpoints, want 40", dialed.Load())
	}
	if got := peak.Load(); got > 4 {
		t.Fatalf("%d connects ran at once, want at most 4", got)
	}
}

func TestProberTimesOutHungConnects(t *testing.T) {
	hook := test.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p := New(Options{
		Targets: staticTargets("socks5://203.0.113.9:1080"),
		Timeout: 50 * time.Millisecond,
		// A blackholed route: the SYN is never answered.
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			<-ctx.Done()
			return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
		},
	})
	start := time.Now()
	p.RunOnce(context.Background())
	p.RunOnce(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("two rounds took %s; each connect must give up after the timeout", elapsed)
	}
	if got := p.Status(); got.Unreachable != 1 {
		t.Fatalf("a hung endpoint should be unreachable, got %+v", got)
	}
	found := false
	for _, entry := range hook.AllEntries() {
		found = found || (entry.Level == log.WarnLevel && strings.HasSuffix(entry.Message, ": timeout"))
	}
	if !found {
		t.Fatal("the warning should give timeout as the reason")
	}
}

func TestProberDiscardsRoundsCutShortByShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := New(Options{
		Targets: staticTargets("socks5://203.0.113.9:1080"),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			cancel()
			return nil, context.Canceled
		},
	})
	p.RunOnce(ctx)
	p.RunOnce(ctx)
	if got := p.Status(); got.Checked || got.Unreachable != 0 {
		t.Fatalf("cancelled connects must not count, got %+v", got)
	}
}

// TestProberConnectsForReal runs the default dialer against a listening and a
// closed local port.
func TestProberConnectsForReal(t *testing.T) {
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	go func() {
		for {
			conn, errAccept := open.Accept()
			if errAccept != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	p := New(Options{
		Targets: staticTargets("socks5://u:p@"+open.Addr().String(), "http://"+closedAddr),
		Timeout: time.Second,
	})
	p.RunOnce(context.Background())
	p.RunOnce(context.Background())
	if got := p.Status(); got != (Status{Checked: true, Total: 2, Unreachable: 1}) {
		t.Fatalf("status = %+v, want the closed port alone unreachable", got)
	}
}

func TestProberStartRunsAtOnceAndStopEnds(t *testing.T) {
	dialer := newFakeDialer()
	p := New(Options{
		Targets:  staticTargets("socks5://203.0.113.9:1080"),
		Interval: time.Hour,
		Dial:     dialer.dial,
	})
	p.Start(context.Background())
	p.Start(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for !p.Status().Checked {
		if time.Now().After(deadline) {
			t.Fatal("the first round should run right after Start, not after an interval")
		}
		time.Sleep(5 * time.Millisecond)
	}

	stopped := make(chan struct{})
	go func() {
		p.Stop()
		p.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	// A stopped prober stays stopped.
	calls := dialer.callCount()
	p.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	if dialer.callCount() != calls {
		t.Fatal("Start after Stop must not run rounds")
	}

	var nilProber *Prober
	nilProber.Start(context.Background())
	nilProber.RunOnce(context.Background())
	nilProber.Stop()
	if got := nilProber.Status(); got != (Status{}) {
		t.Fatalf("nil prober status = %+v", got)
	}
}
