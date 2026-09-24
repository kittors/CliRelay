package sharedredis

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

var incrScript = redis.NewScript(`return redis.call('INCR', KEYS[1])`)

func fastOptions() Options {
	return Options{
		NodeID:         "test-node",
		OpTimeout:      200 * time.Millisecond,
		HealthInterval: 50 * time.Millisecond,
		MinBackoff:     20 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
		StableAfter:    time.Millisecond,
	}
}

func newStartedClient(t *testing.T, addr string, opts Options) *Client {
	t.Helper()
	c, err := New(config.ClusterRedisConfig{Addr: addr}, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Start()
	t.Cleanup(c.Close)
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestClientAvailableAfterStart(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newStartedClient(t, mr.Addr(), fastOptions())
	if !c.Available() {
		t.Fatalf("client must be available right after a successful start probe: %+v", c.Status())
	}
	res, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
	if err != nil || res.(int64) != 1 {
		t.Fatalf("Eval = %v, %v", res, err)
	}
	if got, _ := mr.Get(KeyPrefix + "health:test-node"); got == "" {
		t.Fatalf("probe must write the node health key")
	}
}

func TestClientUnreachableAtStartStaysUnavailableWithoutBlocking(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there any more

	c := newStartedClient(t, addr, fastOptions())
	if c.Available() {
		t.Fatalf("unreachable server must start unavailable")
	}
	start := time.Now()
	_, err = c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Eval while down = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("Eval while down must not touch the network, took %s", elapsed)
	}
}

func TestClientRecoversAfterRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newStartedClient(t, mr.Addr(), fastOptions())
	if !c.Available() {
		t.Fatal("expected available")
	}

	mr.Close()
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); err == nil {
		t.Fatal("Eval against a closed server must fail")
	}
	if c.Available() {
		t.Fatal("a connectivity failure must mark the client unavailable at once")
	}
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("later calls must short-circuit, got %v", err)
	}

	if err := mr.Restart(); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
	waitFor(t, "recovery", c.Available)
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); err != nil {
		t.Fatalf("Eval after recovery: %v", err)
	}
}

func TestClientScriptErrorDoesNotMarkDown(t *testing.T) {
	mr := miniredis.RunT(t)
	c := newStartedClient(t, mr.Addr(), fastOptions())
	bad := redis.NewScript(`return redis.call('HGET', KEYS[1])`)
	if _, err := c.Eval(context.Background(), bad, []string{KeyPrefix + "t"}); err == nil {
		t.Fatal("malformed command must fail")
	}
	if !c.Available() {
		t.Fatal("a command the server rejected must not mark the server unavailable")
	}
}

// TestClientSlowServerIsBoundedByOpTimeout pins the requirement that a slow
// cluster Redis can delay a request by at most the op timeout, and only until
// the client has noticed: after that, requests skip Redis entirely.
func TestClientSlowServerIsBoundedByOpTimeout(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newBlackholeProxy(t, mr.Addr())
	c := newStartedClient(t, proxy.addr(), fastOptions())
	if !c.Available() {
		t.Fatal("expected available through the proxy")
	}

	proxy.blackhole(true)
	start := time.Now()
	_, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Eval through a stalled link must fail")
	}
	if elapsed > c.OpTimeout()+150*time.Millisecond {
		t.Fatalf("stalled Eval took %s, want about the op timeout %s", elapsed, c.OpTimeout())
	}
	if c.Available() {
		t.Fatal("a timed-out command must mark the client unavailable")
	}
	start = time.Now()
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "t"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second Eval = %v, want ErrUnavailable", err)
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("second Eval must not wait")
	}

	proxy.blackhole(false)
	waitFor(t, "recovery after the link heals", c.Available)
}

func TestIsConnectivityError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{redis.Nil, false},
		{context.Canceled, false},
		{context.DeadlineExceeded, true},
		{io.EOF, true},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{redis.ErrPoolTimeout, true},
	}
	for _, tc := range cases {
		if got := IsConnectivityError(tc.err); got != tc.want {
			t.Errorf("IsConnectivityError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestIsConnectivityErrorServerReplies(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	ctx := context.Background()

	_ = rdb.Set(ctx, "s", "v", 0).Err()
	_, wrongType := rdb.HGet(ctx, "s", "y").Result()
	if wrongType == nil || IsConnectivityError(wrongType) {
		t.Fatalf("WRONGTYPE is a per-command error, got %v", wrongType)
	}

	mr.SetError("LOADING Redis is loading the dataset in memory")
	_, loading := rdb.Get(ctx, "s").Result()
	if loading == nil || !IsConnectivityError(loading) {
		t.Fatalf("LOADING means the server is unusable, got %v", loading)
	}
}

func TestHashPartIsStableAndOpaque(t *testing.T) {
	a := HashPart("sk-XXXXsecret")
	if a != HashPart("sk-XXXXsecret") || len(a) != 32 {
		t.Fatalf("HashPart must be stable and 32 hex chars, got %q", a)
	}
	if HashPart("a", "b") == HashPart("ab") {
		t.Fatal("HashPart must separate parts")
	}
}

func TestNilClientIsUnavailable(t *testing.T) {
	var c *Client
	if c.Available() {
		t.Fatal("nil client must be unavailable")
	}
	if _, err := c.Eval(context.Background(), incrScript, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil client Eval = %v", err)
	}
	c.Start()
	c.Close()
	c.Observe(errors.New("x"))
}

// blackholeProxy forwards TCP to a backend until blackhole(true), after which
// it keeps connections open but stops moving bytes in either direction: the
// behaviour of a congested or partitioned public link.
type blackholeProxy struct {
	ln      net.Listener
	backend string
	stalled atomic.Bool
	closed  atomic.Bool
	mu      sync.Mutex
	conns   []net.Conn
}

func newBlackholeProxy(t *testing.T, backend string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &blackholeProxy{ln: ln, backend: backend}
	go p.serve()
	t.Cleanup(func() {
		p.closed.Store(true)
		_ = ln.Close()
		p.mu.Lock()
		for _, conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
	})
	return p
}

func (p *blackholeProxy) addr() string { return p.ln.Addr().String() }

func (p *blackholeProxy) blackhole(on bool) {
	p.stalled.Store(on)
	if on {
		return
	}
	// Healing drops the stalled connections so the client redials cleanly.
	p.mu.Lock()
	for _, conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = nil
	p.mu.Unlock()
}

func (p *blackholeProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.backend)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go p.pipe(client, server)
		go p.pipe(server, client)
	}
}

func (p *blackholeProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			_ = dst.Close()
			return
		}
		for p.stalled.Load() {
			if p.closed.Load() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}
